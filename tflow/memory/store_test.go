package memory_test

import (
	"context"
	"testing"
	"time"

	"github.com/k0923/go/tflow"
	"github.com/k0923/go/tflow/internal/storetest"
	"github.com/k0923/go/tflow/memory"
)

type mainTask struct{}

func (mainTask) Plan(context.Context, tflow.PlanRequest) (tflow.Plan, error) {
	return tflow.Page{}, nil
}

type subTask string

func (s subTask) Key() string { return string(s) }

func TestStoreContract(t *testing.T) {
	storetest.Run(t, func(t *testing.T) storetest.Fixture {
		now := time.Unix(100, 0)
		store := memory.New(memory.WithClock(tflow.ClockFunc(func() time.Time { return now }))).ForMain("contract")
		return storetest.Fixture{Store: store, Main: mainTask{},
			Plan:    tflow.Page{Items: []tflow.SubTask{subTask("a"), subTask("b")}},
			Advance: func(d time.Duration) { now = now.Add(d) }}
	})
}
