// Package apperr is the error type services return. The HTTP layer renders it in the wire format of the
// route family: RFC 9457 problem+json for the native API, `{"error": ...}` for the Readest contract,
// `{"code", "message"}` for KOSync. A service that must answer in a protocol-specific shape sets Body.
package apperr

import (
	"errors"
	"fmt"
	"net/http"
)

// Error is an expected failure with an HTTP status.
type Error struct {
	Status int    // HTTP status
	Code   string // stable slug, e.g. "not-found"; the problem type is /problems/<Code>
	Detail string // human-readable explanation
	Body   any    // when set, rendered verbatim as the JSON body
	Header http.Header
	Err    error // wrapped cause, never shown to clients
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Detail, e.Err)
	}
	return e.Code + ": " + e.Detail
}

func (e *Error) Unwrap() error { return e.Err }

// New builds an Error.
func New(status int, code, detail string) *Error {
	return &Error{Status: status, Code: code, Detail: detail}
}

// WithBody builds an Error rendered as the given JSON body.
func WithBody(status int, body any) *Error {
	return &Error{Status: status, Code: http.StatusText(status), Detail: fmt.Sprint(body), Body: body}
}

func BadRequest(detail string) *Error   { return New(http.StatusBadRequest, "bad-request", detail) }
func Unauthorized(detail string) *Error { return New(http.StatusUnauthorized, "unauthorized", detail) }
func Forbidden(detail string) *Error    { return New(http.StatusForbidden, "forbidden", detail) }
func NotFound(detail string) *Error     { return New(http.StatusNotFound, "not-found", detail) }
func Conflict(detail string) *Error     { return New(http.StatusConflict, "conflict", detail) }
func TooLarge(detail string) *Error {
	return New(http.StatusRequestEntityTooLarge, "too-large", detail)
}
func Unsupported(detail string) *Error {
	return New(http.StatusUnsupportedMediaType, "unsupported-format", detail)
}
func Upstream(detail string, err error) *Error {
	return &Error{Status: http.StatusBadGateway, Code: "upstream", Detail: detail, Err: err}
}

// As extracts an *Error from err.
func As(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}
