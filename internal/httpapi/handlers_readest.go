package httpapi

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"bookroom/internal/api"
	"bookroom/internal/readest"
)

func (s *Server) ReadestPull(ctx context.Context, r api.ReadestPullRequestObject) (api.ReadestPullResponseObject, error) {
	p := readest.PullParams{Since: r.Params.Since, Book: deref(r.Params.Book), MetaHash: deref(r.Params.MetaHash)}
	if r.Params.Type != nil {
		p.Type = string(*r.Params.Type)
	}
	if r.Params.Limit != nil {
		p.Limit = *r.Params.Limit
	}
	res, err := s.Sync.Pull(rc(ctx), currentUser(ctx).ID, p)
	if err != nil {
		return nil, err
	}
	c := ginCtx(ctx)
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	return api.ReadestPull200JSONResponse(res), nil
}

func (s *Server) ReadestPush(ctx context.Context, r api.ReadestPushRequestObject) (api.ReadestPushResponseObject, error) {
	res, err := s.Sync.Push(rc(ctx), currentUser(ctx), *r.Body)
	if err != nil {
		return nil, err
	}
	return api.ReadestPush200JSONResponse(res), nil
}

func (s *Server) ReadestPullReplicas(ctx context.Context, r api.ReadestPullReplicasRequestObject) (api.ReadestPullReplicasResponseObject, error) {
	rows, err := s.Replicas.Pull(rc(ctx), currentUser(ctx).ID, string(r.Params.Kind), r.Params.Since)
	if err != nil {
		return nil, err
	}
	return api.ReadestPullReplicas200JSONResponse{Rows: rows}, nil
}

func (s *Server) ReadestPostReplicas(ctx context.Context, r api.ReadestPostReplicasRequestObject) (api.ReadestPostReplicasResponseObject, error) {
	uid := currentUser(ctx).ID
	if r.Body.Cursors != nil {
		results, err := s.Replicas.PullBatch(rc(ctx), uid, *r.Body.Cursors)
		if err != nil {
			return nil, err
		}
		return api.ReadestPostReplicas200JSONResponse{Results: &results}, nil
	}
	if r.Body.Rows == nil {
		return nil, readest.Fail(http.StatusBadRequest, "body.rows must be an array")
	}
	rows, err := s.Replicas.Push(rc(ctx), uid, *r.Body.Rows)
	if err != nil {
		return nil, err
	}
	return api.ReadestPostReplicas200JSONResponse{Rows: &rows}, nil
}

func (s *Server) ReadestListReplicaKeys(ctx context.Context, _ api.ReadestListReplicaKeysRequestObject) (api.ReadestListReplicaKeysResponseObject, error) {
	rows, err := s.Replicas.ListKeys(rc(ctx), currentUser(ctx).ID)
	if err != nil {
		return nil, err
	}
	return api.ReadestListReplicaKeys200JSONResponse{Rows: rows}, nil
}

func (s *Server) ReadestCreateReplicaKey(ctx context.Context, r api.ReadestCreateReplicaKeyRequestObject) (api.ReadestCreateReplicaKeyResponseObject, error) {
	row, err := s.Replicas.CreateKey(rc(ctx), currentUser(ctx).ID, r.Body.Alg)
	if err != nil {
		return nil, err
	}
	return api.ReadestCreateReplicaKey201JSONResponse{Row: row}, nil
}

func (s *Server) ReadestForgetReplicaKeys(ctx context.Context, _ api.ReadestForgetReplicaKeysRequestObject) (api.ReadestForgetReplicaKeysResponseObject, error) {
	if err := s.Replicas.ForgetKeys(rc(ctx), currentUser(ctx).ID); err != nil {
		return nil, err
	}
	return api.ReadestForgetReplicaKeys200JSONResponse{Ok: true}, nil
}

func (s *Server) StorageUploadUrl(ctx context.Context, r api.StorageUploadUrlRequestObject) (api.StorageUploadUrlResponseObject, error) {
	res, err := s.Storage.UploadURL(rc(ctx), currentUser(ctx), *r.Body, s.baseURL(ctx))
	if err != nil {
		return nil, err
	}
	return api.StorageUploadUrl200JSONResponse(res), nil
}

