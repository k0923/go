package workflow

import "context"

type infrastructureContextKey struct{}

type WorkflowContext struct {
	FlowContext
}

func WithInfrastructure(ctx context.Context, api Infrastructure) context.Context {
	return context.WithValue(ctx, infrastructureContextKey{}, api)
}

func infrastructureFromContext(ctx context.Context) Infrastructure {
	if api, ok := ctx.Value(infrastructureContextKey{}).(Infrastructure); ok && api != nil {
		return api
	}
	return NewDebugInfrastructure()
}

func newWorkflowContext(ctx context.Context, api Infrastructure) *WorkflowContext {
	return &WorkflowContext{
		FlowContext: *newFlowContext(ctx, "", api),
	}
}

func (ctx *WorkflowContext) RunFlow[Input any, Output any](
	key string,
	flow Flow[Input, Output],
	input Input,
	option FlowRunOption,
) IResult[Output] {
	ensureContext(ctx.Context, nil)
	return newWaitedResult(func() *fixedResult[Output] {
		return run(func() Output {
			flowContext := newFlowContext(
				ctx.Context,
				ctx.apiKey(key),
				ctx.api,
			)
			return flow.Run(flowContext, input)
		})
	})
}

func RunFlow[Input any, Output any](
	ctx context.Context,
	handler func(ctx *WorkflowContext) Output,
	input Input,
) Output {
	_ = input
	return run(func() Output {
		ensureContext(ctx, nil)
		return handler(newWorkflowContext(ctx, infrastructureFromContext(ctx)))
	}).Wait()
}
