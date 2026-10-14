package flow

import (
	"context"
	"errors"
	"time"
)

const (
	executionLeaseTTL = 30 * time.Second
	minLoopInterval   = 5 * time.Second
)

type StepOption[Input any, Output any, Result any] struct {
	Handler func(ctx StepContext[Input, Output]) (Result, error)
}

type WaitForOption[Request any] struct {
	Request Request
}

type comparableResultEvent interface {
	ResultEvent
	comparable
}

func contextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func ensureContext(ctx context.Context, event Event) {
	if err := contextErr(ctx); err != nil {
		panic(NewContextError(event, err))
	}
}

func bindContextError(event Event, err error) error {
	var contextErr *ContextError
	if errors.As(err, &contextErr) && contextErr.Event == nil {
		return NewContextError(event, contextErr.Cause)
	}
	return err
}

func run[Output any](handler func() Output) (result *fixedResult[Output]) {
	result = &fixedResult[Output]{}
	defer func() {
		recoverResult := recover()
		if recoverResult == nil {
			return
		}
		switch output := recoverResult.(type) {
		case error:
			result.err = output
			return
		case Signal:
			result.signal = output
			return
		default:
			result.err = NewUnknownError(output)
			return
		}
	}()
	result.value = handler()
	return
}
