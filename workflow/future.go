package workflow

import "context"

// Future supports repeated and concurrent Wait. Closing done publishes the
// immutable output and error to every waiter; no waiter consumes a channel value.
// Future 支持重复和并发 Wait。关闭 done 会向所有等待者发布不可变的输出和错误；
// 没有任何等待者会消费通道值。
type Future[O any] struct {
	done   chan struct{}
	output O
	err    error
}

func (f *Future[O]) Wait() (O, error) {
	<-f.done
	return f.output, f.err
}

func (f *Future[O]) WaitContext(ctx context.Context) (output O, err error) {
	select {
	case <-f.done:
		return f.output, f.err
	case <-ctx.Done():
		return output, ctx.Err()
	}
}

func (f *FlowContext) async[O any](key string, fn func(string) (O, error)) *Future[O] {
	result := &Future[O]{done: make(chan struct{})}
	resolved, err := f.claim(key)
	if err != nil {
		result.err = err
		f.remember(&err)
		close(result.done)
		return result
	}
	ex := f.execution
	ex.mu.Lock()
	if ex.closed {
		ex.mu.Unlock()
		result.err = ErrInvalid
		close(result.done)
		return result
	}
	ex.wg.Add(1)
	ex.mu.Unlock()
	go func() {
		defer ex.wg.Done()
		result.output, result.err = invoke(func() (O, error) { return fn(resolved) })
		f.remember(&result.err)
		close(result.done)
	}()
	return result
}

func (f *FlowContext) Go[I, O any](key string, action Action[I, O], input I, options ...ActionOptions) *Future[O] {
	return f.async(key, func(resolved string) (O, error) { return f.runAction(resolved, action, input, options...) })
}

func (f *FlowContext) GoFlow[I, O any](key string, flow Flow[I, O], input I) *Future[O] {
	return f.async(key, func(resolved string) (output O, err error) {
		if flow == nil {
			return output, ErrInvalid
		}
		return flow.Run(&FlowContext{execution: f.execution, prefix: resolved}, input)
	})
}