// rawFileKey reproduces Readest's tolerance for unencoded '&' and '+' in fileKey (pages/api/storage/download.ts).
func rawFileKey(rawQuery, parsed string) string {
	i := strings.Index(rawQuery, "fileKey=")
	if i < 0 || !strings.Contains(rawQuery, "&") {
		return parsed
	}
	v := rawQuery[i+len("fileKey="):]
	v = strings.TrimSuffix(strings.NewReplacer("+", "%20", "&", "%26").Replace(v), "=")
	if dec, err := url.PathUnescape(v); err == nil {
		return dec
	}
	return parsed
}

func (s *Server) StorageDownloadUrl(ctx context.Context, r api.StorageDownloadUrlRequestObject) (api.StorageDownloadUrlResponseObject, error) {
	key := rawFileKey(ginCtx(ctx).Request.URL.RawQuery, r.Params.FileKey)
	if key == "" {
		return nil, readest.Fail(http.StatusBadRequest, "Missing or invalid fileKey")
	}
	urls, err := s.Storage.DownloadURLs(rc(ctx), currentUser(ctx), []string{key}, s.baseURL(ctx))
	if err != nil {
		return nil, err
	}
	if urls[key] == nil {
		return nil, readest.Fail(http.StatusNotFound, "File not found")
	}
	return api.StorageDownloadUrl200JSONResponse{DownloadUrl: *urls[key]}, nil
}

func (s *Server) StorageDownloadUrls(ctx context.Context, r api.StorageDownloadUrlsRequestObject) (api.StorageDownloadUrlsResponseObject, error) {
	if len(r.Body.FileKeys) == 0 {
		return nil, readest.Fail(http.StatusBadRequest, "fileKeys array cannot be empty")
	}
	urls, err := s.Storage.DownloadURLs(rc(ctx), currentUser(ctx), r.Body.FileKeys, s.baseURL(ctx))
	if err != nil {
		return nil, err
	}
	return api.StorageDownloadUrls200JSONResponse{DownloadUrls: urls}, nil
}

func (s *Server) StorageList(ctx context.Context, r api.StorageListRequestObject) (api.StorageListResponseObject, error) {
	p := readest.ListParams{BookHash: deref(r.Params.BookHash), Search: deref(r.Params.Search)}
	if r.Params.Page != nil {
		p.Page = *r.Params.Page
	}
	if r.Params.PageSize != nil {
		p.PageSize = *r.Params.PageSize
	}
	if r.Params.SortBy != nil {
		p.SortBy = string(*r.Params.SortBy)
	}
	if r.Params.SortOrder != nil {
		p.SortOrder = string(*r.Params.SortOrder)
	}
	res, err := s.Storage.List(rc(ctx), currentUser(ctx), p)
	if err != nil {
		return nil, err
	}
	return api.StorageList200JSONResponse(res), nil
}

func (s *Server) StorageDelete(ctx context.Context, r api.StorageDeleteRequestObject) (api.StorageDeleteResponseObject, error) {
	if r.Params.FileKey == "" {
		return nil, readest.Fail(http.StatusBadRequest, "Missing or invalid fileKey")
	}
	if err := s.Storage.Delete(rc(ctx), currentUser(ctx), r.Params.FileKey); err != nil {
		return nil, err
	}
	return api.StorageDelete200JSONResponse{Message: "File deleted successfully"}, nil
}

func (s *Server) StoragePutObject(ctx context.Context, r api.StoragePutObjectRequestObject) (api.StoragePutObjectResponseObject, error) {
	if err := s.Storage.Put(rc(ctx), r.Token, r.Body); err != nil {
		return nil, err
	}
	return api.StoragePutObject200Response{}, nil
}

func (s *Server) StorageGetObject(ctx context.Context, r api.StorageGetObjectRequestObject) (api.StorageGetObjectResponseObject, error) {
	f, err := s.Storage.Object(rc(ctx), r.Token)
	if err != nil {
		return nil, err
	}
	return fileResponse{req: ginCtx(ctx).Request, file: f, cache: "private, no-store"}, nil
}
