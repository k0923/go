package workflow

type workflowSignal interface {
	workflowSignal()
}

type awaitSignal struct {
	event *SignalAwaitedEvent
}

func (*awaitSignal) workflowSignal() {}

type sleepSignal struct {
	event *SleepScheduledEvent
}

func (*sleepSignal) workflowSignal() {}

type branchSignal struct {
	signals map[string]workflowSignal
	errors  map[string]error
}

func (*branchSignal) workflowSignal() {}
