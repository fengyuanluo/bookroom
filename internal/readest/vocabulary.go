package readest

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"bookroom/internal/api"
	"bookroom/internal/store"
	"bookroom/internal/store/db"
)

// Limits from Readest's services/vocabulary/wire.ts and entry.ts (v0.12.20).
const (
	vocabMaxPushRows    = 100
	vocabMaxPullRows    = 1000
	vocabMaxRowBytes    = 64 * 1024
	vocabMaxOccurrences = 20
)

// vocabCursorFormat renders `updated_at` the way JavaScript's toISOString does, so the cursor a client echoes
// back parses to the same millisecond.
const vocabCursorFormat = "2006-01-02T15:04:05.000Z07:00"

// Vocabulary implements /api/sync/vocabulary: one row per looked-up word, merged with Readest's
// `mergeEntries` and paged on the server-stamped (updated_at, id) keyset.
type Vocabulary struct {
	st  *store.Store
	now func() time.Time
}

// NewVocabulary builds the vocabulary sync service.
func NewVocabulary(st *store.Store) *Vocabulary { return &Vocabulary{st: st, now: time.Now} }

type vocabOccurrence struct {
	raw      json.RawMessage
	sentence string
	bookHash string
	at       float64
}

// vocabEntry mirrors Readest's VocabularyEntry. An empty hint is no hint and a zero deletedAt is alive, as the
// JavaScript truthiness checks treat them; deletedAt keeps nil apart from 0 only to echo rows unchanged.
type vocabEntry struct {
	id, lang, key, word, state, hint string
	occurrences                      []vocabOccurrence
	createdAt, modifiedAt            int64
	deletedAt                        *int64
}

func (e vocabEntry) deleted() bool { return e.deletedAt != nil && *e.deletedAt != 0 }

// Pull returns the rows changed after (sinceTs, sinceID), oldest first.
func (v *Vocabulary) Pull(ctx context.Context, userID string, p api.ReadestPullVocabularyParams) ([]api.VocabularyRow, error) {
	since := int64(-1)
	if ts := deref(p.SinceTs); ts != "" {
		t, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			return nil, Fail(http.StatusBadRequest, "Invalid since_ts")
		}
		since = t.UnixMilli()
	}
	limit := int64(vocabMaxPullRows)
	if n, err := strconv.ParseFloat(deref(p.Limit), 64); err == nil && math.Floor(n) > 0 {
		limit = min(int64(math.Floor(n)), vocabMaxPullRows)
	}
	rows, err := v.st.Q.PullVocabulary(ctx, db.PullVocabularyParams{
		UserID: userID, SinceTs: since, SinceID: deref(p.SinceId), PageSize: limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]api.VocabularyRow, 0, len(rows))
	for _, r := range rows {
		e, err := entryFromStored(r)
		if err != nil {
			return nil, err
		}
		out = append(out, vocabRow(userID, e, r.UpdatedAt))
	}
	return out, nil
}

// Push merges each well-formed row with the stored one and returns the merged rows. A malformed row is skipped
// and reported rather than failing the batch, so one bad word can't block every later push from a device.
func (v *Vocabulary) Push(ctx context.Context, userID string, body api.VocabularyPushRequest) (api.VocabularyPushResponse, error) {
	res := api.VocabularyPushResponse{Rows: []api.VocabularyRow{}, Rejected: []string{}}
	if len(body.Rows) > vocabMaxPushRows {
		return res, Fail(http.StatusRequestEntityTooLarge, "at most "+strconv.Itoa(vocabMaxPushRows)+" rows")
	}
	var incoming []vocabEntry
	for _, raw := range body.Rows {
		e, ok := parseVocabRow(raw)
		if !ok {
			res.Rejected = append(res.Rejected, rejectedVocabID(raw))
			continue
		}
		incoming = append(incoming, e)
	}
	if len(incoming) == 0 {
		return res, nil
	}

	updatedAt := v.now().UnixMilli()
	err := v.st.Tx(ctx, func(q *db.Queries) error {
		ids := make([]string, 0, len(incoming))
		for _, e := range incoming {
			ids = append(ids, e.id)
		}
		stored, err := q.GetVocabulary(ctx, db.GetVocabularyParams{UserID: userID, Ids: ids})
		if err != nil {
			return err
		}
		prior := make(map[string]vocabEntry, len(stored))
		for _, r := range stored {
			if prior[r.ID], err = entryFromStored(r); err != nil {
				return err
			}
		}
		var order []string
		merged := make(map[string]vocabEntry, len(incoming))
		for _, e := range incoming {
			old, seen := merged[e.id]
			if !seen {
				old, seen = prior[e.id]
				order = append(order, e.id)
			}
			if seen {
				e = mergeVocab(old, e)
			}
			merged[e.id] = e
		}
		for _, id := range order {
			e := merged[id]
			if err := q.UpsertVocabulary(ctx, db.UpsertVocabularyParams{
				UserID: userID, ID: e.id, Lang: e.lang, WordKey: e.key, Word: e.word, State: e.state,
				Hint: nonEmptyPtr(&e.hint), Occurrences: string(occurrencesJSON(e.occurrences)),
				CreatedAt: e.createdAt, ModifiedAt: e.modifiedAt, DeletedAt: e.deletedAt, UpdatedAt: updatedAt,
			}); err != nil {
				return err
			}
			res.Rows = append(res.Rows, vocabRow(userID, e, updatedAt))
		}
		return nil
	})
	return res, err
}

