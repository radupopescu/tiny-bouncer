package jev

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"tinybouncer/internal/backend/systemone"
)

// Error taxonomy for the Jev transport. Every failure mode maps to one typed
// error so callers (T06 backend, T07 doctor) can branch without string
// matching. All carry a one-line human message.

// ConfigError reports missing or invalid configuration (e.g. no API key).
// Non-retryable by construction. It is the shared System One configuration
// error, re-exported so transport and judgment configuration errors have one
// identity (errors.As reaches it through either name).
type ConfigError = systemone.ConfigError

// AuthError is a 401: the API key is missing, malformed, or rejected.
// Never retried.
type AuthError struct{ Message string }

func (e *AuthError) Error() string { return e.Message }

// RateLimitError is a 429 or an exhausted retry against a rate-limiting
// endpoint (also 502/503/504). RetryAfter mirrors the parsed retry-after
// header, if any.
type RateLimitError struct {
	Status     int
	RetryAfter time.Duration
	Message    string
}

func (e *RateLimitError) Error() string { return e.Message }

// OverloadedError is a 529 (TypeSafe's "Overloaded" status). Retried like the
// other transient statuses; constructed here when retries are exhausted.
type OverloadedError struct {
	Status     int
	RetryAfter time.Duration
	Message    string
}

func (e *OverloadedError) Error() string { return e.Message }

// InvalidRequestError is a 422: a request validation bug on our side.
// Never retried; Snippet carries a short body excerpt for diagnosis (bodies
// echo our payload, never the API key).
type InvalidRequestError struct {
	Status  int
	Snippet string
	Message string
}

func (e *InvalidRequestError) Error() string { return e.Message }

// TimeoutError is a per-request context/HTTP deadline exceeded.
type TimeoutError struct{ Err error }

func (e *TimeoutError) Error() string { return "jev: request timed out" }
func (e *TimeoutError) Unwrap() error { return e.Err }

// NetworkError covers transport failures that are clearly not timeouts
// (connection refused, DNS, reset mid-response...).
type NetworkError struct{ Err error }

func (e *NetworkError) Error() string {
	return fmt.Sprintf("jev: network error: %v", e.Err)
}
func (e *NetworkError) Unwrap() error { return e.Err }

// UnusableBodyError is a 2xx response whose body is not decodable JSON (bad
// format, truncation, proxy error page). Snippet carries a short excerpt.
type UnusableBodyError struct {
	Status  int
	Snippet string
	Message string
}

func (e *UnusableBodyError) Error() string { return e.Message }

// snippet collapses a response body onto one line and truncates it to a
// bounded excerpt for error messages. Only bodies we control (error text or
// our own request echo) are embedded, never the request payload.
func snippet(body string, limit int) string {
	s := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, body)
	if len(s) > limit {
		s = s[:limit] + "…"
	}
	return s
}

// retryableStatus reports whether a status triggers the retry loop.
func retryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, 529,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}
