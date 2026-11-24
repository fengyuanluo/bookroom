package httpapi

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"bookroom/internal/api"
	"bookroom/internal/kosync"
	"bookroom/internal/opds"
	"bookroom/internal/proxy"
)

// ── KOSync ──

func (s *Server) KosyncHealthcheck(context.Context, api.KosyncHealthcheckRequestObject) (api.KosyncHealthcheckResponseObject, error) {
	return api.KosyncHealthcheck200JSONResponse{State: "OK"}, nil
}

func (s *Server) KosyncCreateUser(context.Context, api.KosyncCreateUserRequestObject) (api.KosyncCreateUserResponseObject, error) {
	return nil, kosync.Fail(http.StatusPaymentRequired, kosync.CodeRegistrationOff,
		"User registration is disabled. Create a device key in BookRoom and use it as the password.")
}

func (s *Server) KosyncAuth(context.Context, api.KosyncAuthRequestObject) (api.KosyncAuthResponseObject, error) {
	return api.KosyncAuth200JSONResponse{Authorized: "OK"}, nil
}

func (s *Server) KosyncPutProgress(ctx context.Context, r api.KosyncPutProgressRequestObject) (api.KosyncPutProgressResponseObject, error) {
	res, err := s.Kosync.Put(rc(ctx), currentUser(ctx).ID, *r.Body)
	if err != nil {
		return nil, err
	}
	return api.KosyncPutProgress200JSONResponse(res), nil
}

func (s *Server) KosyncGetProgress(ctx context.Context, r api.KosyncGetProgressRequestObject) (api.KosyncGetProgressResponseObject, error) {
	res, err := s.Kosync.Get(rc(ctx), currentUser(ctx).ID, r.Document)
	if err != nil {
		return nil, err
	}
	return api.KosyncGetProgress200JSONResponse(res), nil
}

// ── OPDS ──

func page(p *int) int {
	if p == nil || *p < 1 {
		return 1
	}
	return *p
}

func (s *Server) OpdsRoot(ctx context.Context, _ api.OpdsRootRequestObject) (api.OpdsRootResponseObject, error) {
	f, err := s.OPDS.Root(s.baseURL(ctx))
	if err != nil {
		return nil, err
	}
	return feedResponse{f}, nil
}

func (s *Server) OpdsBooks(ctx context.Context, r api.OpdsBooksRequestObject) (api.OpdsBooksResponseObject, error) {
	q := opds.BookQuery{Tag: deref(r.Params.Tag), Page: page(r.Params.Page)}
	params := url.Values{}
	title, id := "Recently added", "books"
	if r.Params.Sort != nil {
		q.Sort = string(*r.Params.Sort)
		params.Set("sort", q.Sort)
		if q.Sort == "title" {
			title, id = "All books by title", "books:title"
		}
	}
	if r.Params.Status != nil {
		q.Status = string(*r.Params.Status)
		params.Set("status", q.Status)
		title, id = "Books: "+q.Status, "books:status:"+q.Status
	}
	if q.Tag != "" {
		params.Set("tag", q.Tag)
		title, id = "Tag: "+q.Tag, "books:tag:"+q.Tag
	}
	self := "/opds/books"
	if len(params) > 0 {
		self += "?" + params.Encode()
	}
	f, err := s.OPDS.Books(rc(ctx), s.baseURL(ctx), currentUser(ctx).ID, title, id, self, q)
	if err != nil {
		return nil, err
	}
	return feedResponse{f}, nil
}

func (s *Server) OpdsShelf(ctx context.Context, r api.OpdsShelfRequestObject) (api.OpdsShelfResponseObject, error) {
	f, err := s.OPDS.Shelf(rc(ctx), s.baseURL(ctx), currentUser(ctx).ID, deref(r.Params.Path), page(r.Params.Page))
	if err != nil {
		return nil, err
	}
	return feedResponse{f}, nil
}

func (s *Server) OpdsTags(ctx context.Context, _ api.OpdsTagsRequestObject) (api.OpdsTagsResponseObject, error) {
	f, err := s.OPDS.Tags(rc(ctx), s.baseURL(ctx))
	if err != nil {
		return nil, err
	}
	return feedResponse{f}, nil
}

