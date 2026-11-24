package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexedwards/argon2id"
	"github.com/stretchr/testify/require"

	"bookroom/internal/auth"
	"bookroom/internal/config"
	"bookroom/internal/httpapi"
	"bookroom/internal/kosync"
	"bookroom/internal/library"
	"bookroom/internal/opds"
	"bookroom/internal/proxy"
	"bookroom/internal/readest"
	"bookroom/internal/scrape"
	"bookroom/internal/stats"
	"bookroom/internal/store"
)

func init() {
	// Fast hashes keep the suite quick; production uses argon2id.DefaultParams.
	auth.HashParams = &argon2id.Params{Memory: 1024, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}
}

type env struct {
	t       *testing.T
	srv     *httptest.Server
	dataDir string
	deps    httpapi.Deps
	scrape  *scrape.Service
	admin   string // admin access token
}

type envOptions struct {
	providers []scrape.Provider
	proxy     *proxy.Proxy
}

func newEnv(t *testing.T, opts ...envOptions) *env {
	t.Helper()
	var o envOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	dir := t.TempDir()
	cfg := &config.Config{DataDir: dir, JWTSecret: strings.Repeat("s", 40), AccessTTL: time.Hour, RefreshTTL: time.Hour,
		MaxUpload: 64 << 20, CORSOrigins: []string{"*"}}
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dir, "bookroom.db"))
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	lib, err := library.New(st, dir, cfg.MaxUpload, true)
	require.NoError(t, err)
	storage, err := readest.NewStorage(st, lib, dir, cfg.JWTSecret)
	require.NoError(t, err)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sc := scrape.New(st, lib, time.Millisecond, log, o.providers...)
	lib.OnScrapeQueued = sc.Wake
	px := o.proxy
	if px == nil {
		px = proxy.New(false, nil)
	}
	deps := httpapi.Deps{
		Cfg: cfg, Store: st, Auth: auth.New(st, cfg.JWTSecret, cfg.AccessTTL, cfg.RefreshTTL, library.ProjectInto), Lib: lib,
		Sync: readest.NewSync(st), Storage: storage, Replicas: readest.NewReplicas(st),
		Vocabulary: readest.NewVocabulary(st), Kosync: kosync.New(st),
		OPDS: &opds.Catalog{Lib: lib}, Stats: stats.New(st), Scrape: sc, Proxy: px, Log: log, Version: "test",
	}
	srv, err := httpapi.New(deps)
	require.NoError(t, err)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	runCtx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	go sc.Run(runCtx)
	e := &env{t: t, srv: ts, dataDir: dir, deps: deps, scrape: sc}
	_, err = deps.Auth.CreateUser(ctx, "admin", "adminpass", "admin@example.com", true)
	require.NoError(t, err)
	e.admin = e.login("admin", "adminpass")
	return e
}

func (e *env) login(user, pass string) string {
	e.t.Helper()
	var tp struct {
		AccessToken string `json:"accessToken"`
	}
	e.expect(e.call("POST", "/api/v1/auth/login", "", map[string]string{"username": user, "password": pass}), 200, &tp)
	return tp.AccessToken
}

// call sends a JSON request; token may be empty. Extra headers come as key, value pairs.
func (e *env) call(method, path, token string, body any, headers ...string) *http.Response {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		switch b := body.(type) {
		case []byte:
			rd = bytes.NewReader(b)
		case string:
			rd = strings.NewReader(b)
		default:
			data, err := json.Marshal(body)
			require.NoError(e.t, err)
			rd = bytes.NewReader(data)
		}
	}
	url := path
	if !strings.HasPrefix(path, "http") {
		url = e.srv.URL + path
	}
	req, err := http.NewRequest(method, url, rd)
	require.NoError(e.t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(e.t, err)
	return resp
}

// expect asserts the status and decodes JSON into v (when non-nil); it returns the raw body.
func (e *env) expect(resp *http.Response, status int, v any) []byte {
	e.t.Helper()
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(e.t, err)
	require.Equal(e.t, status, resp.StatusCode, "%s %s: %s", resp.Request.Method, resp.Request.URL.Path, body)
	if v != nil {
		require.NoError(e.t, json.Unmarshal(body, v), string(body))
	}
	return body
}

// upload posts a file through the native multipart endpoint.
func (e *env) upload(token, path string, fields ...string) *http.Response {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for i := 0; i+1 < len(fields); i += 2 {
		require.NoError(e.t, mw.WriteField(fields[i], fields[i+1]))
	}
	fw, err := mw.CreateFormFile("file", filepath.Base(path))
	require.NoError(e.t, err)
	data, err := os.ReadFile(path)
	require.NoError(e.t, err)
	_, _ = fw.Write(data)
	require.NoError(e.t, mw.Close())
	req, err := http.NewRequest("POST", e.srv.URL+"/api/v1/books", &buf)
	require.NoError(e.t, err)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(e.t, err)
	return resp
}

// user creates a non-admin account and returns its id and token.
func (e *env) user(name string) (string, string) {
	e.t.Helper()
	var u struct {
		ID string `json:"id"`
	}
	e.expect(e.call("POST", "/api/v1/users", e.admin, map[string]any{"username": name, "password": name + "pass1"}), 201, &u)
	return u.ID, e.login(name, name+"pass1")
}

// newServer starts another server on the env's services (after tests change e.deps.Cfg) and returns its URL.
func newServer(t *testing.T, e *env) string {
	t.Helper()
	srv, err := httpapi.New(e.deps)
	require.NoError(t, err)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}
