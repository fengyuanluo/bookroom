// Package httpapi implements the generated api.StrictServerInterface: thin handlers that translate between the
// OpenAPI types and the services, plus the Gin engine, spec-driven authentication and error rendering.
package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/gin-contrib/cors"
	"github.com/gin-contrib/requestid"
	"github.com/gin-gonic/gin"
	ginmiddleware "github.com/oapi-codegen/gin-middleware"
	sloggin "github.com/samber/slog-gin"

	"bookroom/internal/api"
	"bookroom/internal/apperr"
	"bookroom/internal/auth"
	"bookroom/internal/config"
	"bookroom/internal/kosync"
	"bookroom/internal/library"
	"bookroom/internal/opds"
	"bookroom/internal/proxy"
	"bookroom/internal/readest"
	"bookroom/internal/scrape"
	"bookroom/internal/stats"
	"bookroom/internal/store"
)

// Deps are the services the handlers call.
type Deps struct {
	Cfg      *config.Config
	Store    *store.Store
	Auth     *auth.Service
	Lib      *library.Service
	Sync     *readest.Sync
	Storage  *readest.Storage
	Replicas *readest.Replicas
	Kosync   *kosync.Service
	OPDS     *opds.Catalog
	Stats    *stats.Service
	Scrape   *scrape.Service
	Proxy    *proxy.Proxy
	Log      *slog.Logger
	Version  string
}

// Server implements api.StrictServerInterface.
type Server struct {
	Deps
	spec     *openapi3.T
	security map[string]openapi3.SecurityRequirements // "METHOD /gin/:path" -> requirements
}

var _ api.StrictServerInterface = (*Server)(nil)

const userKey = "bookroom.user"

// New loads the embedded spec and indexes its security requirements.
func New(d Deps) (*Server, error) {
	spec, err := api.GetSwagger()
	if err != nil {
		return nil, err
	}
	spec.Servers = nil
	s := &Server{Deps: d, spec: spec, security: map[string]openapi3.SecurityRequirements{}}
	for path, item := range spec.Paths.Map() {
		ginPath := toGinPath(path)
		for method, op := range item.Operations() {
			reqs := spec.Security
			if op.Security != nil {
				reqs = *op.Security
			}
			s.security[method+" "+ginPath] = reqs
		}
	}
	return s, nil
}

func toGinPath(p string) string {
	var b strings.Builder
	for _, seg := range strings.Split(p, "/") {
		if seg == "" {
			continue
		}
		b.WriteByte('/')
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			b.WriteString(":" + seg[1:len(seg)-1])
		} else {
			b.WriteString(seg)
		}
	}
	if b.Len() == 0 {
		return "/"
	}
	return b.String()
}

// Handler builds the Gin engine.
func (s *Server) Handler() http.Handler {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(s.recovery, requestid.New(), sloggin.NewWithConfig(s.Log, sloggin.Config{
		WithRequestID: true, DefaultLevel: slog.LevelInfo, ClientErrorLevel: slog.LevelWarn, ServerErrorLevel: slog.LevelError,
		Filters: []sloggin.Filter{sloggin.IgnorePath("/healthz")},
	}))
	r.Use(cors.New(s.corsConfig()), s.limitBody, s.validator())
	strict := api.NewStrictHandlerWithOptions(s, nil, api.StrictGinServerOptions{
		RequestErrorHandlerFunc: func(c *gin.Context, err error) { s.renderError(c, apperr.BadRequest(err.Error())) },
		HandlerErrorFunc:        s.renderError,
		ResponseErrorHandlerFunc: func(c *gin.Context, err error) {
			s.Log.Warn("response write failed", "path", c.Request.URL.Path, "err", err)
		},
	})
	api.RegisterHandlersWithOptions(r, strict, api.GinServerOptions{
		Middlewares: []api.MiddlewareFunc{s.authenticate},
		ErrorHandler: func(c *gin.Context, err error, status int) {
			s.renderError(c, apperr.New(status, "bad-request", err.Error()))
		},
	})
	if s.Cfg.WebDir != "" {
		s.serveWeb(r)
	}
	return r
}

