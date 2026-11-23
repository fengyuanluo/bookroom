package readest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"bookroom/internal/api"
	"bookroom/internal/auth"
	"bookroom/internal/library"
	"bookroom/internal/store"
	"bookroom/internal/store/db"
)

// signedURLTTL is how long upload and download URLs stay valid (Readest uses 1800 s).
const signedURLTTL = 30 * time.Minute

// unlimitedQuota is reported as the storage quota; BookRoom does not meter storage.
const unlimitedQuota = int64(1) << 50

// Storage implements /api/storage. Book files resolve to the shared library; every other object (fonts,
// textures, replica files) is stored per user under DATA_DIR/objects.
type Storage struct {
	st     *store.Store
	lib    *library.Service
	objDir string
	secret []byte
}

// NewStorage builds the storage service.
func NewStorage(st *store.Store, lib *library.Service, dataDir, secret string) (*Storage, error) {
	s := &Storage{st: st, lib: lib, objDir: filepath.Join(dataDir, "objects"), secret: []byte("storage:" + secret)}
	return s, os.MkdirAll(s.objDir, 0o750)
}

// objectKind classifies a file name below the user prefix.
type objectKind int

const (
	kindObject objectKind = iota // per-user object
	kindBook                     // Readest/Books/<hash>/<name>.<book ext>: the library file
	kindCover                    // Readest/Books/<hash>/cover.png: the library cover
)

// classify parses `Readest/Books/<hash>/<name>.<ext>` (utils/bookFileKey.ts).
func classify(fileName string) (objectKind, string) {
	parts := strings.Split(fileName, "/")
	if len(parts) != 4 || parts[0] != "Readest" || parts[1] != "Books" || !library.IsHash(parts[2]) {
		return kindObject, ""
	}
	if parts[3] == "cover.png" {
		return kindCover, parts[2]
	}
	if _, ok := library.FormatOf(parts[3]); ok {
		return kindBook, parts[2]
	}
	return kindObject, ""
}

// SafeObjectKeyName ports isSafeObjectKeyName: no backslash, NUL, leading slash, or empty/dot segments, in
// the raw and the percent-decoded form.
func SafeObjectKeyName(name string) bool {
	if name == "" {
		return false
	}
	forms := []string{name}
	decoded, err := url.PathUnescape(name)
	if err != nil {
		return false
	}
	if decoded != name {
		forms = append(forms, decoded)
	}
	for _, f := range forms {
		if strings.ContainsAny(f, "\\\x00") || strings.HasPrefix(f, "/") {
			return false
		}
		for _, seg := range strings.Split(f, "/") {
			if seg == "" || seg == "." || seg == ".." {
				return false
			}
		}
	}
	return true
}

type grant struct {
	jwt.RegisteredClaims
	Key  string `json:"key"`
	Op   string `json:"op"` // put | get
	Size int64  `json:"size,omitempty"`
}

func (s *Storage) sign(userID, key, op string, size int64, ttl time.Duration) (string, error) {
	now := time.Now()
	return jwt.NewWithClaims(jwt.SigningMethodHS256, grant{
		RegisteredClaims: jwt.RegisteredClaims{Subject: userID, IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(ttl))},
		Key:              key, Op: op, Size: size,
	}).SignedString(s.secret)
}

