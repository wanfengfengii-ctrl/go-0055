package domain

import "fmt"

// Code is a stable machine-readable error identifier. Codes never change
// for a given failure class, so API clients can branch on them reliably.
type Code string

const (
	CodeOK                 Code = "ok"
	CodeNotFound           Code = "not_found"
	CodeConflict           Code = "conflict"
	CodeRevisionMismatch   Code = "revision_mismatch"
	CodeCapabilityMismatch Code = "capability_mismatch"
	CodeStorage            Code = "storage_error"
	CodeProtocol           Code = "protocol_error"
	CodeUncertain          Code = "uncertain"
	CodeUnavailable        Code = "unavailable"
	CodeBadRequest         Code = "bad_request"
	CodeInternal           Code = "internal"
	CodeTimeout            Code = "timeout"
	CodeNotAllowed         Code = "not_allowed"
)

// Error is the canonical error type carrying a stable Code plus an optional
// detail map for diagnostics. Comparisons on Code, not on Message.
type Error struct {
	Code    Code
	Message string
	Detail  map[string]any
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if len(e.Detail) > 0 {
		return fmt.Sprintf("%s: %s (%v)", e.Code, e.Message, e.Detail)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// NewError constructs an Error with the given code and message.
func NewError(code Code, msg string) *Error {
	return &Error{Code: code, Message: msg}
}

// WithDetail attaches a single key/value detail and returns the error.
func (e *Error) WithDetail(k string, v any) *Error {
	if e.Detail == nil {
		e.Detail = map[string]any{}
	}
	e.Detail[k] = v
	return e
}

// AsCode extracts the Code from an error, defaulting to CodeInternal.
func AsCode(err error) Code {
	if err == nil {
		return CodeOK
	}
	if e, ok := err.(*Error); ok {
		return e.Code
	}
	return CodeInternal
}

// Errf is a convenience constructor with formatting.
func Errf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}
