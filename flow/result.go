package flow

type IResult[Output any] interface {
	Wait() Output
}

type fixedResult[Output any] struct {
	value  Output
	err    error
	signal Signal
}

func (r *fixedResult[Output]) Wait() Output {
	if r.err != nil {
		panic(r.err)
	}
	if r.signal != nil {
		panic(r.signal)
	}
	return r.value
}

func newWaitedResult[Output any](handler func() *fixedResult[Output]) *waitedResult[Output] {
	resultChan := make(chan *fixedResult[Output], 1)
	waitedResult := &waitedResult[Output]{resultChan: resultChan}
	go func() {
		resultChan <- handler()
	}()
	return waitedResult
}

type waitedResult[Output any] struct {
	resultChan chan *fixedResult[Output]
	result     *fixedResult[Output]
}

func (wr *waitedResult[Output]) Wait() Output {
	if wr.result != nil {
		return wr.result.Wait()
	}
	wr.result = <-wr.resultChan
	return wr.result.Wait()
}
