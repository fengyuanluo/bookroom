package httpapi

import (
	"io"
	"mime"
	"net/http"
	"os"
	"strconv"

	"bookroom/internal/library"
	"bookroom/internal/opds"
	"bookroom/internal/proxy"
)

// fileResponse serves a file with Range, conditional requests and HEAD via http.ServeContent. It satisfies
// every generated response interface that returns file bytes.
type fileResponse struct {
	req        *http.Request
	file       library.File
	etag       string
	attachment bool
	cache      string
}

func (f fileResponse) write(w http.ResponseWriter) error {
	fh, err := os.Open(f.file.Path)
	if err != nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return nil
	}
	defer fh.Close()
	h := w.Header()
	h.Set("Content-Type", f.file.MIME)
	if f.etag != "" {
		h.Set("ETag", `"`+f.etag+`"`)
	}
	if f.cache != "" {
		h.Set("Cache-Control", f.cache)
	}
	if f.attachment {
		h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": f.file.Name}))
	}
	http.ServeContent(w, f.req, f.file.Name, f.file.Modified, fh)
	return nil
}

func (f fileResponse) VisitDownloadBookResponse(w http.ResponseWriter) error     { return f.write(w) }
func (f fileResponse) VisitGetBookCoverResponse(w http.ResponseWriter) error     { return f.write(w) }
func (f fileResponse) VisitOpdsDownloadResponse(w http.ResponseWriter) error     { return f.write(w) }
func (f fileResponse) VisitOpdsCoverResponse(w http.ResponseWriter) error        { return f.write(w) }
func (f fileResponse) VisitStorageGetObjectResponse(w http.ResponseWriter) error { return f.write(w) }

// feedResponse writes an OPDS document with its exact media type (profile and kind parameters included).
type feedResponse struct{ opds.Feed }

func (f feedResponse) write(w http.ResponseWriter) error {
	w.Header().Set("Content-Type", f.ContentType+";charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(f.Body)))
	w.WriteHeader(http.StatusOK)
	_, err := w.Write(f.Body)
	return err
}

func (f feedResponse) VisitOpdsRootResponse(w http.ResponseWriter) error       { return f.write(w) }
func (f feedResponse) VisitOpdsBooksResponse(w http.ResponseWriter) error      { return f.write(w) }
func (f feedResponse) VisitOpdsShelfResponse(w http.ResponseWriter) error      { return f.write(w) }
func (f feedResponse) VisitOpdsTagsResponse(w http.ResponseWriter) error       { return f.write(w) }
func (f feedResponse) VisitOpdsSearchResponse(w http.ResponseWriter) error     { return f.write(w) }
func (f feedResponse) VisitOpdsOpenSearchResponse(w http.ResponseWriter) error { return f.write(w) }

// passthrough streams an upstream answer from the proxies.
type passthrough struct{ *proxy.Response }

func (p passthrough) write(w http.ResponseWriter) error {
	defer p.Body.Close()
	for k, vs := range p.Header {
		w.Header()[k] = vs
	}
	w.WriteHeader(p.Status)
	_, err := io.Copy(w, p.Body)
	return err
}

func (p passthrough) VisitProxyOpdsResponse(w http.ResponseWriter) error         { return p.write(w) }
func (p passthrough) VisitProxyOpdsHeadResponse(w http.ResponseWriter) error     { return p.write(w) }
func (p passthrough) VisitProxyKosyncResponse(w http.ResponseWriter) error       { return p.write(w) }
func (p passthrough) VisitProxyBookorbitResponse(w http.ResponseWriter) error    { return p.write(w) }
func (p passthrough) VisitProxyGrimmoryResponse(w http.ResponseWriter) error     { return p.write(w) }
func (p passthrough) VisitProxyHardcoverResponse(w http.ResponseWriter) error    { return p.write(w) }
func (p passthrough) VisitProxyNotion1PostResponse(w http.ResponseWriter) error  { return p.write(w) }
func (p passthrough) VisitProxyNotion2GetResponse(w http.ResponseWriter) error   { return p.write(w) }
func (p passthrough) VisitProxyNotion2PatchResponse(w http.ResponseWriter) error { return p.write(w) }
func (p passthrough) VisitProxyNotion2DeleteResponse(w http.ResponseWriter) error {
	return p.write(w)
}
func (p passthrough) VisitProxyNotion3GetResponse(w http.ResponseWriter) error   { return p.write(w) }
func (p passthrough) VisitProxyNotion3PostResponse(w http.ResponseWriter) error  { return p.write(w) }
func (p passthrough) VisitProxyNotion3PatchResponse(w http.ResponseWriter) error { return p.write(w) }
