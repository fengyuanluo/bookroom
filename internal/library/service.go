// Package library owns the shared book collection: ingesting files, their metadata, covers, the per-user
// shelf/status state, and the projection of library books into every user's Readest `books` rows.
package library

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"bookroom/internal/api"
	"bookroom/internal/apperr"
	"bookroom/internal/store"
	"bookroom/internal/store/db"
)

// Service is the library. All file paths are below DataDir.
type Service struct {
	st         *store.Store
	libDir     string
	coverDir   string
	tmpDir     string
	maxUpload  int64
	autoScrape bool

	locks sync.Map // hash -> *sync.Mutex, serializes concurrent ingests of the same file
}

// New prepares the directory layout under dataDir.
func New(st *store.Store, dataDir string, maxUpload int64, autoScrape bool) (*Service, error) {
	s := &Service{
		st: st, libDir: filepath.Join(dataDir, "library"), coverDir: filepath.Join(dataDir, "covers"),
		tmpDir: filepath.Join(dataDir, "tmp"), maxUpload: maxUpload, autoScrape: autoScrape,
	}
	for _, d := range []string{s.libDir, s.coverDir, s.tmpDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// TmpDir is where streamed uploads are spooled before they are hashed.
func (s *Service) TmpDir() string { return s.tmpDir }

func (s *Service) lock(hash string) func() {
	m, _ := s.locks.LoadOrStore(hash, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// IngestOptions tunes one ingest.
type IngestOptions struct {
	AddedBy    string // user id of the uploader; "" for server-side imports
	Shelf      string // shelf to put the book on for AddedBy
	ExpectHash string // when set, the file must hash to this value (Readest storage uploads)
	// SkipProjection names a user whose Readest client pushes its own books row for this file, so the server
	// must not race it with a projected row.
	SkipProjection string
}

// IngestReader spools r to disk and ingests it. created is false when the hash was already in the library.
func (s *Service) IngestReader(ctx context.Context, r io.Reader, name string, opts IngestOptions) (api.Book, bool, error) {
	if _, ok := FormatOf(name); !ok {
		return api.Book{}, false, apperr.Unsupported("unsupported file type: " + filepath.Ext(name))
	}
	tmp, err := s.Spool(r)
	if err != nil {
		return api.Book{}, false, err
	}
	defer os.Remove(tmp) // no-op after a successful rename into the library
	return s.ingest(ctx, tmp, true, name, opts)
}

// Spool copies r into a temporary file, enforcing the upload limit.
func (s *Service) Spool(r io.Reader) (string, error) {
	f, err := os.CreateTemp(s.tmpDir, "upload-*")
	if err != nil {
		return "", err
	}
	n, err := io.Copy(f, io.LimitReader(r, s.maxUpload+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > s.maxUpload {
		err = apperr.TooLarge(fmt.Sprintf("file exceeds %d bytes", s.maxUpload))
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// IngestFile imports a file that stays where it is: it is hard-linked into the library when possible and
// copied otherwise.
func (s *Service) IngestFile(ctx context.Context, path string, opts IngestOptions) (api.Book, bool, error) {
	if _, ok := FormatOf(path); !ok {
		return api.Book{}, false, apperr.Unsupported("unsupported file type: " + filepath.Ext(path))
	}
	return s.ingest(ctx, path, false, filepath.Base(path), opts)
}

func (s *Service) ingest(ctx context.Context, src string, owned bool, name string, opts IngestOptions) (api.Book, bool, error) {
	format, ok := FormatOf(name)
	if !ok {
		return api.Book{}, false, apperr.Unsupported("unsupported file type: " + filepath.Ext(name))
	}
	hash, size, err := PartialMD5File(src)
	if err != nil {
		return api.Book{}, false, err
	}
	if size == 0 {
		return api.Book{}, false, apperr.BadRequest("empty file")
	}
	if opts.ExpectHash != "" && opts.ExpectHash != hash {
		return api.Book{}, false, apperr.BadRequest("file content does not match bookHash")
	}
	unlock := s.lock(hash)
	defer unlock()

	if _, err := s.st.Q.GetLibraryBook(ctx, hash); err == nil {
		if opts.AddedBy != "" && opts.Shelf != "" {
			if err := s.setShelf(ctx, opts.AddedBy, hash, opts.Shelf); err != nil {
				return api.Book{}, false, err
			}
		}
		b, err := s.Get(ctx, opts.AddedBy, hash)
		return b, false, err
	} else if !store.IsNotFound(err) {
		return api.Book{}, false, err
	}

	m := Parse(ctx, src, format, name)
	now := time.Now().UnixMilli()
	lb := db.LibraryBook{
		Hash: hash, Format: string(format), Size: size, FileName: hash + "." + format.Ext(),
		OriginalFilename: name, FilenameMd5: md5Hex(name), Title: m.Title, Authors: encodeList(m.Authors),
		Series: m.Series, SeriesIndex: m.SeriesIndex, Description: m.Description, Publisher: m.Publisher,
		Published: m.Published, Language: m.Language, Isbn: m.ISBN, Tags: encodeList(m.Tags),
		WorkType: string(api.WorkTypeUnknown), LockedFields: "[]", CreatedAt: now, UpdatedAt: now,
	}
	if lb.Title == "" {
		lb.Title = strings.TrimSuffix(name, filepath.Ext(name))
	}
	if m.PageCount != nil {
		lb.PageCount = new(int64(*m.PageCount))
	}
	if lb.Isbn != "" {
		lb.WorkType = string(api.WorkTypePublished)
	}
	if opts.AddedBy != "" {
		lb.AddedBy = new(opts.AddedBy)
	}

	dest := s.bookPath(&lb)
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return api.Book{}, false, err
	}
	if err := placeFile(src, dest, owned); err != nil {
		return api.Book{}, false, err
	}
	if len(m.Cover) > 0 && m.CoverExt != "" {
		if err := s.writeCover(&lb, m.Cover, m.CoverExt); err != nil {
			os.Remove(dest)
			return api.Book{}, false, err
		}
	}

	err = s.st.Tx(ctx, func(q *db.Queries) error {
		if err := q.InsertLibraryBook(ctx, insertParams(&lb)); err != nil {
			return err
		}
		users, err := q.ListUserIDs(ctx)
		if err != nil {
			return err
		}
		row := projection(&lb, now)
		for _, uid := range users {
			if uid == opts.SkipProjection {
				continue
			}
			row.UserID = uid
			if err := q.InsertBookIfAbsent(ctx, row); err != nil {
				return err
			}
		}
		if opts.AddedBy != "" && opts.Shelf != "" && opts.AddedBy != opts.SkipProjection {
			if _, err := setGroup(ctx, q, opts.AddedBy, hash, opts.Shelf, now); err != nil {
				return err
			}
		}
		if s.autoScrape {
			return q.EnqueueScrapeJob(ctx, db.EnqueueScrapeJobParams{Hash: hash, NextRunAt: now, UpdatedAt: now})
		}
		return nil
	})
	if err != nil {
		os.Remove(dest)
		if lb.CoverFile != "" {
			os.Remove(s.coverPath(&lb))
		}
		return api.Book{}, false, err
	}
	b, err := s.Get(ctx, opts.AddedBy, hash)
	return b, true, err
}

// placeFile moves (owned) or links/copies src to dest.
func placeFile(src, dest string, owned bool) error {
	if owned {
		if err := os.Rename(src, dest); err == nil {
			return nil
		}
	} else if err := os.Link(src, dest); err == nil {
		return nil
	}
	return copyFile(src, dest)
}

func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dest + ".part"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

func insertParams(lb *db.LibraryBook) db.InsertLibraryBookParams {
	return db.InsertLibraryBookParams{
		Hash: lb.Hash, Format: lb.Format, Size: lb.Size, FileName: lb.FileName, OriginalFilename: lb.OriginalFilename,
		FilenameMd5: lb.FilenameMd5, Title: lb.Title, Authors: lb.Authors, Series: lb.Series, SeriesIndex: lb.SeriesIndex,
		Description: lb.Description, Publisher: lb.Publisher, Published: lb.Published, Language: lb.Language,
		Isbn: lb.Isbn, Tags: lb.Tags, PageCount: lb.PageCount, WorkType: lb.WorkType, Webnovel: lb.Webnovel,
		CoverFile: lb.CoverFile, CoverHash: lb.CoverHash, LockedFields: lb.LockedFields,
		MetadataSource: lb.MetadataSource, AddedBy: lb.AddedBy, CreatedAt: lb.CreatedAt, UpdatedAt: lb.UpdatedAt,
	}
}

func (s *Service) bookPath(lb *db.LibraryBook) string {
	return filepath.Join(s.libDir, lb.Hash[:2], lb.Hash, lb.FileName)
}

func (s *Service) coverPath(lb *db.LibraryBook) string {
	return filepath.Join(s.coverDir, lb.Hash[:2], lb.CoverFile)
}

// writeCover stores cover bytes and updates the row's cover fields (not persisted here).
func (s *Service) writeCover(lb *db.LibraryBook, data []byte, ext string) error {
	sum := sha256.Sum256(data)
	name := lb.Hash + "." + ext
	p := filepath.Join(s.coverDir, lb.Hash[:2], name)
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}
	tmp := p + ".part"
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		return err
	}
	if lb.CoverFile != "" && lb.CoverFile != name {
		os.Remove(s.coverPath(lb))
	}
	lb.CoverFile, lb.CoverHash = name, hex.EncodeToString(sum[:8])
	return nil
}

// File locates a book file for download.
type File struct {
	Path     string
	Name     string // download file name
	MIME     string
	Size     int64
	Modified time.Time
}

// BookFile returns the stored file of a book.
func (s *Service) BookFile(ctx context.Context, hash string) (File, error) {
	lb, err := s.row(ctx, hash)
	if err != nil {
		return File{}, err
	}
	name := lb.OriginalFilename
	if name == "" {
		name = lb.FileName
	}
	return File{Path: s.bookPath(&lb), Name: name, MIME: Format(lb.Format).MIME(), Size: lb.Size,
		Modified: time.UnixMilli(lb.CreatedAt)}, nil
}

// CoverFile returns the stored cover of a book.
func (s *Service) CoverFile(ctx context.Context, hash string) (File, string, error) {
	lb, err := s.row(ctx, hash)
	if err != nil {
		return File{}, "", err
	}
	if lb.CoverFile == "" {
		return File{}, "", apperr.NotFound("book has no cover")
	}
	ext := strings.TrimPrefix(filepath.Ext(lb.CoverFile), ".")
	mime := "image/" + ext
	if ext == "jpg" {
		mime = "image/jpeg"
	}
	return File{Path: s.coverPath(&lb), Name: "cover." + ext, MIME: mime, Modified: time.UnixMilli(lb.UpdatedAt)}, lb.CoverHash, nil
}

// HasBook reports whether hash is in the library.
func (s *Service) HasBook(ctx context.Context, hash string) (bool, error) {
	_, err := s.st.Q.GetLibraryBook(ctx, hash)
	if store.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

func (s *Service) row(ctx context.Context, hash string) (db.LibraryBook, error) {
	lb, err := s.st.Q.GetLibraryBook(ctx, hash)
	if store.IsNotFound(err) {
		return lb, apperr.NotFound("no such book")
	}
	return lb, err
}

// Delete removes a book from the library and tombstones it in every user's Readest library.
func (s *Service) Delete(ctx context.Context, hash string) error {
	lb, err := s.row(ctx, hash)
	if err != nil {
		return err
	}
	err = s.st.Tx(ctx, func(q *db.Queries) error {
		if _, err := q.DeleteLibraryBook(ctx, hash); err != nil {
			return err
		}
		return q.TombstoneLibraryBook(ctx, db.TombstoneLibraryBookParams{Now: new(time.Now().UnixMilli()), BookHash: hash})
	})
	if err != nil {
		return err
	}
	os.RemoveAll(filepath.Dir(s.bookPath(&lb)))
	if lb.CoverFile != "" {
		os.Remove(s.coverPath(&lb))
	}
	return nil
}

// SetCoverIfMissing adopts a cover image (e.g. the one a Readest client rendered for a PDF) when the book
// has none. It reports whether the cover was taken.
func (s *Service) SetCoverIfMissing(ctx context.Context, hash string, data []byte) (bool, error) {
	ext := sniffImage(data)
	if ext == "" || len(data) > maxCoverBytes {
		return false, nil
	}
	unlock := s.lock(hash)
	defer unlock()
	lb, err := s.row(ctx, hash)
	if err != nil || lb.CoverFile != "" {
		return false, err
	}
	return true, s.saveMetadata(ctx, &lb, &Metadata{Cover: data, CoverExt: ext}, []api.MetadataField{api.MetadataFieldCover}, applyOverwrite, "")
}

// ProjectInto inserts every library book into a new user's Readest rows (auth.UserHook).
func ProjectInto(ctx context.Context, q *db.Queries, userID string, now int64) error {
	books, err := q.ListLibraryBooks(ctx)
	if err != nil {
		return err
	}
	for i := range books {
		row := projection(&books[i], now)
		row.UserID = userID
		if err := q.InsertBookIfAbsent(ctx, row); err != nil {
			return err
		}
	}
	return nil
}

// projection is the Readest books row a library book starts as in a user's library: uploaded (so Readest
// offers the download), dated by when it entered the library.
func projection(lb *db.LibraryBook, now int64) db.InsertBookIfAbsentParams {
	p := db.InsertBookIfAbsentParams{
		BookHash: lb.Hash, Format: lb.Format, Title: lb.Title, OriginalFilename: new(lb.OriginalFilename),
		Author: strings.Join(decodeList(lb.Authors), ", "), Tags: new(lb.Tags), PageCount: lb.PageCount,
		Metadata: new(readestMetadata(lb)), MetadataUpdatedAt: new(lb.UpdatedAt), CreatedAt: new(lb.CreatedAt),
		UpdatedAt: new(lb.CreatedAt), UploadedAt: new(lb.CreatedAt), SyncedAt: now,
	}
	if lb.CoverHash != "" {
		p.CoverHash, p.CoverUpdatedAt = new(lb.CoverHash), new(lb.UpdatedAt)
	}
	return p
}

// readestMetadata renders the library fields as Readest's BookMetadata JSON.
func readestMetadata(lb *db.LibraryBook) string {
	m := map[string]any{"title": lb.Title, "author": strings.Join(decodeList(lb.Authors), ", "), "language": lb.Language}
	put := func(k, v string) {
		if v != "" {
			m[k] = v
		}
	}
	put("publisher", lb.Publisher)
	put("published", lb.Published)
	put("description", lb.Description)
	put("isbn", lb.Isbn)
	put("identifier", lb.Isbn)
	put("series", lb.Series)
	if lb.SeriesIndex != nil && lb.Series != "" {
		m["seriesIndex"] = *lb.SeriesIndex
	}
	if tags := decodeList(lb.Tags); len(tags) > 0 {
		m["subject"] = tags
	}
	return marshalJSON(m)
}

func marshalJSON(v any) string {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return strings.TrimSuffix(sb.String(), "\n")
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}
