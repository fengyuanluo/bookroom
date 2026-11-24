package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"bookroom/internal/api"
	"bookroom/internal/apperr"
	"bookroom/internal/library"
	"bookroom/internal/stats"
)

func (s *Server) ListBooks(ctx context.Context, r api.ListBooksRequestObject) (api.ListBooksResponseObject, error) {
	p := r.Params
	lq := library.ListQuery{Q: deref(p.Q), Shelf: p.Shelf, Recursive: p.Recursive != nil && *p.Recursive, Tag: deref(p.Tag),
		Cursor: deref(p.Cursor), Desc: p.Order == nil || *p.Order != "asc"}
	if p.Format != nil {
		lq.Format = string(*p.Format)
	}
	if p.Status != nil {
		lq.Status = string(*p.Status)
	}
	if p.WorkType != nil {
		lq.WorkType = string(*p.WorkType)
	}
	if p.Sort != nil {
		lq.Sort = string(*p.Sort)
	}
	if p.Limit != nil {
		lq.Limit = *p.Limit
	}
	page, err := s.Lib.List(rc(ctx), currentUser(ctx).ID, lq)
	if err != nil {
		return nil, err
	}
	return api.ListBooks200JSONResponse(page), nil
}

func (s *Server) UploadBook(ctx context.Context, r api.UploadBookRequestObject) (api.UploadBookResponseObject, error) {
	u := currentUser(ctx)
	var book *api.Book
	created := false
	shelf := ""
	for {
		part, err := r.Body.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, apperr.BadRequest("invalid multipart body: " + err.Error())
		}
		switch part.FormName() {
		case "shelf":
			b, _ := io.ReadAll(io.LimitReader(part, 4096))
			shelf = strings.TrimSpace(string(b))
		case "file":
			if book != nil {
				return nil, apperr.BadRequest("send one file per request")
			}
			name := part.FileName()
			if name == "" {
				return nil, apperr.BadRequest("the file part needs a filename")
			}
			b, c, err := s.Lib.IngestReader(rc(ctx), part, name, library.IngestOptions{AddedBy: u.ID})
			if err != nil {
				return nil, err
			}
			book, created = &b, c
		}
		part.Close()
	}
	if book == nil {
		return nil, apperr.BadRequest("missing file part")
	}
	if shelf != "" && library.ReadestCanOpen(string(book.Format)) {
		b, err := s.Lib.SetState(rc(ctx), u.ID, book.Hash, &shelf, true, nil, false)
		if err != nil {
			return nil, err
		}
		book = &b
	}
	if created {
		return api.UploadBook201JSONResponse(*book), nil
	}
	return api.UploadBook200JSONResponse(*book), nil
}

func (s *Server) GetBook(ctx context.Context, r api.GetBookRequestObject) (api.GetBookResponseObject, error) {
	b, err := s.Lib.Get(rc(ctx), currentUser(ctx).ID, r.Hash)
	if err != nil {
		return nil, err
	}
	return api.GetBook200JSONResponse(b), nil
}

func (s *Server) UpdateBookMetadata(ctx context.Context, r api.UpdateBookMetadataRequestObject) (api.UpdateBookMetadataResponseObject, error) {
	b, err := s.Lib.UpdateMetadata(rc(ctx), currentUser(ctx).ID, r.Hash, *r.Body)
	if err != nil {
		return nil, err
	}
	return api.UpdateBookMetadata200JSONResponse(b), nil
}

func (s *Server) DeleteBook(ctx context.Context, r api.DeleteBookRequestObject) (api.DeleteBookResponseObject, error) {
	if err := s.Lib.Delete(rc(ctx), r.Hash); err != nil {
		return nil, err
	}
	return api.DeleteBook204Response{}, nil
}

func (s *Server) DownloadBook(ctx context.Context, r api.DownloadBookRequestObject) (api.DownloadBookResponseObject, error) {
	f, err := s.Lib.BookFile(rc(ctx), r.Hash)
	if err != nil {
		return nil, err
	}
	return fileResponse{req: ginCtx(ctx).Request, file: f, etag: r.Hash, attachment: true, cache: "private, max-age=31536000, immutable"}, nil
}

func (s *Server) GetBookCover(ctx context.Context, r api.GetBookCoverRequestObject) (api.GetBookCoverResponseObject, error) {
	f, etag, err := s.Lib.CoverFile(rc(ctx), r.Hash)
	if err != nil {
		return nil, err
	}
	return fileResponse{req: ginCtx(ctx).Request, file: f, etag: etag, cache: "private, max-age=86400"}, nil
}

// optionalNullable decodes a json.RawMessage member: absent → (nil, false); null → (nil, true).
func optionalNullable(raw json.RawMessage) (*string, bool, error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, true, nil
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, false, apperr.BadRequest("expected a string or null")
	}
	return &v, true, nil
}