func (s *Storage) verify(token, op string) (*grant, error) {
	var g grant
	_, err := jwt.ParseWithClaims(token, &g, func(*jwt.Token) (any, error) { return s.secret, nil },
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
	if err != nil || g.Op != op || g.Subject == "" {
		return nil, Fail(http.StatusForbidden, "Invalid or expired signature")
	}
	return &g, nil
}

func objectURL(baseURL, token string) string { return baseURL + "/api/storage/objects/" + token }

// UploadURL issues a signed PUT URL.
func (s *Storage) UploadURL(ctx context.Context, user auth.User, req api.UploadUrlRequest, baseURL string) (api.UploadUrlResponse, error) {
	if !SafeObjectKeyName(req.FileName) {
		return api.UploadUrlResponse{}, Fail(http.StatusBadRequest, "Invalid fileName")
	}
	if req.FileSize <= 0 || req.FileSize > math.MaxInt64/2 {
		return api.UploadUrlResponse{}, Fail(http.StatusBadRequest, "Invalid fileSize")
	}
	fileName := req.FileName
	if req.Temp != nil && *req.Temp || req.Media != nil && *req.Media != "" {
		fileName = "temp/" + req.FileName // share images: private here, signed download for 3 days
	}
	fileKey := user.ID + "/" + fileName
	kind, hash := classify(fileName)
	now := time.Now().UnixMilli()
	if kind == kindObject {
		if err := s.st.WQ.ReserveFile(ctx, db.ReserveFileParams{
			UserID: user.ID, FileKey: fileKey, BookHash: nonEmptyPtr(req.BookHash), ReplicaKind: nonEmptyPtr(req.ReplicaKind),
			ReplicaID: nonEmptyPtr(req.ReplicaId), FileSize: req.FileSize, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			return api.UploadUrlResponse{}, err
		}
	} else if req.BookHash != nil && *req.BookHash != "" && *req.BookHash != hash {
		return api.UploadUrlResponse{}, Fail(http.StatusBadRequest, "bookHash does not match fileName")
	}
	token, err := s.sign(user.ID, fileKey, "put", req.FileSize, signedURLTTL)
	if err != nil {
		return api.UploadUrlResponse{}, err
	}
	usage, err := s.st.Q.StorageUsage(ctx, user.ID)
	if err != nil {
		return api.UploadUrlResponse{}, err
	}
	res := api.UploadUrlResponse{UploadUrl: objectURL(baseURL, token), FileKey: &fileKey, Usage: &usage, Quota: new(unlimitedQuota)}
	if fileName != req.FileName {
		dl, err := s.sign(user.ID, fileKey, "get", 0, 72*time.Hour)
		if err != nil {
			return res, err
		}
		res.DownloadUrl = new(objectURL(baseURL, dl))
	}
	return res, nil
}

func nonEmptyPtr(s *string) *string {
	if s == nil || *s == "" {
		return nil
	}
	return s
}

// Put receives the bytes of a signed upload.
func (s *Storage) Put(ctx context.Context, token string, body io.Reader) error {
	g, err := s.verify(token, "put")
	if err != nil {
		return err
	}
	userID, fileName, _ := strings.Cut(g.Key, "/")
	switch kind, hash := classify(fileName); kind {
	case kindBook:
		if ok, err := s.lib.HasBook(ctx, hash); err != nil {
			return err
		} else if ok {
			_, err := io.Copy(io.Discard, body) // already in the library; the client still sends the bytes
			return err
		}
		_, _, err := s.lib.IngestReader(ctx, body, path.Base(fileName), library.IngestOptions{
			AddedBy: userID, ExpectHash: hash, SkipProjection: userID,
		})
		return err
	case kindCover:
		data, err := io.ReadAll(io.LimitReader(body, 10<<20+1))
		if err != nil {
			return err
		}
		if ok, err := s.lib.HasBook(ctx, hash); err != nil {
			return err
		} else if ok {
			_, err := s.lib.SetCoverIfMissing(ctx, hash, data)
			return err
		}
		return s.storeObject(ctx, userID, g.Key, bytes.NewReader(data), &hash)
	default:
		return s.storeObject(ctx, userID, g.Key, body, nil)
	}
}

func (s *Storage) objectPath(userID, key string) string {
	sum := sha256.Sum256([]byte(key))
	h := hex.EncodeToString(sum[:])
	return filepath.Join(s.objDir, userID, h[:2], h)
}

func (s *Storage) storeObject(ctx context.Context, userID, key string, body io.Reader, bookHash *string) error {
	p := s.objectPath(userID, key)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}
	tmp, err := s.lib.Spool(body)
	if err != nil {
		return err
	}
	st, err := os.Stat(tmp)
	if err == nil {
		err = os.Rename(tmp, p)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	now := time.Now().UnixMilli()
	if err := s.st.WQ.ReserveFile(ctx, db.ReserveFileParams{UserID: userID, FileKey: key, BookHash: bookHash,
		FileSize: st.Size(), CreatedAt: now, UpdatedAt: now}); err != nil {
		return err
	}
	return s.st.WQ.MarkFileStored(ctx, db.MarkFileStoredParams{FileSize: st.Size(), UpdatedAt: now, UserID: userID, FileKey: key})
}

// Object resolves the target of a signed download.
func (s *Storage) Object(ctx context.Context, token string) (library.File, error) {
	g, err := s.verify(token, "get")
	if err != nil {
		return library.File{}, err
	}
	userID, fileName, _ := strings.Cut(g.Key, "/")
	if f, err := s.st.Q.GetFile(ctx, db.GetFileParams{UserID: userID, FileKey: g.Key}); err == nil && f.Stored == 1 {
		return library.File{Path: s.objectPath(userID, g.Key), Name: path.Base(fileName), MIME: "application/octet-stream",
			Size: f.FileSize, Modified: time.UnixMilli(f.UpdatedAt)}, nil
	}
	switch kind, hash := classify(fileName); kind {
	case kindBook:
		return s.lib.BookFile(ctx, hash)
	case kindCover:
		f, _, err := s.lib.CoverFile(ctx, hash)
		return f, err
	}
	return library.File{}, Fail(http.StatusNotFound, "File not found")
}

// DownloadURLs signs downloads for the keys the user may read; others map to nil.
func (s *Storage) DownloadURLs(ctx context.Context, user auth.User, keys []string, baseURL string) (map[string]*string, error) {
	out := make(map[string]*string, len(keys))
	stored := map[string]bool{}
	var own []string
	for _, k := range keys {
		out[k] = nil
		if strings.HasPrefix(k, user.ID+"/") {
			own = append(own, k)
		}
	}
	for start := 0; start < len(own); start += batch {
		rows, err := s.st.Q.StoredFilesByKeys(ctx, db.StoredFilesByKeysParams{UserID: user.ID, Keys: own[start:min(start+batch, len(own))]})
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			stored[r.FileKey] = true
		}
	}
	for _, k := range own {
		ok := stored[k]
		if !ok {
			switch kind, hash := classify(strings.TrimPrefix(k, user.ID+"/")); kind {
			case kindBook:
				ok, _ = s.lib.HasBook(ctx, hash)
			case kindCover:
				_, _, err := s.lib.CoverFile(ctx, hash)
				ok = err == nil
			}
		}
		if !ok {
			continue
		}
		token, err := s.sign(user.ID, k, "get", 0, signedURLTTL)
		if err != nil {
			return nil, err
		}
		out[k] = new(objectURL(baseURL, token))
	}
	return out, nil
}

// ListParams are the query parameters of GET /api/storage/list.
type ListParams struct {
	Page, PageSize    int
	SortBy, SortOrder string
	BookHash, Search  string
}

// List lists the user's objects; filtering by bookHash also includes the library file and cover of that book.
func (s *Storage) List(ctx context.Context, user auth.User, p ListParams) (api.StorageListResponse, error) {
	if p.Page < 1 {
		p.Page = 1
	}
	if p.PageSize < 1 || p.PageSize > 1000 {
		p.PageSize = 50
	}
	col := map[string]string{"created_at": "created_at", "updated_at": "updated_at", "file_size": "file_size", "file_key": "file_key"}[p.SortBy]
	if col == "" {
		col = "created_at"
	}
	dir := "DESC"
	if p.SortOrder == "asc" {
		dir = "ASC"
	}
	where := "user_id = ? AND stored = 1"
	args := []any{user.ID}
	if p.BookHash != "" {
		where += " AND book_hash = ?"
		args = append(args, p.BookHash)
	}
	if p.Search != "" {
		where += ` AND file_key LIKE ? ESCAPE '\'`
		args = append(args, "%"+strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(p.Search)+"%")
	}
	var total int
	if err := s.st.R.QueryRowContext(ctx, "SELECT COUNT(*) FROM files WHERE "+where, args...).Scan(&total); err != nil {
		return api.StorageListResponse{}, err
	}
	rows, err := s.st.R.QueryContext(ctx, fmt.Sprintf("SELECT user_id, file_key, book_hash, replica_kind, replica_id, file_size, stored, created_at, updated_at FROM files WHERE %s ORDER BY %s %s LIMIT ? OFFSET ?", where, col, dir),
		append(args, p.PageSize, (p.Page-1)*p.PageSize)...)
	if err != nil {
		return api.StorageListResponse{}, err
	}
	defer rows.Close()
	files := map[string]db.File{}
	var order []string
	for rows.Next() {
		var f db.File
		if err := rows.Scan(&f.UserID, &f.FileKey, &f.BookHash, &f.ReplicaKind, &f.ReplicaID, &f.FileSize, &f.Stored, &f.CreatedAt, &f.UpdatedAt); err != nil {
			return api.StorageListResponse{}, err
		}
		files[f.FileKey] = f
		order = append(order, f.FileKey)
	}
	if err := rows.Err(); err != nil {
		return api.StorageListResponse{}, err
	}
	// Every other file of the same book or replica comes along (Readest groups them in its UI).
	var hashes, replicaIDs []*string
	for _, k := range order {
		if h := files[k].BookHash; h != nil {
			hashes = append(hashes, h)
		}
		if r := files[k].ReplicaID; r != nil {
			replicaIDs = append(replicaIDs, r)
		}
	}
	absorb := func(fs []db.File, err error) error {
		for _, f := range fs {
			if _, ok := files[f.FileKey]; !ok {
				files[f.FileKey] = f
				order = append(order, f.FileKey)
			}
		}
		return err
	}
	if len(hashes) > 0 {
		if err := absorb(s.st.Q.StoredFilesByBookHashes(ctx, db.StoredFilesByBookHashesParams{UserID: user.ID, Hashes: hashes})); err != nil {
			return api.StorageListResponse{}, err
		}
	}
	if len(replicaIDs) > 0 {
		if err := absorb(s.st.Q.StoredFilesByReplicaIDs(ctx, db.StoredFilesByReplicaIDsParams{UserID: user.ID, Ids: replicaIDs})); err != nil {
			return api.StorageListResponse{}, err
		}
	}
	res := api.StorageListResponse{Files: []api.StorageFile{}, Page: p.Page, PageSize: p.PageSize}
	for _, k := range order {
		f := files[k]
		res.Files = append(res.Files, api.StorageFile{FileKey: f.FileKey, FileSize: f.FileSize, BookHash: f.BookHash,
			ReplicaKind: f.ReplicaKind, ReplicaId: f.ReplicaID, CreatedAt: *isoMillis(&f.CreatedAt), UpdatedAt: *isoMillis(&f.UpdatedAt)})
	}
	if p.BookHash != "" {
		if bf, err := s.lib.BookFile(ctx, p.BookHash); err == nil {
			key := fmt.Sprintf("%s/Readest/Books/%s/%s", user.ID, p.BookHash, filepath.Base(bf.Path))
			created := bf.Modified.UnixMilli()
			res.Files = append(res.Files, api.StorageFile{FileKey: key, FileSize: bf.Size, BookHash: &p.BookHash,
				CreatedAt: *isoMillis(&created), UpdatedAt: *isoMillis(&created)})
			total++
			if cf, _, err := s.lib.CoverFile(ctx, p.BookHash); err == nil {
				st, _ := os.Stat(cf.Path)
				size := int64(0)
				if st != nil {
					size = st.Size()
				}
				mod := cf.Modified.UnixMilli()
				res.Files = append(res.Files, api.StorageFile{FileKey: fmt.Sprintf("%s/Readest/Books/%s/cover.png", user.ID, p.BookHash),
					FileSize: size, BookHash: &p.BookHash, CreatedAt: *isoMillis(&mod), UpdatedAt: *isoMillis(&mod)})
				total++
			}
		}
	}
	res.Total = total
	res.TotalPages = (total + p.PageSize - 1) / p.PageSize
	return res, nil
}

// Delete removes one object. A library book file removes the book from the library (admins only); a library
// cover is shared and stays.
func (s *Storage) Delete(ctx context.Context, user auth.User, key string) error {
	if !strings.HasPrefix(key, user.ID+"/") {
		return Fail(http.StatusForbidden, "Unauthorized access to the file")
	}
	if _, err := s.st.Q.GetFile(ctx, db.GetFileParams{UserID: user.ID, FileKey: key}); err == nil {
		if err := s.st.WQ.DeleteFile(ctx, db.DeleteFileParams{UserID: user.ID, FileKey: key}); err != nil {
			return err
		}
		os.Remove(s.objectPath(user.ID, key))
		return nil
	} else if !store.IsNotFound(err) {
		return err
	}
	switch kind, hash := classify(strings.TrimPrefix(key, user.ID+"/")); kind {
	case kindBook:
		if ok, err := s.lib.HasBook(ctx, hash); err != nil || !ok {
			if err != nil {
				return err
			}
			break
		}
		if !user.IsAdmin {
			return Fail(http.StatusForbidden, "Only an admin can remove a book from the library")
		}
		return s.lib.Delete(ctx, hash)
	case kindCover:
		if ok, _ := s.lib.HasBook(ctx, hash); ok {
			return nil
		}
	}
	return Fail(http.StatusNotFound, "File not found")
}

// PurgeUser removes a deleted user's objects from disk.
func (s *Storage) PurgeUser(userID string) error {
	if userID == "" || strings.ContainsAny(userID, `/\.`) {
		return nil
	}
	return os.RemoveAll(filepath.Join(s.objDir, userID))
}
