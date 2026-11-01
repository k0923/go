package debugger

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/k0923/go/cflow"
)

func TestRunResumesLoopItemsWithLostCommitAndAck(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "handled_failure"}[fail], func(t *testing.T) {
			f := newFixture(t)
			f.store.now = time.Now
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var commits, acks, roots, actions atomic.Int32
			handler := NewServer(f.store)
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/infra/commit_flow") && commits.Add(1) == 1 {
					// Commit transaction, then lose the response on the wire.
					recorder := httptest.NewRecorder()
					handler.ServeHTTP(recorder, r)
					if recorder.Code != http.StatusOK {
						t.Errorf("commit status %d: %s", recorder.Code, recorder.Body.String())
					}
					http.Error(w, "response lost", http.StatusServiceUnavailable)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/infra/ack") && acks.Add(1) == 1 {
					http.Error(w, "ack unavailable", http.StatusServiceUnavailable)
					return
				}
				handler.ServeHTTP(w, r)
			}))
			defer proxy.Close()
			body := cflow.FlowFunc[string, string](func(fc *cflow.FlowContext, id string) (string, error) {
				if _, err := fc.Run("submit", cflow.AsAction(func(context.Context, string) (string, error) {
					actions.Add(1)
					if fail && id == "b" {
						return "", cflow.Permanent(errors.New("rejected"))
					}
					return id, nil
				}), id); err != nil {
					return "", err
				}
				return fc.WaitFor[string]("approval", id), nil
			})
			var caught error
			wf := cflow.FlowFunc[int, []string](func(fc *cflow.FlowContext, _ int) ([]string, error) {
				roots.Add(1)
				out, err := fc.ForEach("orders", body, []string{"a", "b"}, func(id string) string { return id })
				caught = err
				return out, nil
			})
			type outcome struct {
				result cflow.Result[[]string]
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := Run(ctx, wf, 0, Options{Server: proxy.URL, RunID: "loop",
					Engine:     cflow.Options{Flows: map[string]cflow.FlowDefinition{"item": body, "batch": wf}},
					RetryDelay: time.Millisecond, Logf: func(string, ...any) {}})
				done <- outcome{result, err}
			}()
			client, _ := NewClient(proxy.URL, "loop")
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case out := <-done:
					want := []string{"approved", "approved"}
					completed := 2
					if fail {
						want, completed = nil, 1
					}
					if out.err != nil || out.result.Status != cflow.Completed || !slices.Equal(out.result.Output, want) ||
						roots.Load() != 2 || actions.Load() != 2 || (fail != (caught != nil)) {
						t.Fatalf("result=%+v err=%v roots=%d actions=%d caught=%v", out.result, out.err, roots.Load(), actions.Load(), caught)
					}
					loop, err := client.GetLoop(ctx, "orders")
					if err != nil || loop.CompletedCount != completed {
						t.Fatalf("%+v %v", loop, err)
					}
					detail, err := client.Detail(ctx)
					if err != nil || detail.Run.Status != string(cflow.Completed) {
						t.Fatalf("%+v %v", detail, err)
					}
					return
				case <-ticker.C:
					for _, id := range []string{"a", "b"} {
						_, _ = client.Submit(ctx, "orders|item["+id+"]|approval", "approved")
					}
				case <-ctx.Done():
					t.Fatal("dispatcher stalled:", ctx.Err())
				}
			}
		})
	}
}

func TestRunReportsEveryBoundaryPending(t *testing.T) {
	f := newFixture(t)
	f.store.now = time.Now
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	server := httptest.NewServer(NewServer(f.store))
	defer server.Close()
	body := cflow.FlowFunc[string, string](func(fc *cflow.FlowContext, id string) (string, error) {
		return fc.WaitFor[string]("approval", id), nil
	})
	wf := cflow.FlowFunc[int, []string](func(fc *cflow.FlowContext, _ int) ([]string, error) {
		return fc.ForEach("orders", body, []string{"a", "b"}, func(id string) string { return id }, cflow.Concurrency(2))
	})
	done := make(chan error, 1)
	go func() {
		_, err := Run(ctx, wf, 0, Options{Server: server.URL, RunID: "loop-pending",
			Engine:     cflow.Options{Flows: map[string]cflow.FlowDefinition{"item": body, "batch": wf}},
			RetryDelay: time.Millisecond, Logf: func(string, ...any) {}})
		done <- err
	}()
	client, _ := NewClient(server.URL, "loop-pending")
	want := []string{"orders", "orders|item[a]|approval", "orders|item[b]|approval"}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			t.Fatal("run finished before both items suspended:", err)
		case <-ctx.Done():
			t.Fatal("pending never aggregated:", ctx.Err())
		case <-ticker.C:
		}
		detail, err := client.Detail(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var keys []string
		for _, pending := range detail.Run.Pending {
			keys = append(keys, pending.Key)
		}
		if slices.Equal(keys, want) {
			break
		}
	}
	for _, id := range []string{"a", "b"} {
		if ok, err := client.Submit(ctx, "orders|item["+id+"]|approval", "approved"); !ok || err != nil {
			t.Fatalf("%t %v", ok, err)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	detail, err := client.Detail(ctx)
	if err != nil || detail.Run.Status != string(cflow.Completed) || len(detail.Run.Pending) != 0 {
		t.Fatalf("%+v %v", detail.Run, err)
	}
}

func TestLoopWaitAnyWakeTargetsItem(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, client := f.engine(t, "loop-wait-any")
	body := cflow.FlowFunc[string, string](func(fc *cflow.FlowContext, id string) (string, error) {
		a := cflow.OnSignal[string]("a", id)
		b := cflow.OnSignal[string]("b", id)
		winner := fc.WaitAny("choice", a, b)
		if winner == a {
			return a.Value(), nil
		}
		return b.Value(), nil
	})
	wf := cflow.FlowFunc[int, []string](func(fc *cflow.FlowContext, _ int) ([]string, error) {
		return fc.ForEach("orders", body, []string{"a"}, func(id string) string { return id })
	})
	engine, err := cflow.NewEngine(client, cflow.Options{Flows: map[string]cflow.FlowDefinition{"item": body, "batch": wf}})
	if err != nil {
		t.Fatal(err)
	}
	if r, err := engine.Start(ctx, wf, 0); err != nil || r.Status != cflow.Suspended {
		t.Fatalf("%+v %v", r, err)
	}
	key := "orders|item[a]"
	if r, err := engine.Resume(ctx, key); err != nil || r.Status != cflow.Suspended {
		t.Fatalf("%+v %v", r, err)
	}
	if ok, err := client.Submit(ctx, key+"|choice|b", "winner"); !ok || err != nil {
		t.Fatalf("%t %v", ok, err)
	}
	wakes, err := client.WaitWake(ctx, time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, wake := range wakes {
		if wake.Key == key+"|choice|b" {
			found = true
			if wake.ExecutionKey != key {
				t.Fatalf("wrong route: %+v", wake)
			}
		}
	}
	if !found {
		raw, _ := json.Marshal(wakes)
		t.Fatalf("missing wake: %s", raw)
	}
	if r, err := engine.Resume(ctx, key); err != nil || r.Status != cflow.Completed || string(r.Output) != `"winner"` {
		t.Fatalf("%+v %v", r, err)
	}
}
