package readest

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"bookroom/internal/api"
	"bookroom/internal/apperr"
	"bookroom/internal/store"
	"bookroom/internal/store/db"
)

// Limits from Readest's libs/replicaSchemas.ts and libs/replicaSyncServer.ts.
const (
	maxPushBatch     = 100
	maxPullBatch     = 50
	maxFieldCount    = 64
	maxFieldsBytes   = 64 * 1024
	maxFilenameLen   = 255
	hlcSkewTolerance = 60 * time.Second
	pullPageSize     = 1000
	replicaKeyAlg    = "pbkdf2-600k-sha256"
)

// replicaKinds is the server allowlist; every kind is at schema version 1 in Readest v0.12.20.
var replicaKinds = map[string]bool{
	"bookshelf": true, "dictionary": true, "font": true, "texture": true, "opds_catalog": true, "abs_server": true,
	"custom_translator": true, "translation_prompt": true, "custom_tts": true, "settings": true,
}

// Replicas implements /api/sync/replicas and /api/sync/replica-keys.
type Replicas struct {
	st  *store.Store
	now func() time.Time
}

// NewReplicas builds the replica service.
func NewReplicas(st *store.Store) *Replicas { return &Replicas{st: st, now: time.Now} }

func replicaErr(status int, code, msg string, idx *int) *apperr.Error {
	return apperr.WithBody(status, api.ReplicaError{Error: msg, Code: api.ReplicaErrorCode(code), OffendingIndex: idx})
}

// hlcPhysicalMs reads the 13-hex-digit physical part of an HLC.
func hlcPhysicalMs(h string) (int64, bool) {
	i := strings.IndexByte(h, '-')
	if i <= 0 || strings.IndexByte(h[i+1:], '-') < 0 {
		return 0, false
	}
	v, err := strconv.ParseInt(h[:i], 16, 64)
	return v, err == nil
}

func hlcMax(a, b *string) *string {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case *a >= *b:
		return a
	}
	return b
}

type envelope map[string]json.RawMessage

func (e envelope) str(k string) (string, bool) {
	raw, ok := e[k]
	if !ok {
		return "", false
	}
	var s string
	return s, json.Unmarshal(raw, &s) == nil
}

// validEnvelope accepts {v, t, s} field envelopes and {c, i, s, alg, h} cipher envelopes.
func validEnvelope(raw json.RawMessage) bool {
	var e envelope
	if json.Unmarshal(raw, &e) != nil || e == nil {
		return false
	}
	if _, ok := e["v"]; ok {
		_, t := e.str("t")
		_, s := e.str("s")
		return t && s
	}
	for _, k := range []string{"c", "i", "s", "alg", "h"} {
		if _, ok := e.str(k); !ok {
			return false
		}
	}
	return true
}