func vocabRow(userID string, e vocabEntry, updatedAt int64) api.VocabularyRow {
	occurrences := make([]json.RawMessage, len(e.occurrences))
	for i, o := range e.occurrences {
		occurrences[i] = o.raw
	}
	return api.VocabularyRow{
		UserId: userID, Id: e.id, Lang: e.lang, WordKey: e.key, Word: e.word,
		State: api.VocabularyRowState(e.state), Hint: nonEmptyPtr(&e.hint), Occurrences: occurrences,
		CreatedAt: e.createdAt, ModifiedAt: e.modifiedAt, DeletedAt: e.deletedAt,
		UpdatedAt: time.UnixMilli(updatedAt).UTC().Format(vocabCursorFormat),
	}
}

func occurrencesJSON(list []vocabOccurrence) []byte {
	var b bytes.Buffer
	b.WriteByte('[')
	for i, o := range list {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(o.raw)
	}
	b.WriteByte(']')
	return b.Bytes()
}

func entryFromStored(r db.Vocabulary) (vocabEntry, error) {
	var raws []json.RawMessage
	if err := json.Unmarshal([]byte(r.Occurrences), &raws); err != nil {
		return vocabEntry{}, err
	}
	occurrences := make([]vocabOccurrence, 0, len(raws))
	for _, raw := range raws {
		o, _ := parseOccurrence(raw)
		occurrences = append(occurrences, o)
	}
	return vocabEntry{
		id: r.ID, lang: r.Lang, key: r.WordKey, word: r.Word, state: r.State, hint: deref(r.Hint),
		occurrences: occurrences, createdAt: r.CreatedAt, modifiedAt: r.ModifiedAt, deletedAt: r.DeletedAt,
	}, nil
}

func parseOccurrence(raw json.RawMessage) (vocabOccurrence, bool) {
	var o struct {
		Sentence *string  `json:"sentence"`
		BookHash any      `json:"bookHash"`
		At       *float64 `json:"at"`
	}
	if err := json.Unmarshal(raw, &o); err != nil || o.Sentence == nil || o.At == nil {
		return vocabOccurrence{raw: raw}, false
	}
	hash, _ := o.BookHash.(string)
	return vocabOccurrence{raw: raw, sentence: *o.Sentence, bookHash: hash, at: *o.At}, true
}

