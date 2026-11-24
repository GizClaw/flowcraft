package openai

import (
	"context"
	"io"
)

// sdkStream adapts the cursor every provider SDK exposes — advance, read the
// current value, report the failure, close — to the protocol's wire stream.
//
// The five streaming surfaces this driver serves differ in the cursor's concrete
// type and in nothing else, so the adapter is written once and parameterised
// instead of once per surface.
type sdkStream[T any] struct {
	advance func() bool
	current func() T
	failure func() error
	close   func() error
}

func newSDKStream[T any](
	advance func() bool,
	current func() T,
	failure func() error,
	close func() error,
) *sdkStream[T] {
	return &sdkStream[T]{advance: advance, current: current, failure: failure, close: close}
}

// Next advances one step, following the pull contract: the provider's cursor
// ended means io.EOF, and a reported failure is terminal.
func (s *sdkStream[T]) Next(context.Context) (T, error) {
	if s.advance() {
		return s.current(), nil
	}
	var zero T
	if err := s.failure(); err != nil {
		return zero, err
	}
	return zero, io.EOF
}

func (s *sdkStream[T]) Close() error { return s.close() }
