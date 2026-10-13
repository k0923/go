package flow

import (
	"context"
)

type Flow[Input any, Output any] struct {
	// Handler must not recover panics from flow runtime APIs.
	Handler func(ctx *Runtime[Input, Output]) Output
}

func (flow *Flow[Input, Output]) StartRun(ctx context.Context, input Input, infrastructure Infrastructure[Input, Output]) {
	state, err := infrastructure.GetState(ctx)
	if err != nil {
		infrastructure.OnError(err)
		return
	}

	if state.Status == FlowStatusCompleted || state.Status == FlowStatusFailed {
		return
	}

	if state.Status == FlowStatusRunning {
		runtime := newRuntime(ctx, infrastructure, state.Input)
		runtime.run(flow.Handler, nil)
		return
	}

	state = State[Input, Output]{
		Input:  input,
		Status: FlowStatusRunning,
	}

	if err := infrastructure.SetState(ctx, state); err != nil {
		infrastructure.OnError(err)
		return
	}
	runtime := newRuntime(ctx, infrastructure, input)
	runtime.run(flow.Handler, nil)
}

func (flow *Flow[Input, Output]) ResumeRun(ctx context.Context, infrastructure Infrastructure[Input, Output], signal Signal) {
	state, err := infrastructure.GetState(ctx)
	if err != nil {
		infrastructure.OnError(err)
		return
	}
	if state.Status != FlowStatusRunning {
		return
	}
	runtime := newRuntime[Input, Output](ctx, infrastructure, state.Input)
	runtime.run(flow.Handler, signal)
}