func (s *Server) OpdsSearch(ctx context.Context, r api.OpdsSearchRequestObject) (api.OpdsSearchResponseObject, error) {
	q := opds.BookQuery{Q: r.Params.Q, Page: page(r.Params.Page)}
	self := "/opds/search?" + url.Values{"q": {r.Params.Q}}.Encode()
	f, err := s.OPDS.Books(rc(ctx), s.baseURL(ctx), currentUser(ctx).ID, "Search: "+r.Params.Q, "search:"+r.Params.Q, self, q)
	if err != nil {
		return nil, err
	}
	return feedResponse{f}, nil
}

func (s *Server) OpdsOpenSearch(ctx context.Context, _ api.OpdsOpenSearchRequestObject) (api.OpdsOpenSearchResponseObject, error) {
	return feedResponse{opds.OpenSearchDescription(s.baseURL(ctx))}, nil
}

func (s *Server) OpdsDownload(ctx context.Context, r api.OpdsDownloadRequestObject) (api.OpdsDownloadResponseObject, error) {
	f, err := s.Lib.BookFile(rc(ctx), r.Hash)
	if err != nil {
		return nil, err
	}
	return fileResponse{req: ginCtx(ctx).Request, file: f, etag: r.Hash, attachment: true}, nil
}

func (s *Server) OpdsCover(ctx context.Context, r api.OpdsCoverRequestObject) (api.OpdsCoverResponseObject, error) {
	f, etag, err := s.Lib.CoverFile(rc(ctx), r.Hash)
	if err != nil {
		return nil, err
	}
	return fileResponse{req: ginCtx(ctx).Request, file: f, etag: etag, cache: "private, max-age=86400"}, nil
}

// ── proxies ──

// opdsProxyRequest reads the query like Readest does: `url` runs up to the last &stream=, &auth= or
// &headers=, so catalog URLs with their own unencoded query survive.
func opdsProxyRequest(rawQuery string, p api.ProxyOpdsParams, upstreamAuth string) proxy.OPDSRequest {
	req := proxy.OPDSRequest{URL: string(p.Url)}
	if i := strings.Index(rawQuery, "url="); i >= 0 {
		end := len(rawQuery)
		for _, marker := range []string{"&stream=", "&auth=", "&headers=", "&access_token="} {
			if j := strings.LastIndex(rawQuery, marker); j > i && j < end {
				end = j
			}
		}
		if dec, err := url.QueryUnescape(rawQuery[i+4 : end]); err == nil {
			req.URL = dec
		}
	}
	if p.Auth != nil {
		req.Auth = string(*p.Auth)
	}
	if upstreamAuth != "" {
		req.Auth = upstreamAuth
	}
	if p.Stream != nil {
		req.Stream = string(*p.Stream)
	}
	if p.Headers != nil {
		req.Headers = string(*p.Headers)
	}
	return req
}

func (s *Server) ProxyOpds(ctx context.Context, r api.ProxyOpdsRequestObject) (api.ProxyOpdsResponseObject, error) {
	c := ginCtx(ctx)
	resp, err := s.Proxy.OPDS(rc(ctx), opdsProxyRequest(c.Request.URL.RawQuery, r.Params, c.GetHeader("X-Upstream-Authorization")))
	if err != nil {
		return nil, err
	}
	return passthrough{resp}, nil
}

func (s *Server) ProxyOpdsHead(ctx context.Context, r api.ProxyOpdsHeadRequestObject) (api.ProxyOpdsHeadResponseObject, error) {
	c := ginCtx(ctx)
	p := api.ProxyOpdsParams{Url: r.Params.Url, Auth: r.Params.Auth, Headers: r.Params.Headers}
	if r.Params.Stream != nil {
		p.Stream = new(api.ProxyOpdsParamsStream(*r.Params.Stream))
	}
	req := opdsProxyRequest(c.Request.URL.RawQuery, p, c.GetHeader("X-Upstream-Authorization"))
	req.Head = true
	resp, err := s.Proxy.OPDS(rc(ctx), req)
	if err != nil {
		return nil, err
	}
	return passthrough{resp}, nil
}