func validateRow(row *api.ReplicaRow) (string, string) {
	if row.SchemaVersion < 1 || row.SchemaVersion > 1 {
		return "SCHEMA_TOO_NEW", fmt.Sprintf("schemaVersion %d out of bounds [1, 1] for kind %s", row.SchemaVersion, row.Kind)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(row.FieldsJsonb, &fields) != nil || fields == nil {
		return "VALIDATION", "malformed field envelope"
	}
	if len(fields) > maxFieldCount {
		return "VALIDATION", fmt.Sprintf("field count %d exceeds MAX_FIELD_COUNT=%d", len(fields), maxFieldCount)
	}
	if len(row.FieldsJsonb) > maxFieldsBytes {
		return "VALIDATION", fmt.Sprintf("fields_jsonb size %dB exceeds MAX_JSON_BYTES=%d", len(row.FieldsJsonb), maxFieldsBytes)
	}
	for _, raw := range fields {
		if !validEnvelope(raw) {
			return "VALIDATION", "malformed field envelope"
		}
	}
	if !isAbsentOrNull(row.ManifestJsonb) {
		var m struct {
			Files []struct {
				Filename   *string  `json:"filename"`
				ByteSize   *float64 `json:"byteSize"`
				PartialMd5 *string  `json:"partialMd5"`
			} `json:"files"`
			SchemaVersion *float64 `json:"schemaVersion"`
		}
		if json.Unmarshal(row.ManifestJsonb, &m) != nil || m.Files == nil || m.SchemaVersion == nil {
			return "VALIDATION", "malformed manifest_jsonb"
		}
		for _, f := range m.Files {
			if f.Filename == nil || f.ByteSize == nil || f.PartialMd5 == nil {
				return "VALIDATION", "malformed manifest_jsonb"
			}
			if reason := filenameProblem(*f.Filename); reason != "" {
				return "VALIDATION", fmt.Sprintf("manifest filename invalid: %s (%s)", *f.Filename, reason)
			}
		}
	}
	return "", ""
}

func filenameProblem(name string) string {
	switch {
	case name == "":
		return "empty"
	case len([]rune(name)) > maxFilenameLen:
		return "too long"
	case name == "." || name == "..":
		return "dot path"
	case strings.ContainsAny(name, `/\`):
		return "path separator"
	case strings.Contains(name, ".."):
		return "path traversal"
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "control character"
		}
	}
	return ""
}

// Push validates and merges replica rows (crdt_merge_replica, Readest migration 005).
func (r *Replicas) Push(ctx context.Context, userID string, rows []api.ReplicaRow) ([]api.ReplicaRow, error) {
	if len(rows) > maxPushBatch {
		return nil, replicaErr(http.StatusRequestEntityTooLarge, "VALIDATION", fmt.Sprintf("batch size %d exceeds MAX_PUSH_BATCH=%d", len(rows), maxPushBatch), nil)
	}
	nowMs := r.now().UnixMilli()
	skewOK := func(h string) bool {
		p, ok := hlcPhysicalMs(h)
		return ok && p >= nowMs-hlcSkewTolerance.Milliseconds() && p <= nowMs+hlcSkewTolerance.Milliseconds()
	}
	for i := range rows {
		row := &rows[i]
		idx := new(i)
		if row.UserId != userID {
			return nil, replicaErr(http.StatusForbidden, "AUTH", fmt.Sprintf("row[%d].user_id does not match authenticated user", i), idx)
		}
		if !replicaKinds[row.Kind] {
			return nil, replicaErr(http.StatusUnprocessableEntity, "UNKNOWN_KIND", fmt.Sprintf("row[%d].kind=%s is not in the server allowlist", i, row.Kind), idx)
		}
		if code, msg := validateRow(row); code != "" {
			return nil, replicaErr(http.StatusUnprocessableEntity, code, fmt.Sprintf("row[%d] %s", i, msg), idx)
		}
		if !skewOK(row.UpdatedAtTs) {
			return nil, replicaErr(http.StatusConflict, "CLOCK_SKEW", fmt.Sprintf("row[%d].updated_at_ts physical time is outside ±%dms of server", i, hlcSkewTolerance.Milliseconds()), idx)
		}
		if row.DeletedAtTs != nil && *row.DeletedAtTs != "" && !skewOK(*row.DeletedAtTs) {
			return nil, replicaErr(http.StatusConflict, "CLOCK_SKEW", fmt.Sprintf("row[%d].deleted_at_ts physical time is outside ±%dms of server", i, hlcSkewTolerance.Milliseconds()), idx)
		}
	}
	out := make([]api.ReplicaRow, 0, len(rows))
	err := r.st.Tx(ctx, func(q *db.Queries) error {
		for i := range rows {
			merged, err := mergeReplica(ctx, q, &rows[i], nowMs)
			if err != nil {
				return err
			}
			out = append(out, replicaRow(merged))
		}
		return nil
	})
	return out, err
}

func mergeReplica(ctx context.Context, q *db.Queries, in *api.ReplicaRow, nowMs int64) (*db.Replica, error) {
	incoming := db.Replica{
		UserID: in.UserId, Kind: in.Kind, ReplicaID: in.ReplicaId, FieldsJsonb: "{}", ManifestJsonb: rawJSON(in.ManifestJsonb),
		DeletedAtTs: nonEmptyPtr(in.DeletedAtTs), Reincarnation: in.Reincarnation, UpdatedAtTs: in.UpdatedAtTs,
		SchemaVersion: int64(in.SchemaVersion), CreatedAt: nowMs, ModifiedAt: nowMs,
	}
	if f := rawJSON(in.FieldsJsonb); f != nil {
		incoming.FieldsJsonb = *f
	}
	stored, err := q.GetReplica(ctx, db.GetReplicaParams{UserID: in.UserId, Kind: in.Kind, ReplicaID: in.ReplicaId})
	row := &incoming
	if err == nil {
		row = mergeReplicaRows(&stored, &incoming, nowMs)
	} else if !store.IsNotFound(err) {
		return nil, err
	}
	return row, q.UpsertReplica(ctx, db.UpsertReplicaParams{
		UserID: row.UserID, Kind: row.Kind, ReplicaID: row.ReplicaID, FieldsJsonb: row.FieldsJsonb,
		ManifestJsonb: row.ManifestJsonb, DeletedAtTs: row.DeletedAtTs, Reincarnation: row.Reincarnation,
		UpdatedAtTs: row.UpdatedAtTs, SchemaVersion: row.SchemaVersion, CreatedAt: row.CreatedAt, ModifiedAt: row.ModifiedAt,
	})
}

// mergeReplicaRows is the ON CONFLICT branch of crdt_merge_replica.
func mergeReplicaRows(r, ex *db.Replica, nowMs int64) *db.Replica {
	out := *r
	fields := mergeFields(r.FieldsJsonb, ex.FieldsJsonb)
	out.FieldsJsonb = fields
	deleted := hlcMax(r.DeletedAtTs, ex.DeletedAtTs)
	out.DeletedAtTs = deleted
	alive := func(updated string) bool { return deleted == nil || updated > *deleted }
	switch {
	case r.Reincarnation == nil && ex.Reincarnation == nil:
		out.Reincarnation = nil
	case r.Reincarnation != nil && ex.Reincarnation == nil:
		out.Reincarnation = keepIf(r.Reincarnation, alive(r.UpdatedAtTs))
	case r.Reincarnation == nil:
		out.Reincarnation = keepIf(ex.Reincarnation, alive(ex.UpdatedAtTs))
	case ex.UpdatedAtTs > r.UpdatedAtTs:
		out.Reincarnation = keepIf(ex.Reincarnation, alive(ex.UpdatedAtTs))
	default:
		out.Reincarnation = keepIf(r.Reincarnation, alive(r.UpdatedAtTs))
	}
	switch {
	case ex.ManifestJsonb == nil:
	case r.ManifestJsonb == nil, ex.UpdatedAtTs > r.UpdatedAtTs:
		out.ManifestJsonb = ex.ManifestJsonb
	}
	out.SchemaVersion = max(r.SchemaVersion, ex.SchemaVersion)
	updated := hlcMax(&r.UpdatedAtTs, &ex.UpdatedAtTs)
	out.UpdatedAtTs = *hlcMax(updated, new(computeUpdatedAt(fields, deleted)))
	out.ModifiedAt = nowMs
	return &out
}

func keepIf(v *string, ok bool) *string {
	if ok {
		return v
	}
	return nil
}

// mergeFields is crdt_merge_fields: per field the envelope with the greater HLC `t` wins, ties broken by the
// greater device id `s`; envelopes without `t` never replace an existing one.
func mergeFields(local, remote string) string {
	var l, r map[string]json.RawMessage
	if json.Unmarshal([]byte(local), &l) != nil || l == nil {
		l = map[string]json.RawMessage{}
	}
	if json.Unmarshal([]byte(remote), &r) != nil {
		return marshalFields(l)
	}
	for k, rEnv := range r {
		lEnv, ok := l[k]
		if !ok {
			l[k] = rEnv
			continue
		}
		var le, re envelope
		_ = json.Unmarshal(lEnv, &le)
		_ = json.Unmarshal(rEnv, &re)
		lt, lok := le.str("t")
		rt, rok := re.str("t")
		if !lok || !rok {
			continue
		}
		if rt > lt {
			l[k] = rEnv
		} else if rt == lt {
			ls, _ := le.str("s")
			rs, _ := re.str("s")
			if rs > ls {
				l[k] = rEnv
			}
		}
	}
	return marshalFields(l)
}

func marshalFields(m map[string]json.RawMessage) string {
	b, _ := json.Marshal(m)
	return string(b)
}

// computeUpdatedAt is crdt_compute_updated_at: the greatest field HLC, at least the deletion HLC.
func computeUpdatedAt(fields string, deleted *string) string {
	result := "0000000000000-00000000-"
	if deleted != nil {
		result = *deleted
	}
	var m map[string]envelope
	_ = json.Unmarshal([]byte(fields), &m)
	for _, e := range m {
		if t, ok := e.str("t"); ok && t > result {
			result = t
		}
	}
	return result
}

func replicaRow(r *db.Replica) api.ReplicaRow {
	out := api.ReplicaRow{
		UserId: r.UserID, Kind: r.Kind, ReplicaId: r.ReplicaID, FieldsJsonb: json.RawMessage(r.FieldsJsonb),
		ManifestJsonb: json.RawMessage("null"), DeletedAtTs: r.DeletedAtTs, Reincarnation: r.Reincarnation,
		UpdatedAtTs: r.UpdatedAtTs, SchemaVersion: int(r.SchemaVersion),
		CreatedAt: isoMillis(&r.CreatedAt), ModifiedAt: isoMillis(&r.ModifiedAt),
	}
	if r.ManifestJsonb != nil {
		out.ManifestJsonb = json.RawMessage(*r.ManifestJsonb)
	}
	return out
}

// Pull returns up to 1000 rows of one kind after an HLC cursor.
func (r *Replicas) Pull(ctx context.Context, userID, kind string, since *string) ([]api.ReplicaRow, error) {
	if kind == "" {
		return nil, replicaErr(http.StatusBadRequest, "VALIDATION", "kind query parameter required", nil)
	}
	if !replicaKinds[kind] {
		return nil, replicaErr(http.StatusUnprocessableEntity, "UNKNOWN_KIND", fmt.Sprintf("kind=%s is not in the server allowlist", kind), nil)
	}
	var cursor any
	if since != nil && *since != "" {
		cursor = *since
	}
	rows, err := r.st.Q.PullReplicas(ctx, db.PullReplicasParams{UserID: userID, Kind: kind, Since: cursor})
	if err != nil {
		return nil, err
	}
	out := make([]api.ReplicaRow, len(rows))
	for i := range rows {
		out[i] = replicaRow(&rows[i])
	}
	return out, nil
}

// PullBatch serves a `{cursors: [...]}` body.
func (r *Replicas) PullBatch(ctx context.Context, userID string, cursors []api.ReplicaCursor) ([]api.ReplicaPullResult, error) {
	if len(cursors) > maxPullBatch {
		return nil, replicaErr(http.StatusRequestEntityTooLarge, "VALIDATION", fmt.Sprintf("cursors.length %d exceeds MAX_PULL_BATCH=%d", len(cursors), maxPullBatch), nil)
	}
	seen := map[string]bool{}
	for i, c := range cursors {
		idx := new(i)
		switch {
		case c.Kind == "":
			return nil, replicaErr(http.StatusBadRequest, "VALIDATION", fmt.Sprintf("cursors[%d].kind must be a non-empty string", i), idx)
		case !replicaKinds[c.Kind]:
			return nil, replicaErr(http.StatusUnprocessableEntity, "UNKNOWN_KIND", fmt.Sprintf("cursors[%d].kind=%s is not in the server allowlist", i, c.Kind), idx)
		case seen[c.Kind]:
			return nil, replicaErr(http.StatusBadRequest, "VALIDATION", fmt.Sprintf("cursors[%d].kind=%s is duplicated in the batch", i, c.Kind), idx)
		}
		seen[c.Kind] = true
	}
	out := make([]api.ReplicaPullResult, 0, len(cursors))
	for _, c := range cursors {
		rows, err := r.Pull(ctx, userID, c.Kind, c.Since)
		if err != nil {
			return nil, err
		}
		out = append(out, api.ReplicaPullResult{Kind: c.Kind, Rows: rows})
	}
	return out, nil
}

// ListKeys lists encryption salts.
func (r *Replicas) ListKeys(ctx context.Context, userID string) ([]api.ReplicaKey, error) {
	rows, err := r.st.Q.ListReplicaKeys(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]api.ReplicaKey, len(rows))
	for i, k := range rows {
		out[i] = api.ReplicaKey{SaltId: k.SaltID, Alg: k.Alg, Salt: base64.StdEncoding.EncodeToString(k.Salt), CreatedAt: *isoMillis(&k.CreatedAt)}
	}
	return out, nil
}

// CreateKey creates a 32-byte salt.
func (r *Replicas) CreateKey(ctx context.Context, userID, alg string) (api.ReplicaKey, error) {
	if alg != replicaKeyAlg {
		return api.ReplicaKey{}, replicaErr(http.StatusUnprocessableEntity, "UNSUPPORTED_ALG", "Unsupported alg: "+alg, nil)
	}
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return api.ReplicaKey{}, err
	}
	now := r.now().UnixMilli()
	k := db.ReplicaKey{UserID: userID, SaltID: uuid.NewString(), Alg: alg, Salt: salt, CreatedAt: now}
	if err := r.st.WQ.CreateReplicaKey(ctx, db.CreateReplicaKeyParams(k)); err != nil {
		return api.ReplicaKey{}, err
	}
	return api.ReplicaKey{SaltId: k.SaltID, Alg: alg, Salt: base64.StdEncoding.EncodeToString(salt), CreatedAt: *isoMillis(&now)}, nil
}

// ForgetKeys drops every salt and every field whose value is an encrypted envelope (replica_keys_forget).
func (r *Replicas) ForgetKeys(ctx context.Context, userID string) error {
	now := r.now().UnixMilli()
	return r.st.Tx(ctx, func(q *db.Queries) error {
		rows, err := q.ListUserReplicas(ctx, userID)
		if err != nil {
			return err
		}
		for _, row := range rows {
			var fields map[string]envelope
			if json.Unmarshal([]byte(row.FieldsJsonb), &fields) != nil {
				continue
			}
			kept := map[string]json.RawMessage{}
			dropped := false
			for k, e := range fields {
				var v map[string]json.RawMessage
				if json.Unmarshal(e["v"], &v) == nil && v != nil {
					if _, enc := v["alg"]; enc {
						dropped = true
						continue
					}
				}
				b, _ := json.Marshal(e)
				kept[k] = b
			}
			if dropped {
				if err := q.SetReplicaFields(ctx, db.SetReplicaFieldsParams{FieldsJsonb: marshalFields(kept), ModifiedAt: now,
					UserID: userID, Kind: row.Kind, ReplicaID: row.ReplicaID}); err != nil {
					return err
				}
			}
		}
		return q.DeleteReplicaKeys(ctx, userID)
	})
}
