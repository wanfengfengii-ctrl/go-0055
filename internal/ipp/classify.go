package ipp

import (
	"context"
	"errors"
	"net"
)

// classifyDoError maps a transport error returned by http.Client.Do to a
// stable ProtoError kind so callers (via Classify) can record a stable
// attempt category and the scheduler can route it through the right
// recovery branch.
//
// Transport/client timeouts and a caller context whose deadline has expired
// both surface as ErrKindTimeout; an active caller cancellation keeps
// ErrKindCanceled; everything else is a plain network failure.
func classifyDoError(ctx context.Context, err error) *ProtoError {
	// An active caller cancellation keeps its own semantics and is checked
	// first: if the caller explicitly canceled, the failure is a
	// cancellation rather than a timeout, even if a deadline error happens
	// to be racing it.
	if ctx.Err() == context.Canceled {
		return &ProtoError{Kind: ErrKindCanceled, Msg: err.Error()}
	}
	// A transport/client timeout (a net.Error whose Timeout reports true, or
	// context.DeadlineExceeded surfaced by the HTTP client's own deadline)
	// and a caller context whose deadline has expired both map to timeout.
	if isTimeoutError(err) || ctx.Err() == context.DeadlineExceeded {
		return &ProtoError{Kind: ErrKindTimeout, Msg: err.Error()}
	}
	return &ProtoError{Kind: ErrKindNetwork, Msg: err.Error()}
}

// isTimeoutError reports whether err represents a transport timeout. It
// recognises context.DeadlineExceeded (used by both the HTTP client's own
// Timeout and caller deadlines) and any net.Error whose Timeout method
// reports true (e.g. dial/TLS/read timeouts).
func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return ne.Timeout()
	}
	return false
}
