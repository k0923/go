package flow

import (
	"context"
	"encoding/json"
	"fmt"
)

type Flow[Input any, Output any] struct {
	// Handler must not recover panics from flow runtime APIs.
	Handler func(ctx *runtime[Input]) Output
}

type flowCompleted struct {
	output any
}

func (flow *Flow[Input, Output]) StartRun(ctx context.Context, input Input, infrastructure Infrastructure) {
	state, err := infrastructure.GetState(ctx)
	if err != nil {
		infrastructure.OnError(err)
		return
	}

	if state.Status == FlowStatusCompleted || state.Status == FlowStatusFailed {
		return
	}

	if state.Status == FlowStatusRunning {
		var input Input
		if err := json.Unmarshal(state.Input, &input); err != nil {
			infrastructure.OnError(err)
			return
		}
		flow.run(ctx, input, infrastructure)
		return
	}

	inputData, err := json.Marshal(input)
	if err != nil {
		infrastructure.OnError(err)
		return
	}

	state = State{
		Input:  json.RawMessage(inputData),
		Status: FlowStatusRunning,
	}

	if err := infrastructure.SetState(ctx, state); err != nil {
		infrastructure.OnError(err)
		return
	}

	flow.run(ctx, input, infrastructure)
}

func (flow *Flow[Input, Output]) ResumeRun(ctx context.Context, infrastructure Infrastructure, signal Signal) {
	state, err := infrastructure.GetState(ctx)
	if err != nil {
		infrastructure.OnError(err)
		return
	}
	var input Input
	if err := json.Unmarshal(state.Input, &input); err != nil {
		infrastructure.OnError(err)
		return
	}

	if state.Status != FlowStatusRunning {
		return
	}
	resolved, err := infrastructure.ResolveSignal(ctx, signal)
	if err != nil {
		infrastructure.OnError(err)
		return
	}
	if !resolved {
		// 信号冲突，忽略
		return
	}
	flow.run(ctx, input, infrastructure)

}

func (flow *Flow[Input, Output]) run(ctx context.Context, input Input, infrastructure Infrastructure) {
	runtime := &runtime[Input]{Context: ctx, infrastructure: infrastructure}
	setState := func(output json.RawMessage, status FlowStatus, errors []string) {
		inputData, err := json.Marshal(input)
		if err != nil {
			infrastructure.OnError(err)
			return
		}
		if err := infrastructure.SetState(ctx, State{
			Input:  inputData,
			Output: output,
			Status: status,
			Errors: errors,
		}); err != nil {
			infrastructure.OnError(err)
		}
	}

	defer func() {
		result := recover()
		if result == nil {
			return
		}
		switch output := result.(type) {
		case flowCompleted:
			outputData, err := json.Marshal(output.output)
			if err != nil {
				infrastructure.OnError(err)
				setState(nil, FlowStatusFailed, []string{err.Error()})
				return
			}
			setState(json.RawMessage(outputData), FlowStatusCompleted, nil)
			return
		case BranchSignal:
			errors := make([]string, 0)
			for _, branchResult := range output {
				if branchResult == nil {
					continue
				}
				switch result := branchResult.(type) {
				case Signal:
					continue
				case error:
					infrastructure.OnError(result)
					errors = append(errors, result.Error())
				default:
					err := NewSystemError(fmt.Sprintf("flow panic: %v", result))
					infrastructure.OnError(err)
					errors = append(errors, err.Error())
				}
			}
			if len(errors) > 0 {
				setState(nil, FlowStatusFailed, errors)
				return
			}
			return
		case error:
			if IsConflictError(output) {
				return
			}
			infrastructure.OnError(output)
			setState(nil, FlowStatusFailed, []string{output.Error()})
			return
		case Signal:
			return
		default:
			err := NewSystemError(fmt.Sprintf("flow panic: %v", output))
			infrastructure.OnError(err)
			setState(nil, FlowStatusFailed, []string{err.Error()})
			return
		}
	}()

	panic(flowCompleted{output: flow.Handler(runtime)})
}
