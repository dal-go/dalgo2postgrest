package dalgo2postgrest

import (
	"errors"
	"fmt"
)

var (
	// ErrInvalidConfig reports a configuration that cannot be used. Its message
	// never contains the endpoint or a header value.
	ErrInvalidConfig = errors.New("postgrest: invalid configuration")

	// ErrUnsafeHeader reports a header provider result containing a CR/LF, an
	// invalid field name, a hop-by-hop header or a header this adapter owns.
	ErrUnsafeHeader = errors.New("postgrest: unsafe configured header")

	// ErrRedirectRefused reports a 3xx response. Redirects are never followed.
	ErrRedirectRefused = errors.New("postgrest: redirect refused")

	// ErrRequestFailed reports a failure to obtain headers or to complete the
	// HTTP exchange. The underlying error is deliberately not wrapped: it can
	// contain a URL, credentials or a response body.
	ErrRequestFailed = errors.New("postgrest: request could not be completed")

	// ErrInvalidResponse reports a body that is not one complete JSON array of
	// row objects, or a body that was truncated.
	ErrInvalidResponse = errors.New("postgrest: invalid response")

	// ErrResponseTooLarge reports a response above the configured byte budget.
	ErrResponseTooLarge = errors.New("postgrest: response exceeds byte budget")

	// ErrBudgetExceeded reports a query whose pages, rows or offset do not fit
	// the configured budgets. It is returned before any request is sent.
	ErrBudgetExceeded = errors.New("postgrest: query exceeds configured budget")

	// ErrUnauthorized reports HTTP 401 or 403, or metadata the active role may
	// not see.
	ErrUnauthorized = errors.New("postgrest: authentication or authorization failed")

	// ErrClosed reports use of a handle after Close.
	ErrClosed = errors.New("postgrest: database is closed")

	// ErrMetadataUnavailable reports a missing, malformed or non-OpenAPI root
	// document. It is never used for a valid OpenAPI document that exposes no
	// relations to the active role.
	ErrMetadataUnavailable = errors.New("postgrest: schema metadata unavailable")

	// ErrMetadataUnauthorized reports a root document that the active role may
	// not read.
	ErrMetadataUnauthorized = errors.New("postgrest: schema metadata not authorized")

	// ErrRelationNotFound reports a relation absent from the permitted OpenAPI
	// document.
	ErrRelationNotFound = errors.New("postgrest: relation not found in schema metadata")
)

// HTTPError is a non-2xx PostgREST response. It carries only the status code:
// PostgREST error bodies may contain row values, SQL and role names.
type HTTPError struct {
	StatusCode int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("postgrest: request failed with HTTP %d", e.StatusCode)
}

// Is lets callers test 401/403 with errors.Is(err, ErrUnauthorized) without
// inspecting the numeric status.
func (e *HTTPError) Is(target error) bool {
	return target == ErrUnauthorized && (e.StatusCode == 401 || e.StatusCode == 403)
}

// MetadataError reports a schema-metadata failure. StatusCode is zero when the
// root was reached but its document was unusable. The error wraps either
// ErrMetadataUnavailable or ErrMetadataUnauthorized; it never carries a body.
type MetadataError struct {
	Op         string
	StatusCode int
	Reason     string
	Err        error
}

func (e *MetadataError) Error() string {
	state := "unavailable"
	if errors.Is(e.Err, ErrMetadataUnauthorized) {
		state = "not authorized"
	}
	message := "postgrest: " + e.Op + " metadata " + state
	if e.StatusCode != 0 {
		message += fmt.Sprintf(" (HTTP %d)", e.StatusCode)
	}
	if e.Reason != "" {
		message += ": " + e.Reason
	}
	return message
}

func (e *MetadataError) Unwrap() error {
	return e.Err
}
