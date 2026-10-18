package workflow

import (
	"errors"
	"sync/atomic"
)

// Future supports repeated and concurrent Wait. Closing done publishes the
// immutable output and error to every waiter; no waiter consumes a channel value.
// Future 支持重复和并发 Wait。关闭 done 会向所有等待者发布不可变的输出和错误；
// 没有任何等待者会消费通道值。
type Future[O any] struct {
	done   chan struct{}
	output O
	err    error
	// halted means the task's strand suspended or aborted; Wait re-halts the
	// caller so a suspension cannot be observed as a zero value.
	// halted 表示任务所在 strand 已挂起或中止；Wait 会让调用方再次停止，
	// 因此挂起不会被观察为零值。
	halted   bool
	observed atomic.Bool
	owner    *FlowContext
}

// Wait blocks until the task finishes. err is nil or the task's business
// error (*ActionError, or whatever a GoFlow handler returned). If the task is
// waiting or the drive aborted, Wait halts the calling orchestration instead
// of returning. Call it from orchestration code, not from a raw goroutine.
// Wait 阻塞直到任务结束。err 只会是 nil 或任务的业务错误（*ActionError，或
// GoFlow 处理函数返回的任意 error）。若任务在等待或 drive 已中止，Wait 会停止
// 调用方编排而不是返回。应在编排代码中调用，不要在裸 goroutine 中调用。
func (f *Future[O]) Wait() (O, error) {
	<-f.done
	if f.halted {
		f.owner.halt()
	}
	f.observed.Store(true)
	return f.output, f.err
}

type observable interface {
	unobserved() error
}

func (f *Future[O]) unobserved() error {
	if f.halted || f.err == nil || f.observed.Load() {
		return nil
	}
	return f.err
}

// async starts a task on its own strand at path/key. The task must not touch
// the caller's strand: a halt in the task is reported through the Future.
// async 在独立 strand 上于 path/key 启动任务。任务不得触及调用方 strand：任务中的 halt 通过 Future 报告。
func (f *FlowContext) async[O any](path Path, key string, fn func(child *FlowContext) (O, error)) *Future[O] {
	result := &Future[O]{done: make(chan struct{}), owner: f}
	child := &FlowContext{execution: f.execution, path: f.path, depth: f.depth, scope: f.scope, strand: &strand{}}
	ex := f.execution
	ex.mu.Lock()
	if ex.closed || (f.scope != nil && f.scope.closed) {
		ex.mu.Unlock()
		f.abort(fatal(errors.New("workflow: async task started after its scope closed")))
	}
	ex.wg.Add(1)
	if f.scope != nil {
		f.scope.wg.Add(1)
	}
	ex.futures = append(ex.futures, result)
	ex.mu.Unlock()
	go func() {
		defer ex.wg.Done()
		if f.scope != nil {
			defer f.scope.wg.Done()
		}
		defer close(result.done)
		var err error
		result.output, err, result.halted = runStrand(child.strand, func() (O, error) { return fn(child) })
		var fatalErr *fatalError
		if errors.As(err, &fatalErr) {
			// An orchestration panic inside GoFlow is a definition bug.
			// GoFlow 内的编排 panic 是定义缺陷。
			ex.mu.Lock()
			ex.abort = append(ex.abort, err)
			ex.mu.Unlock()
			ex.cancel()
			result.halted = true
			return
		}
		result.err = err
	}()
	return result
}

// Go starts an Action concurrently at name[input.StepKey()] (or name). Its
// Future reports nil or *ActionError.
// Go 在 name[input.StepKey()]（或 name）并发启动一个 Action。它的 Future 报告 nil 或 *ActionError。
func (f *FlowContext) Go[I, O any](name string, action Action[I, O], input I, options ...ActionOptions) *Future[O] {
	path, key := f.claimInput(name, input)
	return f.async(path, key, func(child *FlowContext) (O, error) {
		output, business, err := child.runAction(path, key, action, input, options...)
		child.must(err)
		if business != nil {
			return output, business
		}
		return output, nil
	})
}

// GoFlow mounts a Flow on its own strand, like RunFlow but concurrent. Its
// Future completes after the called Flow and its async Actions exit.
// GoFlow 在独立 strand 上挂载 Flow，语义同 RunFlow 但并发执行；Future 在 Flow 及其
// 异步 Action 全部退出后完成。
func (f *FlowContext) GoFlow[I, O any](name string, flow Flow[I, O], input I) *Future[O] {
	path, key := f.claimInput(name, input)
	return f.async(path, key, func(child *FlowContext) (output O, err error) {
		if invalidFlow(flow) {
			child.abort(fatal(ErrInvalid))
		}
		return child.runChildFlow(path, flow, input)
	})
}
