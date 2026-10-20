package zipbox

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrorCode is the public error code carried by every Zipbox error. The code is
// the contract; the message is for humans and may change.
type ErrorCode string

// The public error codes. See https://zipbox.ai/docs/errors for causes and fixes.
const (
	CodeInvalidRequest      ErrorCode = "invalid_request"
	CodeUnauthorized        ErrorCode = "unauthorized"
	CodeForbidden           ErrorCode = "forbidden"
	CodeNotFound            ErrorCode = "not_found"
	CodeInsufficientCredits ErrorCode = "insufficient_credits"
	CodeConflict            ErrorCode = "conflict"
	CodeRateLimited         ErrorCode = "rate_limited"
	CodeBoxNotRunning       ErrorCode = "box_not_running"
	CodeUnavailable         ErrorCode = "unavailable"
)

const docsErrorsURL = "https://zipbox.ai/docs/errors"

// Error is the single error type every SDK call returns for a failure the
// control plane or a box reported (and for a box that is not ready). Use
// errors.As to inspect it:
//
//	var zerr *zipbox.Error
//	if errors.As(err, &zerr) && zerr.Code == zipbox.CodeNotFound { … }
//
// A command that exits non-zero is a result, never an Error.
type Error struct {
	// Code is the public error code.
	Code ErrorCode `json:"code"`
	// Status is the HTTP status, or 0 when the error did not come from a response.
	Status int `json:"status,omitempty"`
	// Message is a human-readable description.
	Message string `json:"message"`
	// RequestID identifies the request in Zipbox's logs, when the server sent one.
	RequestID string `json:"requestId,omitempty"`
	// Retryable reports whether the same request may succeed if sent again later.
	Retryable bool `json:"retryable"`
	// RetryAfter is how long the server asked the caller to wait, when it said.
	RetryAfter time.Duration `json:"-"`
	// DocsURL points at the docs entry for Code.
	DocsURL string `json:"docsUrl"`

	// hasRetryAfter distinguishes "retry now" (Retry-After: 0) from no hint.
	hasRetryAfter bool
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("zipbox: %s (%d): %s", e.Code, e.Status, e.Message)
	}
	return fmt.Sprintf("zipbox: %s: %s", e.Code, e.Message)
}

// IsCode reports whether err is a *Error with the given code.
func IsCode(err error, code ErrorCode) bool {
	zerr, ok := asError(err)
	return ok && zerr.Code == code
}

func asError(err error) (*Error, bool) {
	for err != nil {
		if zerr, ok := err.(*Error); ok {
			return zerr, true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return nil, false
		}
		err = unwrapper.Unwrap()
	}
	return nil, false
}

func docsURLFor(code ErrorCode) string {
	return docsErrorsURL + "#" + string(code)
}

func retryableCode(code ErrorCode) bool {
	switch code {
	case CodeRateLimited, CodeUnavailable, CodeBoxNotRunning:
		return true
	}
	return false
}

// codeForStatus is the status mapping every SDK shares, for responses that
// carry no error envelope (box routes, proxy error pages).
func codeForStatus(status int) ErrorCode {
	switch status {
	case 400, 413, 422:
		return CodeInvalidRequest
	case 401:
		return CodeUnauthorized
	case 402:
		return CodeInsufficientCredits
	case 403:
		return CodeForbidden
	case 404:
		return CodeNotFound
	case 409:
		return CodeConflict
	case 429:
		return CodeRateLimited
	case 502:
		return CodeBoxNotRunning
	}
	if status >= 500 {
		return CodeUnavailable
	}
	return CodeInvalidRequest
}

func newError(code ErrorCode, message string) *Error {
	return &Error{Code: code, Message: message, Retryable: retryableCode(code), DocsURL: docsURLFor(code)}
}

type envelopeBody struct {
	Code         ErrorCode `json:"code"`
	Message      string    `json:"message"`
	RequestID    string    `json:"requestId"`
	Retryable    *bool     `json:"retryable"`
	RetryAfterMs *int64    `json:"retryAfterMs"`
	DocsURL      string    `json:"docsUrl"`
}

// errorFromResponse classifies a non-2xx response. An `{error: {code, …}}` body
// is the public envelope; anything else (box routes answer `{error: "<msg>"}`)
// maps by status.
func errorFromResponse(resp *http.Response, body []byte) *Error {
	retryAfter, hasRetryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
	var raw struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &raw) == nil && len(raw.Error) > 0 {
		var env envelopeBody
		if raw.Error[0] == '{' && json.Unmarshal(raw.Error, &env) == nil && env.Code != "" {
			zerr := &Error{
				Code:       env.Code,
				Status:     resp.StatusCode,
				Message:    env.Message,
				RequestID:  env.RequestID,
				Retryable:  retryableCode(env.Code),
				RetryAfter: retryAfter,
				DocsURL:    env.DocsURL,

				hasRetryAfter: hasRetryAfter,
			}
			if env.Retryable != nil {
				zerr.Retryable = *env.Retryable
			}
			if env.RetryAfterMs != nil && !hasRetryAfter {
				zerr.RetryAfter = time.Duration(*env.RetryAfterMs) * time.Millisecond
				zerr.hasRetryAfter = true
			}
			if zerr.DocsURL == "" {
				zerr.DocsURL = docsURLFor(zerr.Code)
			}
			return zerr
		}
		var message string
		if json.Unmarshal(raw.Error, &message) == nil {
			return statusError(resp, message, retryAfter, hasRetryAfter)
		}
	}
	message := strings.TrimSpace(string(body))
	if len(message) > 300 || message == "" {
		message = http.StatusText(resp.StatusCode)
	}
	return statusError(resp, message, retryAfter, hasRetryAfter)
}

func statusError(resp *http.Response, message string, retryAfter time.Duration, hasRetryAfter bool) *Error {
	code := codeForStatus(resp.StatusCode)
	return &Error{
		Code:       code,
		Status:     resp.StatusCode,
		Message:    message,
		RequestID:  resp.Header.Get("X-Request-Id"),
		Retryable:  retryableCode(code),
		RetryAfter: retryAfter,
		DocsURL:    docsURLFor(code),

		hasRetryAfter: hasRetryAfter,
	}
}

// parseRetryAfter reads `Retry-After` as delta-seconds or an HTTP date.
// The bool is false when the header is absent or unreadable.
func parseRetryAfter(value string) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseFloat(value, 64); err == nil && seconds >= 0 {
		return time.Duration(seconds * float64(time.Second)), true
	}
	if at, err := http.ParseTime(value); err == nil {
		if wait := time.Until(at); wait > 0 {
			return wait, true
		}
		return 0, true
	}
	return 0, false
}