func (s *Server) ProxyKosync(ctx context.Context, r api.ProxyKosyncRequestObject) (api.ProxyKosyncResponseObject, error) {
	b := r.Body
	resp, err := s.Proxy.Kosync(rc(ctx), b.ServerUrl, b.Endpoint, string(b.Method), b.Headers, b.Body)
	if err != nil {
		return nil, err
	}
	return passthrough{resp}, nil
}

func (s *Server) ProxyBookorbit(ctx context.Context, r api.ProxyBookorbitRequestObject) (api.ProxyBookorbitResponseObject, error) {
	b := r.Body
	resp, err := s.Proxy.BookOrbit(rc(ctx), b.ServerUrl, b.Endpoint, string(b.Method), b.Headers, b.Body)
	if err != nil {
		return nil, err
	}
	return passthrough{resp}, nil
}

func (s *Server) ProxyGrimmory(ctx context.Context, r api.ProxyGrimmoryRequestObject) (api.ProxyGrimmoryResponseObject, error) {
	b := r.Body
	resp, err := s.Proxy.Grimmory(rc(ctx), b.ServerUrl, b.Path, string(b.Method), b.Headers, b.Body)
	if err != nil {
		return nil, err
	}
	return passthrough{resp}, nil
}

func (s *Server) ProxyHardcover(ctx context.Context, r api.ProxyHardcoverRequestObject) (api.ProxyHardcoverResponseObject, error) {
	var body map[string]any
	if r.Body != nil {
		body = *r.Body
	}
	resp, err := s.Proxy.Hardcover(rc(ctx), r.Params.XUpstreamAuthorization, body)
	if err != nil {
		return nil, err
	}
	return passthrough{resp}, nil
}

func (s *Server) notion(ctx context.Context, path []string, token string, body *map[string]any) (passthrough, error) {
	var b map[string]any
	if body != nil {
		b = *body
	}
	c := ginCtx(ctx)
	resp, err := s.Proxy.Notion(rc(ctx), c.Request.Method, path, token, c.Request.URL.Query(), b)
	if err != nil {
		return passthrough{}, err
	}
	return passthrough{resp}, nil
}

func (s *Server) ProxyNotion1Post(ctx context.Context, r api.ProxyNotion1PostRequestObject) (api.ProxyNotion1PostResponseObject, error) {
	return s.notion(ctx, []string{r.Collection}, r.Params.XNotionToken, (*map[string]any)(r.Body))
}

func (s *Server) ProxyNotion2Get(ctx context.Context, r api.ProxyNotion2GetRequestObject) (api.ProxyNotion2GetResponseObject, error) {
	return s.notion(ctx, []string{r.Collection, r.Id}, r.Params.XNotionToken, nil)
}

func (s *Server) ProxyNotion2Patch(ctx context.Context, r api.ProxyNotion2PatchRequestObject) (api.ProxyNotion2PatchResponseObject, error) {
	return s.notion(ctx, []string{r.Collection, r.Id}, r.Params.XNotionToken, (*map[string]any)(r.Body))
}

func (s *Server) ProxyNotion2Delete(ctx context.Context, r api.ProxyNotion2DeleteRequestObject) (api.ProxyNotion2DeleteResponseObject, error) {
	return s.notion(ctx, []string{r.Collection, r.Id}, r.Params.XNotionToken, nil)
}

func (s *Server) ProxyNotion3Get(ctx context.Context, r api.ProxyNotion3GetRequestObject) (api.ProxyNotion3GetResponseObject, error) {
	return s.notion(ctx, []string{r.Collection, r.Id, r.Action}, r.Params.XNotionToken, nil)
}

func (s *Server) ProxyNotion3Post(ctx context.Context, r api.ProxyNotion3PostRequestObject) (api.ProxyNotion3PostResponseObject, error) {
	return s.notion(ctx, []string{r.Collection, r.Id, r.Action}, r.Params.XNotionToken, (*map[string]any)(r.Body))
}

func (s *Server) ProxyNotion3Patch(ctx context.Context, r api.ProxyNotion3PatchRequestObject) (api.ProxyNotion3PatchResponseObject, error) {
	return s.notion(ctx, []string{r.Collection, r.Id, r.Action}, r.Params.XNotionToken, (*map[string]any)(r.Body))
}
