package blossom

import (
	"context"
	"fmt"
	"io"
	"time"
)

// stallReader wraps an io.Reader with a context and timer that resets on every successful Read.
// If the timer expires, [stallReader.Context] is cancelled with a timeout error.
// When done, call [stallReader.Stop] to release resources.
type stallReader struct {
	data io.Reader

	ctx    context.Context
	cancel context.CancelCauseFunc

	timer   *time.Timer
	timeout time.Duration
}

func newStallReader(parent context.Context, data io.Reader, timeout time.Duration) *stallReader {
	if timeout <= 0 {
		panic("stall timeout must be positive")
	}

	ctx, cancel := context.WithCancelCause(parent)
	s := &stallReader{
		data:    data,
		timeout: timeout,
		ctx:     ctx,
		cancel:  cancel,
	}
	s.timer = time.AfterFunc(timeout, func() {
		cancel(fmt.Errorf("read stalled for longer than %v", timeout))
	})
	return s
}

// Context returns the context of the stall reader, which is cancelled if:
// - the parent context is cancelled
// - no Read has succeeded for longer than the stall timeout
func (s *stallReader) Context() context.Context {
	return s.ctx
}

// Err returns the error that caused the stall reader to be cancelled, or nil if it is still running.
// It's short for context.Cause(s.Context())
func (s *stallReader) Err() error {
	return context.Cause(s.ctx)
}

// Stop the stallReader timer and release resources.
func (s *stallReader) Stop() {
	s.timer.Stop()
	s.cancel(nil)
}

func (s *stallReader) Read(p []byte) (int, error) {
	if s.ctx.Err() != nil {
		return 0, context.Cause(s.ctx)
	}

	n, err := s.data.Read(p)
	if n > 0 {
		s.timer.Reset(s.timeout)
	}
	return n, err
}
