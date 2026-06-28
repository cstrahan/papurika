package papurika

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"strings"
	"time"
)

// RetryPolicy controls automatic retry of safe (idempotent) requests.
type RetryPolicy struct {
	MaxAttempts int           // total attempts including the first (default 3)
	BaseBackoff time.Duration // default 200ms, exponential with full jitter
	MaxBackoff  time.Duration // default 5s
}

// DefaultRetryPolicy is applied when none is supplied.
var DefaultRetryPolicy = RetryPolicy{MaxAttempts: 3, BaseBackoff: 200 * time.Millisecond, MaxBackoff: 5 * time.Second}

func (p RetryPolicy) normalized() RetryPolicy {
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = DefaultRetryPolicy.MaxAttempts
	}
	if p.BaseBackoff <= 0 {
		p.BaseBackoff = DefaultRetryPolicy.BaseBackoff
	}
	if p.MaxBackoff <= 0 {
		p.MaxBackoff = DefaultRetryPolicy.MaxBackoff
	}
	return p
}

// buildHTTPClient constructs a pooled client with per-client TLS config (never
// the global default). caCert, if set, is a PEM file path to trust.
func buildHTTPClient(insecure bool, caCert string, timeout time.Duration) (*http.Client, error) {
	tlsCfg := &tls.Config{InsecureSkipVerify: insecure} //nolint:gosec // opt-in via --insecure
	if caCert != "" {
		pem, err := os.ReadFile(caCert)
		if err != nil {
			return nil, fmt.Errorf("papurika: reading ca_cert %s: %w", caCert, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("papurika: ca_cert %s: no certificates parsed", caCert)
		}
		tlsCfg.RootCAs = pool
	}
	return &http.Client{
		// A generous transport-level safety net. The authoritative wall-clock bound
		// is the caller's context (see async.md §B); we never rely on this Timeout
		// to cancel a long async gather.
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig:       tlsCfg,
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       90 * time.Second,
			ForceAttemptHTTP2:     true,
			ResponseHeaderTimeout: 0, // unbounded; ctx governs
		},
	}, nil
}

// httpResult is a completed HTTP exchange.
type httpResult struct {
	status      int
	contentType string
	body        []byte
	module      Module
}

// doRequest performs one HTTP request with retry of safe failures. retryable
// controls whether THIS request may be retried (callers pass false for
// non-idempotent command POSTs). It does not interpret salt-level semantics —
// that is classify()'s job.
func (c *Client) doRequest(ctx context.Context, method, path string, body []byte, token string, retryable bool) (*httpResult, error) {
	policy := c.retry.normalized()
	var lastErr error
	for attempt := 1; attempt <= policy.MaxAttempts; attempt++ {
		res, err := c.doOnce(ctx, method, path, body, token)
		if err == nil && !isRetryableStatus(res.status) {
			return res, nil
		}
		if err == nil {
			lastErr = apiError(ErrTransport, res.status, res.contentType, string(res.body), res.module)
		} else {
			if ctx.Err() != nil {
				return nil, apiError(ErrTransport, 0, "", "", ModuleUnknown)
			}
			lastErr = fmt.Errorf("%w: %v", ErrTransport, err)
		}
		if !retryable || attempt == policy.MaxAttempts {
			break
		}
		// Honor Retry-After on 429/503 if present, else backoff with jitter.
		wait := backoff(policy, attempt)
		if err == nil {
			if ra := retryAfter(res); ra > 0 {
				wait = ra
			}
		}
		select {
		case <-ctx.Done():
			return nil, apiError(ErrTransport, 0, "", "", ModuleUnknown)
		case <-time.After(wait):
		}
	}
	return nil, lastErr
}

func (c *Client) doOnce(ctx context.Context, method, path string, body []byte, token string) (*httpResult, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Requested-With", "XMLHttpRequest") // CSRF convention; avoids cookie-only auth
	if token != "" {
		req.Header.Set("X-Auth-Token", token)
	}

	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return &httpResult{
		status:      resp.StatusCode,
		contentType: resp.Header.Get("Content-Type"),
		body:        b,
		module:      detectModule(resp.Header.Get("Server"), resp.Header.Get("X-Salt-Version")),
	}, nil
}

// classify maps an HTTP result + body to a request-level error (or nil on
// success). It inspects status AND body and never assumes JSON on errors. A 200
// can still be a failure (tornado authz / malformed) — those body strings are
// detected here. See library.md §C.
func classify(r *httpResult) error {
	switch r.status {
	case http.StatusOK, http.StatusAccepted, http.StatusCreated:
		// HTTP 200 != success. Scan for the known tornado "fake 200" failures.
		if k := scanBodyError(r.body); k != nil {
			return apiError(k, r.status, r.contentType, string(r.body), r.module)
		}
		return nil
	case http.StatusUnauthorized:
		// Distinguish authz denial (don't retry) from authn failure (re-auth).
		if bodyContains(r.body, "Authorization error occurred") {
			return apiError(ErrAuthDenied, r.status, r.contentType, string(r.body), r.module)
		}
		return apiError(ErrAuthFailure, r.status, r.contentType, string(r.body), r.module)
	case http.StatusBadRequest:
		return apiError(ErrMalformed, r.status, r.contentType, string(r.body), r.module)
	case http.StatusNotFound:
		return apiError(ErrNotFound, r.status, r.contentType, string(r.body), r.module)
	case http.StatusNotAcceptable, http.StatusRequestEntityTooLarge:
		return apiError(ErrMalformed, r.status, r.contentType, string(r.body), r.module)
	case http.StatusInternalServerError, http.StatusNotImplemented:
		// cherrypy returns 500 for missing fun/tgt (a malformed lowstate).
		return apiError(ErrMalformed, r.status, r.contentType, string(r.body), r.module)
	default:
		return apiError(ErrTransport, r.status, r.contentType, string(r.body), r.module)
	}
}

// scanBodyError detects tornado's "fake 200" failures: authz denial and
// malformed-request exceptions surface as a 200 with a marker string in the
// return array. Returns nil if none.
func scanBodyError(body []byte) error {
	switch {
	case bodyContains(body, "Failed to authenticate"):
		return ErrAuthDenied
	case bodyContains(body, "Unexpected exception while handling request"):
		return ErrMalformed
	default:
		return nil
	}
}

func bodyContains(body []byte, s string) bool { return bytes.Contains(body, []byte(s)) }

func detectModule(server, saltVersion string) Module {
	switch {
	case strings.Contains(server, "CherryPy"):
		return ModuleCherryPy
	case strings.Contains(server, "TornadoServer"):
		return ModuleTornado
	default:
		return ModuleUnknown
	}
}

func isRetryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func retryAfter(r *httpResult) time.Duration {
	// httpResult does not carry headers; Retry-After handling is best-effort and
	// currently a no-op placeholder (backoff covers the common case). Kept as a
	// seam for when we thread response headers through.
	return 0
}

func backoff(p RetryPolicy, attempt int) time.Duration {
	d := p.BaseBackoff << (attempt - 1)
	if d > p.MaxBackoff {
		d = p.MaxBackoff
	}
	// full jitter
	return time.Duration(rand.Int64N(int64(d) + 1))
}

// asAPIError extracts an *APIError if present.
func asAPIError(err error) (*APIError, bool) {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}

// parseExpire converts salt's float seconds-since-epoch to a time.Time.
func parseExpire(sec float64) time.Time {
	if sec <= 0 {
		return time.Now().Add(time.Hour) // conservative default
	}
	whole, frac := int64(sec), sec-float64(int64(sec))
	return time.Unix(whole, int64(frac*1e9))
}
