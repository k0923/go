// A complete business-owned codec and pagination adapter for a durable batch.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/k0923/go/tflow"
	"github.com/k0923/go/tflow/examples/internal/executor"
	"github.com/k0923/go/tflow/sqlite"
)

type Batch struct {
	Next  int
	Total int
}

type Unit struct{ Row int }

func (u Unit) Key() string { return fmt.Sprint(u.Row) }

type BatchPlan struct {
	tflow.Page
	Next int
}

func (b *Batch) Plan(_ context.Context, req tflow.PlanRequest) (tflow.Plan, error) {
	end := min(b.Total, b.Next+req.Limit())
	p := BatchPlan{Next: end}
	p.More = end < b.Total
	for i := b.Next; i < end; i++ {
		p.Items = append(p.Items, Unit{Row: i})
	}
	return p, nil
}

type Envelope struct {
	Type    string
	Version int
	Payload json.RawMessage
}

type BusinessCodec struct{}

func (BusinessCodec) Encode(value any) ([]byte, error) {
	var kind string
	switch value.(type) {
	case *Batch:
		kind = "batch"
	case Unit:
		kind = "unit"
	case tflow.FillPolicy:
		kind = "fill"
	case tflow.BufferPolicy:
		kind = "buffer"
	case tflow.WavePolicy:
		kind = "wave"
	case tflow.DefaultMainDecider:
		kind = "default_decider"
	default:
		return nil, fmt.Errorf("unsupported business type %T", value)
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(Envelope{Type: kind, Version: 1, Payload: data})
}

func (BusinessCodec) Decode(data []byte) (any, error) {
	var envelope Envelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, err
	}
	if envelope.Version != 1 {
		return nil, fmt.Errorf("unsupported business schema %d", envelope.Version)
	}
	var target any
	switch envelope.Type {
	case "batch":
		target = &Batch{}
	case "unit":
		target = &Unit{}
	case "fill":
		target = &tflow.FillPolicy{}
	case "buffer":
		target = &tflow.BufferPolicy{}
	case "wave":
		target = &tflow.WavePolicy{}
	case "default_decider":
		target = &tflow.DefaultMainDecider{}
	default:
		return nil, fmt.Errorf("unknown business type %q", envelope.Type)
	}
	if err := json.Unmarshal(envelope.Payload, target); err != nil {
		return nil, err
	}
	switch v := target.(type) {
	case *Unit:
		return *v, nil
	case *tflow.FillPolicy:
		return *v, nil
	case *tflow.BufferPolicy:
		return *v, nil
	case *tflow.WavePolicy:
		return *v, nil
	case *tflow.DefaultMainDecider:
		return *v, nil
	default:
		return target, nil
	}
}

func run(ctx context.Context, path string) (err error) {
	repository, err := sqlite.New(ctx, path, sqlite.Options{
		Codec: BusinessCodec{},
		CommitPlan: func(old tflow.MainTask, p tflow.Plan) (tflow.MainTask, error) {
			next := *old.(*Batch)
			next.Next = p.(BatchPlan).Next
			return &next, nil
		},
	})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, repository.Close()) }()
	store, err := repository.Open(ctx, "reference-batch")
	if err != nil {
		return err
	}
	runner, err := tflow.New(store, tflow.Options{})
	if err != nil {
		return err
	}
	progress, err := runner.GetMain(ctx)
	if errors.Is(err, tflow.ErrNotFound) {
		progress, err = runner.Submit(ctx, &Batch{Total: 20}, tflow.SubmitOptions{
			ConcurrencyLimit: 3, PlanningPolicy: tflow.BufferPolicy{TargetPending: 3, MaxPlanSize: 6},
		})
	}
	if err != nil {
		return err
	}
	// Replace with a durable idempotent business executor for real side effects.
	exec := executor.Func(func(ctx context.Context, task tflow.SubTask, execution tflow.Execution) (tflow.DispatchResult, error) {
		if err := ctx.Err(); err != nil {
			return tflow.DispatchResult{}, err
		}
		fmt.Printf("row=%d execution=%s\n", task.(Unit).Row, execution.Key())
		return tflow.DispatchResult{Status: tflow.ExecutionSucceeded}, nil
	})
	dispatcher, err := tflow.NewExecutorDispatcher(runner, exec)
	if err != nil {
		return err
	}
	for !progress.Status.Terminal() {
		receipt, err := runner.DispatchBatch(ctx, 100, dispatcher)
		if err != nil {
			return err
		}
		progress, err = runner.GetMain(ctx)
		if err != nil {
			return err
		}
		if receipt.Claimed == 0 && !progress.Status.Terminal() {
			return errors.New("no due commands; schedule another worker pass")
		}
	}
	// Deliver final notifications too, including after a prior completed run.
	if _, err := runner.DispatchBatch(ctx, 100, dispatcher); err != nil {
		return err
	}
	fmt.Printf("main=%s succeeded=%d database=%s\n", progress.Status, progress.Subs.Succeeded, path)
	return nil
}

func main() {
	path := flag.String("db", "tflow-reference.db", "SQLite file path")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := run(ctx, *path); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
