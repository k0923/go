package main

import (
	"context"
	"fmt"

	"github.com/k0923/go/tflow"
	"github.com/k0923/go/tflow/memory"
)

type rows struct {
	position int64
	limits   *[]int
	counts   *[]int
}
type unit int64

func (u unit) Key() string { return fmt.Sprint(int64(u)) }

type rowPlan struct {
	tasks []tflow.SubTask
	end   int64
}

func (p rowPlan) Tasks() []tflow.SubTask { return p.tasks }
func (p rowPlan) HasMore() bool          { return p.end < 22 }
func (r *rows) Plan(_ context.Context, req tflow.PlanRequest) (tflow.Plan, error) {
	*r.limits = append(*r.limits, req.Limit())
	end := min(int64(22), r.position+int64(req.Limit()))
	p := rowPlan{end: end}
	for i := r.position; i < end; i++ {
		p.tasks = append(p.tasks, unit(i))
	}
	*r.counts = append(*r.counts, len(p.tasks))
	return p, nil
}

func scenario(name string, policy tflow.PlanningPolicy) error {
	ctx := context.Background()
	var limits, counts []int
	storage := memory.New(memory.WithPlanCommit(func(task tflow.MainTask, plan tflow.Plan) (tflow.MainTask, error) {
		next := *task.(*rows)
		next.position = plan.(rowPlan).end
		return &next, nil
	})).ForMain(name)
	runner, err := tflow.New(storage, tflow.Options{})
	if err != nil {
		return err
	}
	m, err := runner.Submit(ctx, &rows{limits: &limits, counts: &counts}, tflow.SubmitOptions{
		ConcurrencyLimit: 5,
		PlanningPolicy:   policy,
	})
	if err != nil {
		return err
	}
	for completed := 0; !m.Status.Terminal(); completed++ {
		running, err := runner.ListSubTasks(ctx, tflow.SubQuery{Statuses: []tflow.SubStatus{tflow.SubRunning}, Limit: 1})
		if err != nil {
			return err
		}
		if len(running) != 1 {
			return fmt.Errorf("%s: expected a running unit", name)
		}
		receipt, err := runner.Report(ctx, tflow.Report{Execution: running[0].Execution(name), Status: tflow.SubSucceeded})
		if err != nil {
			return err
		}
		m = receipt.Main
		if completed == 0 {
			fmt.Printf("%s after first completion: running=%d pending=%d cursor=%d\n", name, m.Subs.Running, m.Subs.Pending, m.DispatchCursor)
		}
	}
	fmt.Printf("%s completed: %s requests=%v returned=%v\n", name, m.Status, limits, counts)
	return nil
}

func main() {
	for _, strategy := range []struct {
		name   string
		policy tflow.PlanningPolicy
	}{
		{"wave", tflow.WavePolicy{}},
		{"buffer", tflow.BufferPolicy{TargetPending: 4, MaxPlanSize: 5}},
		{"fill", tflow.FillPolicy{}},
	} {
		if err := scenario(strategy.name, strategy.policy); err != nil {
			panic(err)
		}
	}
}