func (s *Server) UpdateBookState(ctx context.Context, r api.UpdateBookStateRequestObject) (api.UpdateBookStateResponseObject, error) {
	shelf, shelfSet, err := optionalNullable(r.Body.Shelf)
	if err != nil {
		return nil, err
	}
	status, statusSet, err := optionalNullable(r.Body.ReadingStatus)
	if err != nil {
		return nil, err
	}
	if status != nil && *status != "" {
		switch *status {
		case "unread", "reading", "finished", "abandoned":
		default:
			return nil, apperr.BadRequest("readingStatus must be unread, reading, finished or abandoned")
		}
	}
	b, err := s.Lib.SetState(rc(ctx), currentUser(ctx).ID, r.Hash, shelf, shelfSet, status, statusSet)
	if err != nil {
		return nil, err
	}
	return api.UpdateBookState200JSONResponse(b), nil
}

func (s *Server) SetMetadataLocks(ctx context.Context, r api.SetMetadataLocksRequestObject) (api.SetMetadataLocksResponseObject, error) {
	b, err := s.Lib.SetLocks(rc(ctx), currentUser(ctx).ID, r.Hash, r.Body.Fields)
	if err != nil {
		return nil, err
	}
	return api.SetMetadataLocks200JSONResponse(b), nil
}

func (s *Server) SearchBookMetadata(ctx context.Context, r api.SearchBookMetadataRequestObject) (api.SearchBookMetadataResponseObject, error) {
	var q api.MetadataQuery
	if r.Body != nil {
		q = *r.Body
	}
	res, err := s.Scrape.Search(rc(ctx), r.Hash, q)
	if err != nil {
		return nil, err
	}
	return api.SearchBookMetadata200JSONResponse(res), nil
}

func (s *Server) ApplyBookMetadata(ctx context.Context, r api.ApplyBookMetadataRequestObject) (api.ApplyBookMetadataResponseObject, error) {
	b, err := s.Scrape.Apply(rc(ctx), currentUser(ctx).ID, r.Hash, *r.Body)
	if err != nil {
		return nil, err
	}
	return api.ApplyBookMetadata200JSONResponse(b), nil
}

func (s *Server) RefreshBookMetadata(ctx context.Context, r api.RefreshBookMetadataRequestObject) (api.RefreshBookMetadataResponseObject, error) {
	job, err := s.Scrape.Enqueue(rc(ctx), r.Hash)
	if err != nil {
		return nil, err
	}
	return api.RefreshBookMetadata202JSONResponse(job), nil
}

func (s *Server) ListMetadataProviders(ctx context.Context, _ api.ListMetadataProvidersRequestObject) (api.ListMetadataProvidersResponseObject, error) {
	return api.ListMetadataProviders200JSONResponse{Items: s.Scrape.Providers(rc(ctx))}, nil
}

func (s *Server) ListShelves(ctx context.Context, _ api.ListShelvesRequestObject) (api.ListShelvesResponseObject, error) {
	items, err := s.Lib.Shelves(rc(ctx), currentUser(ctx).ID)
	if err != nil {
		return nil, err
	}
	return api.ListShelves200JSONResponse{Items: items}, nil
}

func (s *Server) RenameShelf(ctx context.Context, r api.RenameShelfRequestObject) (api.RenameShelfResponseObject, error) {
	n, err := s.Lib.RenameShelf(rc(ctx), currentUser(ctx).ID, r.Body.From, r.Body.To)
	if err != nil {
		return nil, err
	}
	return api.RenameShelf200JSONResponse{Moved: n}, nil
}

func (s *Server) ListTags(ctx context.Context, _ api.ListTagsRequestObject) (api.ListTagsResponseObject, error) {
	items, err := s.Lib.Tags(rc(ctx))
	if err != nil {
		return nil, err
	}
	return api.ListTags200JSONResponse{Items: items}, nil
}

func (s *Server) GetStatsSummary(ctx context.Context, r api.GetStatsSummaryRequestObject) (api.GetStatsSummaryResponseObject, error) {
	rng, err := stats.NewRange(r.Params.From, r.Params.To, r.Params.Tz)
	if err != nil {
		return nil, err
	}
	out, err := s.Stats.Summary(rc(ctx), currentUser(ctx).ID, rng)
	if err != nil {
		return nil, err
	}
	return api.GetStatsSummary200JSONResponse(out), nil
}

func (s *Server) GetStatsDaily(ctx context.Context, r api.GetStatsDailyRequestObject) (api.GetStatsDailyResponseObject, error) {
	rng, err := stats.NewRange(r.Params.From, r.Params.To, r.Params.Tz)
	if err != nil {
		return nil, err
	}
	out, err := s.Stats.Daily(rc(ctx), currentUser(ctx).ID, rng)
	if err != nil {
		return nil, err
	}
	return api.GetStatsDaily200JSONResponse(out), nil
}

func (s *Server) GetStatsBooks(ctx context.Context, r api.GetStatsBooksRequestObject) (api.GetStatsBooksResponseObject, error) {
	rng, err := stats.NewRange(r.Params.From, r.Params.To, nil)
	if err != nil {
		return nil, err
	}
	limit := 0
	if r.Params.Limit != nil {
		limit = *r.Params.Limit
	}
	out, err := s.Stats.Books(rc(ctx), currentUser(ctx).ID, rng, limit)
	if err != nil {
		return nil, err
	}
	return api.GetStatsBooks200JSONResponse(out), nil
}

func deref[T ~string](s *T) string {
	if s == nil {
		return ""
	}
	return string(*s)
}