func (s *Server) corsConfig() cors.Config {
	c := cors.Config{
		AllowMethods: []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"},
		AllowHeaders: []string{"Authorization", "Content-Type", "Accept", "Range", "apikey", "x-auth-user", "x-auth-key",
			"x-notion-token", "X-Upstream-Authorization", "X-Request-ID"},
		ExposeHeaders: []string{"Content-Length", "Content-Range", "Content-Disposition", "ETag", "X-Content-Length",
			"Retry-After", "RateLimit", "RateLimit-Policy", "X-Request-ID"},
		MaxAge: 12 * time.Hour,
	}
	if slices.Contains(s.Cfg.CORSOrigins, "*") || len(s.Cfg.CORSOrigins) == 0 {
		c.AllowAllOrigins = true
	} else {
		c.AllowOrigins = s.Cfg.CORSOrigins
	}
	return c
}

// limitBody bounds request bodies: uploads up to the configured maximum, everything else to 32 MiB
// (Readest pushes up to ~8 MB of books in one sync).
func (s *Server) limitBody(c *gin.Context) {
	if c.Request.Body != nil {
		limit := int64(32 << 20)
		if strings.HasPrefix(c.Request.URL.Path, "/api/storage/objects/") || (c.Request.Method == http.MethodPost && c.Request.URL.Path == "/api/v1/books") {
			limit = s.Cfg.MaxUpload + 1<<20
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
	}
	c.Next()
}

// validator checks native API requests against the spec (types, enums, lengths, patterns), so agents get
// precise 400s. Multipart uploads are excluded: validation would buffer the whole file.
func (s *Server) validator() gin.HandlerFunc {
	v := ginmiddleware.OapiRequestValidatorWithOptions(s.spec, &ginmiddleware.Options{
		SilenceServersWarning: true,
		// Handlers decode the raw body themselves: re-binding the validated value would turn an explicit JSON
		// null into an absent member (BookStateUpdate distinguishes the two).
		DiscardValidatedValue: true,
		Options: openapi3filter.Options{
			AuthenticationFunc: func(context.Context, *openapi3filter.AuthenticationInput) error { return nil },
			MultiError:         false,
		},
		ErrorHandler: func(c *gin.Context, message string, status int) {
			message = strings.TrimPrefix(message, "error in openapi3filter.RequestError: ")
			s.renderError(c, apperr.New(status, "invalid-request", message))
			c.Abort()
		},
	})
	return func(c *gin.Context) {
		p := c.Request.URL.Path
		if !strings.HasPrefix(p, "/api/v1/") || strings.HasPrefix(c.ContentType(), "multipart/") {
			c.Next()
			return
		}
		v(c)
	}
}

func (s *Server) recovery(c *gin.Context) {
	defer func() {
		if r := recover(); r != nil {
			if err, ok := r.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(r)
			}
			s.Log.Error("panic", "path", c.Request.URL.Path, "panic", r)
			s.renderError(c, errors.New("internal error"))
			c.Abort()
		}
	}()
	c.Next()
}

// authenticate enforces the operation's `security` from the spec: alternatives are OR-ed, schemes inside one
// requirement AND-ed, and the bearerAuth scope "admin" requires an admin.
func (s *Server) authenticate(c *gin.Context) {
	reqs := s.security[c.Request.Method+" "+c.FullPath()]
	if len(reqs) == 0 {
		return
	}
	var lastErr error
	for _, req := range reqs {
		if len(req) == 0 {
			return
		}
		u, err := s.satisfy(c, req)
		if err == nil {
			c.Set(userKey, u)
			return
		}
		lastErr = err
	}
	s.renderError(c, lastErr)
	c.Abort()
}

