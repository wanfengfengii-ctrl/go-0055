package ipp

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"pressguard/internal/domain"
)

// errTransport is a stub http.RoundTripper that surfaces the request
// context error when it is already done, or otherwise returns a
// configurable transport error. It lets the client exercise every
// transport-failure classification path deterministically, without real
// network I/O or timing.
type errTransport struct {
	err error
}

func (t *errTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	return nil, t.err
}

// netErrorStub is a configurable net.Error used to model raw transport
// timeouts (Timeout()==true) and plain network failures (Timeout()==false).
type netErrorStub struct {
	timeout   bool
	temporary bool
	msg       string
}

func (e *netErrorStub) Error() string   { return e.msg }
func (e *netErrorStub) Timeout() bool   { return e.timeout }
func (e *netErrorStub) Temporary() bool { return e.temporary }

func newClientWithTransport(rt http.RoundTripper) *Client {
	return NewClient("test-printer", "http://test.example/ipp", WithHTTPClient(&http.Client{
		Transport: rt,
	}))
}

// canceledContext returns an already-canceled context.
func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// expiredDeadlineContext returns a context whose deadline has already
// passed, so ctx.Err() reports context.DeadlineExceeded.
func expiredDeadlineContext() context.Context {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-1*time.Hour))
	cancel()
	return ctx
}

// TestClientDoErrorClassification covers the transport-failure
// classification boundary: transport/client timeouts and a caller context
// deadline both map to timeout, a plain network error stays network, and an
// active caller cancellation stays canceled. It asserts both the ProtoError
// kind produced by the public operation and the domain ErrorCode produced by
// Classify.
func TestClientDoErrorClassification(t *testing.T) {
	tests := []struct {
		name     string
		ctx      context.Context
		rtErr    error
		wantKind ErrKind
		wantCode domain.ErrorCode
	}{
		{
			name:     "transport timeout via client deadline surfaces as timeout",
			ctx:      context.Background(),
			rtErr:    context.DeadlineExceeded,
			wantKind: ErrKindTimeout,
			wantCode: domain.ErrTimeout,
		},
		{
			name:     "raw net transport timeout surfaces as timeout",
			ctx:      context.Background(),
			rtErr:    &netErrorStub{timeout: true, temporary: true, msg: "dial tcp: i/o timeout"},
			wantKind: ErrKindTimeout,
			wantCode: domain.ErrTimeout,
		},
		{
			name:     "caller context deadline expired surfaces as timeout",
			ctx:      expiredDeadlineContext(),
			rtErr:    nil,
			wantKind: ErrKindTimeout,
			wantCode: domain.ErrTimeout,
		},
		{
			name:     "plain network failure stays network",
			ctx:      context.Background(),
			rtErr:    &netErrorStub{timeout: false, temporary: false, msg: "dial tcp: connection refused"},
			wantKind: ErrKindNetwork,
			wantCode: domain.ErrNetwork,
		},
		{
			name:     "plain error stays network",
			ctx:      context.Background(),
			rtErr:    errors.New("connection reset"),
			wantKind: ErrKindNetwork,
			wantCode: domain.ErrNetwork,
		},
		{
			name:     "active caller cancellation stays canceled",
			ctx:      canceledContext(),
			rtErr:    nil,
			wantKind: ErrKindCanceled,
			wantCode: domain.ErrCanceled,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newClientWithTransport(&errTransport{err: tt.rtErr})
			_, err := c.GetPrinterAttributes(tt.ctx)
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			pe, ok := err.(*ProtoError)
			if !ok {
				t.Fatalf("expected *ProtoError, got %T: %v", err, err)
			}
			if pe.Kind != tt.wantKind {
				t.Errorf("ProtoError.Kind = %q, want %q (msg=%q)", pe.Kind, tt.wantKind, pe.Msg)
			}
			if got := Classify(err); got != tt.wantCode {
				t.Errorf("Classify = %q, want %q", got, tt.wantCode)
			}
		})
	}
}
