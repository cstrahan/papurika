package papurika

import (
	"errors"
	"fmt"
)

// Sentinel errors for the request-level failure taxonomy. These are returned
// (wrapped) from Client methods and are matchable with errors.Is. Per-minion and
// per-command outcomes are NOT errors — they are reported via MinionResult.Status
// and Retcode on the Result.
var (
	// ErrAuthFailure: authentication failed (bad credentials, expired/invalid
	// token). Triggers a single automatic re-auth + retry in the transport.
	ErrAuthFailure = errors.New("authentication failed")
	// ErrAuthDenied: authenticated but not authorized (valid token, missing perm).
	// On cherrypy this is a 401; on tornado it is a 200 with "Failed to
	// authenticate" in the body. Never retried — re-login cannot grant a perm.
	ErrAuthDenied = errors.New("authorization denied")
	// ErrNotFound: a requested resource does not exist.
	ErrNotFound = errors.New("not found")
	// ErrJobNotFound: a jid is unknown or aged out of the job cache (the explicit
	// Error field on GET /jobs/<jid>).
	ErrJobNotFound = errors.New("job not found")
	// ErrMalformed: the request was rejected as malformed (400 bad JSON, or
	// tornado's 200-with-an-exception-string). papurika validates the lowstate
	// before sending, so salt's own "missing fun/tgt" case cannot reach the wire.
	ErrMalformed = errors.New("malformed request")
	// ErrServerError: the server failed to handle an otherwise well-formed
	// request (HTTP 500). cherrypy answers a malformed lowstate with the same
	// opaque 500 as an internal failure ("An unexpected error occurred" either
	// way — cherrypy_learnings.md), so the two are indistinguishable from the
	// response alone. papurika closes that gap from the other side: it validates
	// the lowstate client-side, so by the time a 500 comes back the request is
	// known to have carried the fields salt requires, and the fault is the
	// server's. Ambiguous for retry purposes — the job may or may not have
	// started.
	ErrServerError = errors.New("server error")
	// ErrNoResponse: one or more targeted minions did not respond. Only returned
	// when the caller opts into strict no-response-is-an-error handling; by default
	// non-responders are reported via Result, not as an error.
	ErrNoResponse = errors.New("minion did not respond")
	// ErrTransport: a network/TLS/timeout failure, or a context cancellation. Wraps
	// the underlying cause (e.g. context.DeadlineExceeded).
	ErrTransport = errors.New("transport error")
	// ErrUnsupported: the requested operation is not supported by the target netapi
	// module (e.g. wheel/local_batch on rest_tornado).
	ErrUnsupported = errors.New("unsupported by netapi module")
)

// Module identifies the salt-api netapi module a response came from (best-effort,
// from the response shape / Server header).
type Module int

const (
	ModuleUnknown Module = iota
	ModuleCherryPy
	ModuleTornado
)

func (m Module) String() string {
	switch m {
	case ModuleCherryPy:
		return "rest_cherrypy"
	case ModuleTornado:
		return "rest_tornado"
	default:
		return "unknown"
	}
}

// APIError carries HTTP context for a request-level failure. It wraps one of the
// sentinel errors above, so both errors.Is(err, ErrAuthDenied) and
// errors.As(err, &apiErr) work.
type APIError struct {
	Kind        error  // one of the sentinels
	StatusCode  int    // HTTP status (0 if the request never completed)
	ContentType string // response Content-Type (may be text/html on errors)
	Body        string // raw response body — for diagnostics, never trusted as a result
	Module      Module
}

func (e *APIError) Error() string {
	kind := "error"
	if e.Kind != nil {
		kind = e.Kind.Error()
	}
	if e.StatusCode != 0 {
		return fmt.Sprintf("%s (HTTP %d)", kind, e.StatusCode)
	}
	return kind
}

func (e *APIError) Unwrap() error { return e.Kind }

// apiError builds an *APIError wrapping kind.
func apiError(kind error, status int, contentType, body string, mod Module) *APIError {
	return &APIError{Kind: kind, StatusCode: status, ContentType: contentType, Body: body, Module: mod}
}
