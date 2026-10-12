package flow

type IResult[Output any] interface {
	Wait() Output
}

func newFixedResult[Output any](resultHandler func() any) *fixedResult[Output] {
	return &fixedResult[Output]{resultHandler: resultHandler}
}

type fixedResult[Output any] struct {
	resultHandler func() any
}

func (fr *fixedResult[Output]) Wait() Output {
	output := fr.resultHandler()
	switch result := output.(type) {
	case commonValue[Output]:
		return result.Value
	case Output:
		return result
	default:
		panic(output)
	}
}

func newWaitedResult[Output any](handler func() Output) *waitedResult[Output] {
	resultChan := make(chan any, 1)
	waitedResult := &waitedResult[Output]{resultChan: resultChan}
	go func() {
		defer func() {
			resultChan <- recover()
			close(resultChan)
		}()
		output := handler()
		panic(commonValue[Output]{Value: output})
	}()
	return waitedResult
}

type waitedResult[Output any] struct {
	resultChan  chan any
	result      Output
	isCompleted bool
}

func (wr *waitedResult[Output]) Wait() Output {
	if wr.isCompleted {
		return wr.result
	}
	output := <-wr.resultChan
	switch result := output.(type) {
	case commonValue[Output]:
		wr.result = result.Value
		wr.isCompleted = true
		return wr.result
	default:
		panic(result)
	}
}
