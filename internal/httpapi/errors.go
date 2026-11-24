package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"bookroom/internal/api"
	"bookroom/internal/apperr"
	"bookroom/internal/kosync"
)

// family is the wire dialect of a route.
type family int

const (
	native family = iota
	readestSync
	readestReplica
	gotrue
	kosyncFam
	opdsFam
	proxyFam
)

func familyOf(path string) family {
	switch {
	case strings.HasPrefix(path, "/api/sync/replica"):
		return readestReplica
	case path == "/api/sync" || strings.HasPrefix(path, "/api/storage"):
		return readestSync
	case strings.HasPrefix(path, "/auth/v1/"):
		return gotrue
	case strings.HasPrefix(path, "/kosync/"):
		return kosyncFam
	case path == "/opds" || strings.HasPrefix(path, "/opds/"):
		return opdsFam
	case strings.HasPrefix(path, "/api/") && !strings.HasPrefix(path, "/api/v1/"):
		return proxyFam
	}
	return native
}

// renderError writes err in the dialect of the route. Unexpected errors are logged and hidden.
func (s *Server) renderError(c *gin.Context, err error) {
	if c.Writer.Written() {
		s.Log.Warn("error after response started", "path", c.Request.URL.Path, "err", err)
		return
	}
	ae, ok := apperr.As(err)
	if !ok {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			ae = apperr.TooLarge("request body too large")
		} else {
			s.Log.Error("request failed", "method", c.Request.Method, "path", c.Request.URL.Path, "err", err)
			ae = apperr.New(http.StatusInternalServerError, "internal", "internal server error")
		}
	} else if ae.Status >= 500 && ae.Err != nil {
		s.Log.Error("request failed", "method", c.Request.Method, "path", c.Request.URL.Path, "err", err)
	}
	for k, vs := range ae.Header {
		for _, v := range vs {
			c.Writer.Header().Add(k, v)
		}
	}
	if ae.Body != nil {
		c.JSON(ae.Status, ae.Body)
		return
	}
	switch familyOf(c.Request.URL.Path) {
	case readestSync:
		status := ae.Status
		if status == http.StatusUnauthorized {
			status = http.StatusForbidden // Readest answers 403 {"error": "Not authenticated"}
		}
		c.JSON(status, api.ReadestError{Error: ae.Detail})
	case proxyFam:
		c.JSON(ae.Status, api.ReadestError{Error: ae.Detail})
	case readestReplica:
		code := api.ReplicaErrorCode("SERVER")
		switch ae.Status {
		case http.StatusUnauthorized, http.StatusForbidden:
			code = "AUTH"
		case http.StatusBadRequest, http.StatusRequestEntityTooLarge:
			code = "VALIDATION"
		}
		c.JSON(ae.Status, api.ReplicaError{Error: ae.Detail, Code: code})
	case gotrue:
		code := "server_error"
		switch ae.Status {
		case http.StatusBadRequest:
			code = "invalid_request"
		case http.StatusUnauthorized:
			code = "invalid_grant"
		}
		c.JSON(ae.Status, api.GoTrueError{Error: code, ErrorDescription: ae.Detail, Msg: &ae.Detail, Code: &ae.Status})
	case kosyncFam:
		code := kosync.CodeInvalidRequest
		if ae.Status == http.StatusUnauthorized {
			code = kosync.CodeUnauthorized
		}
		c.JSON(ae.Status, api.KosyncError{Code: code, Message: ae.Detail})
	case opdsFam:
		if ae.Status == http.StatusUnauthorized {
			c.Header("WWW-Authenticate", `Basic realm="BookRoom", charset="UTF-8"`)
		}
		c.Data(ae.Status, "text/plain; charset=utf-8", []byte(ae.Detail))
	default:
		c.Header("Content-Type", "application/problem+json")
		c.JSON(ae.Status, api.Problem{
			Type: "/problems/" + ae.Code, Title: http.StatusText(ae.Status), Status: ae.Status,
			Detail: &ae.Detail, Instance: new(c.Request.URL.Path),
		})
	}
}
