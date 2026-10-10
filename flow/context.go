package flow

import (
	"context"
	"time"
)

type StepContext struct {
	context.Context
	name string
}

func (ct StepContext) GetName() string {
	return ct.name
}

type StepOption[Result any] struct {
	Handler func(ctx StepContext) (Result, error)
}

type WaitForOption[Request any] struct {
	Request Request
}

type Branch[Input any, Output any] struct {
	Key     string
	Handler func(ctx *FlowContext[Input, Output])
}

type FlowContext[Input any, Output any] struct {
	context.Context
	runtime *runtime[Input, Output]
	branch  []string
	input   Input
}

func (ctx *FlowContext[Input, Output]) GetInput() Input {
	return ctx.input
}

func (ctx *FlowContext[Input, Output]) Step[Result any](name string, option StepOption[Result]) Result {
	return ctx.runtime.Step(ctx, ctx.branch, name, option.Handler)
}

func (ctx *FlowContext[Input, Output]) WaitFor[Request any, Response any](name string, option WaitForOption[Request]) Response {
	return ctx.runtime.WaitFor[Request, Response](ctx, ctx.branch, name, option)
}

func (ctx *FlowContext[Input, Output]) Sleep(name string, duration time.Duration) {
	ctx.runtime.Sleep(ctx, ctx.branch, name, duration)
}

func (ctx *FlowContext[Input, Output]) SleepUntil(name string, until time.Time) {
	ctx.runtime.SleepUntil(ctx, ctx.branch, name, until)
}

func (ctx *FlowContext[Input, Output]) Parallel(name string, branches ...Branch[Input, Output]) {
	ctx.runtime.Parallel(ctx, ctx.input, ctx.branch, name, branches...)
}

func (ctx *FlowContext[Input, Output]) Now(name string) time.Time {
	return ctx.runtime.Now(ctx, ctx.branch, name)
}

func (ctx *FlowContext[Input, Output]) UUID(name string) string {
	return ctx.runtime.UUID(ctx, ctx.branch, name)
}

func newBranchContext[Input any, Output any](ctx context.Context, input Input, runtime *runtime[Input, Output], branches []string) *FlowContext[Input, Output] {
	return &FlowContext[Input, Output]{
		Context: ctx,
		runtime: runtime,
		branch:  branches,
		input:   input,
	}
}
