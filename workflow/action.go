package workflow

import (
	"context"
	"reflect"
)

// Action is a single executable step. Dependencies belong to the implementation;
// business state travels through input/output. Actions do not receive FlowContext.
// IO effects still require external idempotency across failed result commits.
// Action 表示一个执行步骤，依赖由实现持有，业务状态通过输入输出传递。
type Action[I, O any] interface {
	Run(context.Context, I) (O, error)
}

// ActionFn adapts a function to Action. Closures must not capture mutable
// orchestration state or FlowContext.
// ActionFn 将函数适配为 Action；闭包不能捕获可变编排状态或 FlowContext。
type ActionFn[I, O any] func(context.Context, I) (O, error)

func (fn ActionFn[I, O]) Run(ctx context.Context, input I) (output O, err error) {
	if fn == nil {
		return output, ErrInvalid
	}
	return fn(ctx, input)
}

// AsAction infers input/output types for ordinary functions and client methods.
func AsAction[I, O any](fn func(context.Context, I) (O, error)) Action[I, O] {
	return ActionFn[I, O](fn)
}

// Pure adapts a deterministic calculation; inexpensive calculations can simply
// execute in Flow without creating a durable step.
func Pure[I, O any](fn func(I) O) Action[I, O] {
	if fn == nil {
		return ActionFn[I, O](nil)
	}
	return ActionFn[I, O](func(_ context.Context, input I) (O, error) { return fn(input), nil })
}

func invalidAction[I, O any](action Action[I, O]) bool { return nilValue(action) }

// invalidFlow also catches a typed nil such as FlowFunc(nil).
// invalidFlow 同样能识别 FlowFunc(nil) 这类带类型的 nil。
func invalidFlow[I, O any](flow Flow[I, O]) bool { return nilValue(flow) }

func nilValue(v any) bool {
	if v == nil {
		return true
	}
	value := reflect.ValueOf(v)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