// parseVocabRow is Readest's validateRow followed by rowToEntry.
func parseVocabRow(raw json.RawMessage) (vocabEntry, bool) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil || utf16Len(compact.String()) > vocabMaxRowBytes {
		return vocabEntry{}, false
	}
	var r struct {
		ID          any               `json:"id"`
		Lang        any               `json:"lang"`
		WordKey     any               `json:"word_key"`
		Word        any               `json:"word"`
		State       any               `json:"state"`
		Hint        any               `json:"hint"`
		Occurrences []json.RawMessage `json:"occurrences"`
		CreatedAt   any               `json:"created_at"`
		ModifiedAt  any               `json:"modified_at"`
		DeletedAt   any               `json:"deleted_at"`
	}
	if !bytes.HasPrefix(compact.Bytes(), []byte("{")) || json.Unmarshal(compact.Bytes(), &r) != nil {
		return vocabEntry{}, false
	}
	lang, ok1 := r.Lang.(string)
	key, ok2 := r.WordKey.(string)
	word, ok3 := r.Word.(string)
	id, _ := r.ID.(string)
	state, _ := r.State.(string)
	if !ok1 || !ok2 || !ok3 || key == "" || id != lang+":"+key || strings.Contains(id, `"`) {
		return vocabEntry{}, false
	}
	if state != "none" && state != "learning" && state != "mastered" {
		return vocabEntry{}, false
	}
	hint, hintIsString := r.Hint.(string)
	if r.Hint != nil && !hintIsString {
		return vocabEntry{}, false
	}
	created, ok1 := r.CreatedAt.(float64)
	modified, ok2 := r.ModifiedAt.(float64)
	if !ok1 || !ok2 {
		return vocabEntry{}, false
	}
	var deletedAt *int64
	if r.DeletedAt != nil {
		d, ok := r.DeletedAt.(float64)
		if !ok {
			return vocabEntry{}, false
		}
		deletedAt = new(int64(d))
	}
	if r.Occurrences == nil || len(r.Occurrences) > vocabMaxOccurrences {
		return vocabEntry{}, false
	}
	occurrences := make([]vocabOccurrence, 0, len(r.Occurrences))
	for _, raw := range r.Occurrences {
		o, ok := parseOccurrence(raw)
		if !ok {
			return vocabEntry{}, false
		}
		occurrences = append(occurrences, o)
	}
	return vocabEntry{
		id: id, lang: lang, key: key, word: word, state: state, hint: hint, occurrences: occurrences,
		createdAt: int64(created), modifiedAt: int64(modified), deletedAt: deletedAt,
	}, true
}

// rejectedVocabID is the id Readest reports for a rejected row: `String(row?.id ?? "")`.
func rejectedVocabID(raw json.RawMessage) string {
	var r struct {
		ID any `json:"id"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return ""
	}
	switch id := r.ID.(type) {
	case string:
		return id
	case float64:
		return strconv.FormatFloat(id, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(id)
	}
	return ""
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// normalizeOccurrences keeps one occurrence per book and sentence, newest first, at most 20.
func normalizeOccurrences(list []vocabOccurrence) []vocabOccurrence {
	sorted := slices.Clone(list)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].at > sorted[j].at })
	seen := make(map[string]bool, len(sorted))
	out := make([]vocabOccurrence, 0, min(len(sorted), vocabMaxOccurrences))
	for _, o := range sorted {
		k := o.bookHash + "|" + o.sentence
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, o)
		if len(out) == vocabMaxOccurrences {
			break
		}
	}
	return out
}

// vocabTieKey breaks a same-millisecond tie the same way on every device and on the server, whatever the
// argument order: a delete first, then the larger state/hint, then the larger display word.
func vocabTieKey(e vocabEntry) string {
	d := "0"
	if e.deleted() {
		d = "1"
	}
	return d + "|" + e.state + "|" + e.hint + "|" + e.word
}

func vocabAWins(a, b vocabEntry) bool {
	if a.modifiedAt != b.modifiedAt {
		return a.modifiedAt > b.modifiedAt
	}
	return compareUTF16(vocabTieKey(a), vocabTieKey(b)) >= 0
}

// mergeVocab is Readest's mergeEntries: the side changed last wins the state, hint and tombstone; sentences are
// unioned. A delete drops every sentence from before it, so a word looked up again after a delete comes back
// with only the new sentences.
func mergeVocab(a, b vocabEntry) vocabEntry {
	winner, loser := b, a
	if vocabAWins(a, b) {
		winner, loser = a, b
	}
	out := winner
	if winner.deleted() {
		var after []vocabOccurrence
		for _, o := range loser.occurrences {
			if o.at > float64(*winner.deletedAt) {
				after = append(after, o)
			}
		}
		if len(after) == 0 {
			out.occurrences = []vocabOccurrence{}
			return out
		}
		out.hint = ""
		out.state = "none"
		out.deletedAt = nil
		out.occurrences = normalizeOccurrences(after)
		return out
	}
	var cutoff float64
	if loser.deletedAt != nil {
		cutoff = float64(*loser.deletedAt)
	}
	var union []vocabOccurrence
	for _, o := range append(slices.Clone(winner.occurrences), loser.occurrences...) {
		if o.at > cutoff {
			union = append(union, o)
		}
	}
	out.createdAt = min(a.createdAt, b.createdAt)
	out.deletedAt = nil
	out.occurrences = normalizeOccurrences(union)
	return out
}

// compareUTF16 orders strings by UTF-16 code units, as JavaScript's `<` does.
func compareUTF16(a, b string) int {
	return slices.Compare(utf16.Encode([]rune(a)), utf16.Encode([]rune(b)))
}

// utf16Len is JavaScript's String.prototype.length.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}