func (s *Server) satisfy(c *gin.Context, req openapi3.SecurityRequirement) (auth.User, error) {
	var user auth.User
	ctx := c.Request.Context()
	for scheme, scopes := range req {
		var u auth.User
		var err error
		switch scheme {
		case "bearerAuth":
			token, ok := strings.CutPrefix(c.GetHeader("Authorization"), "Bearer ")
			if !ok {
				token, ok = strings.CutPrefix(c.GetHeader("Authorization"), "bearer ")
			}
			if !ok || token == "" {
				return user, apperr.Unauthorized("Not authenticated")
			}
			u, err = s.Auth.VerifyAccess(ctx, strings.TrimSpace(token))
		case "accessTokenQuery":
			if c.Query("access_token") == "" {
				return user, apperr.Unauthorized("Not authenticated")
			}
			u, err = s.Auth.VerifyAccess(ctx, c.Query("access_token"))
		case "kosyncKey":
			if c.GetHeader("x-auth-key") == "" {
				return user, apperr.Unauthorized("Unauthorized")
			}
			u, err = s.Auth.UserByDeviceKeyMD5(ctx, c.GetHeader("x-auth-key"))
		case "kosyncUser":
			if name := c.GetHeader("x-auth-user"); name == "" || strings.Contains(name, ":") {
				return user, apperr.Unauthorized("Unauthorized")
			}
			continue
		case "basicAuth":
			name, pass, ok := c.Request.BasicAuth()
			if !ok {
				return user, apperr.Unauthorized("credentials required")
			}
			u, err = s.Auth.Basic(ctx, name, pass)
		default:
			return user, apperr.Unauthorized("unsupported security scheme " + scheme)
		}
		if err != nil {
			return user, err
		}
		if slices.Contains(scopes, "admin") && !u.IsAdmin {
			return user, apperr.Forbidden("admin only")
		}
		if user.ID != "" && user.ID != u.ID {
			return user, apperr.Unauthorized("credentials disagree")
		}
		user = u
	}
	if user.ID == "" {
		return user, apperr.Unauthorized("Not authenticated")
	}
	return user, nil
}

// currentUser is the authenticated user of a request (set by authenticate).
func currentUser(ctx context.Context) auth.User {
	if c, ok := ctx.(*gin.Context); ok {
		if v, ok := c.Get(userKey); ok {
			return v.(auth.User)
		}
	}
	return auth.User{}
}

// rc is the request's own context for service calls. *gin.Context is pooled and reused once the handler
// returns, so it must never escape into database/sql or HTTP clients that may still hold it.
func rc(ctx context.Context) context.Context {
	if c, ok := ctx.(*gin.Context); ok {
		return c.Request.Context()
	}
	return ctx
}

func ginCtx(ctx context.Context) *gin.Context {
	c, _ := ctx.(*gin.Context)
	return c
}

// baseURL is the external origin for absolute links: BOOKROOM_PUBLIC_URL, else the request's scheme and host
// (honouring X-Forwarded-Proto/Host from a reverse proxy).
func (s *Server) baseURL(ctx context.Context) string {
	if s.Cfg.PublicURL != "" {
		return s.Cfg.PublicURL
	}
	c := ginCtx(ctx)
	if c == nil {
		return ""
	}
	scheme := "http"
	if c.Request.TLS != nil {
		scheme = "https"
	}
	if p := c.GetHeader("X-Forwarded-Proto"); p == "https" || p == "http" {
		scheme = p
	}
	host := c.Request.Host
	if h := c.GetHeader("X-Forwarded-Host"); h != "" && !strings.ContainsAny(h, "/\\ ") {
		host = h
	}
	return scheme + "://" + host
}

// serveWeb serves a static Readest web build with an index.html fallback for client-side routes.
func (s *Server) serveWeb(r *gin.Engine) {
	root := s.Cfg.WebDir
	r.NoRoute(func(c *gin.Context) {
		p := c.Request.URL.Path
		if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
			s.renderError(c, apperr.NotFound("no such route"))
			return
		}
		for _, prefix := range []string{"/api/", "/opds", "/kosync/", "/auth/v1/"} {
			if strings.HasPrefix(p, prefix) {
				s.renderError(c, apperr.NotFound("no such route"))
				return
			}
		}
		clean := filepath.Join(root, filepath.FromSlash(filepath.Clean("/"+p)))
		if st, err := os.Stat(clean); err == nil && !st.IsDir() {
			c.File(clean)
			return
		}
		if st, err := os.Stat(filepath.Join(clean, "index.html")); err == nil && !st.IsDir() {
			c.File(filepath.Join(clean, "index.html"))
			return
		}
		c.File(filepath.Join(root, "index.html"))
	})
}
