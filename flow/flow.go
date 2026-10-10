package flow

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

type Flow[Input any, Output any] struct {
	Handler func(ctx *FlowContext[Input, Output]) Output
}

func (flow *Flow[Input, Output]) StartRun(ctx context.Context, input Input, infrastructure Infrastructure) error {
	events := make([]Event, 0)
	runtime, err := newRuntime[Input, Output](infrastructure, events)
	if err != nil {
		return err
	}
	inputData, err := json.Marshal(input)
	if err != nil {
		return err
	}
	state := State{
		Input:  json.RawMessage(inputData),
		Status: FlowStatusRunning,
	}
	if err := infrastructure.SetState(ctx, state); err != nil {
		return err
	}

	return flow.run(ctx, input, json.RawMessage(inputData), runtime, infrastructure)
}

func (flow *Flow[Input, Output]) ResumeRun(ctx context.Context, infrastructure Infrastructure, signal Signal) error {
	state, err := infrastructure.GetState(ctx)
	if err != nil {
		return err
	}
	var input Input
	if err = json.Unmarshal(state.Input, &input); err != nil {
		return err
	}
	if state.Status != FlowStatusRunning {
		return fmt.Errorf("flow is not running")
	}
	events, err := infrastructure.GetEvents(ctx)
	if err != nil {
		return err
	}

	runtime, err := newRuntime[Input, Output](infrastructure, events)
	if err != nil {
		return err
	}
	switch s := signal.(type) {
	case conflictEventSignal:
		return fmt.Errorf("conflict event signal")
	case WaitForSignal:
		waitForEvent := runtime.getEvent[*WaitForEvent](s.Branches, s.Name)
		if waitForEvent == nil {
			return NewBizError(
				fmt.Sprintf("wait for event %s not found", s.Name),
				ErrWaitForEventNotFound,
			)
		}
		if runtime.getEvent[*WaitForResultEvent](s.Branches, s.Name) != nil {
			return nil
		}
		event := &WaitForResultEvent{
			Branches: append([]string(nil), waitForEvent.Branches...),
			Name:     s.Name,
			Response: s.Response,
			Ts:       time.Now(),
		}
		if accepted, err := runtime.AppendEvent(ctx, event); err != nil {
			return err
		} else if !accepted {
			return nil
		}
	case SleepSignal:
	}
	return flow.run(ctx, input, json.RawMessage(state.Input), runtime, infrastructure)
}

func (flow *Flow[Input, Output]) run(ctx context.Context, input Input, inputData json.RawMessage, runtime *runtime[Input, Output], infrastructure Infrastructure) (err error) {
	var signalWaitGroup sync.WaitGroup
	defer func() {
		runtime.Close()
		signalWaitGroup.Wait()
		if result := recover(); result != nil {
			if _, ok := result.(Signal); ok {
				return
			}
			state := State{
				Status: FlowStatusFailed,
				Input:  inputData,
			}
			if stateErr := infrastructure.SetState(ctx, state); stateErr != nil {
				err = NewInfrastructureError("fail to set failed flow state", stateErr)
				return
			}
			if resultErr, ok := result.(error); ok {
				err = resultErr
				return
			}
			err = NewSystemError(fmt.Sprintf("flow panic: %v", result))
		}
	}()
	signalWaitGroup.Go(func() {
		for signal := range runtime.GetSignal() {
			if signal.innerSignal() {
				continue
			}
			infrastructure.ProcessSignal(ctx, signal)
		}
	})
	flowCtx := newBranchContext(ctx, input, runtime, nil)
	output := flow.Handler(flowCtx)
	outputData, err := json.Marshal(output)
	if err != nil {
		return err
	}
	state := State{
		Input:  inputData,
		Output: json.RawMessage(outputData),
		Status: FlowStatusFinished,
	}
	if err := infrastructure.SetState(ctx, state); err != nil {
		return err
	}

	return nil
}
