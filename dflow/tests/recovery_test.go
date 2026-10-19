package tests

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/dflow/sqlite"
	"github.com/k0923/go/lease"
)

func suspendedSnapshot() *dflow.State {
	return &dflow.State{Nodes: map[string]dflow.NodeState{
		"node": {Status: dflow.Suspended, Suspension: &dflow.SuspensionState{
			Token: "token-1", Payload: &dflow.ValueSuspension{Kind: "external", Data: "saved"},
		}},
	}}
}

func TestSQLiteConcurrentResolveAndFencing(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "concurrent.db")
	left, err := sqlite.Open(ctx, path, "run", fixtureCodec{})
	if err != nil {
		t.Fatal(err)
	}
	defer left.Close()
	right, err := sqlite.Open(ctx, path, "run", fixtureCodec{})
	if err != nil {
		t.Fatal(err)
	}
	defer right.Close()
	if err := right.Initialize(ctx, dflow.Manifest{Hash: "v1", Roots: []string{"node"}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := left.Initialize(ctx, dflow.Manifest{Hash: "v1", Roots: []string{"node"}}, nil); err != nil {
		t.Fatal("concurrent initialize must be idempotent", err)
	}
	old, err := left.ClaimNode(ctx, "node", time.Minute)
	if err != nil || old == nil {
		t.Fatal(old, err)
	}
	if task, err := right.ClaimNode(ctx, "node", time.Minute); err != nil || task != nil {
		t.Fatal("multiple owners", task, err)
	}
	if err := left.ReleaseNode(ctx, old.Lease); err != nil {
		t.Fatal(err)
	}
	task, err := right.ClaimNode(ctx, "node", time.Minute)
	if err != nil || task == nil {
		t.Fatal(task, err)
	}
	state := suspendedSnapshot()
	commit := dflow.NodeCommit{NodeID: "node", ExpectedVersion: task.State.Version, State: state.Nodes["node"]}
	if _, err := left.Commit(ctx, old.Lease, commit); !errors.Is(err, lease.ErrConflict) {
		t.Fatal("stale writer accepted", err)
	}
	if _, err := right.Commit(ctx, task.Lease, commit); err != nil {
		t.Fatal(err)
	}
	if _, err := right.Commit(ctx, task.Lease, commit); !errors.Is(err, dflow.ErrConflict) {
		t.Fatal("stale snapshot accepted", err)
	}
	type response struct {
		accepted bool
		err      error
	}
	results := make(chan response, 2)
	for _, store := range []*sqlite.Infrastructure{left, right} {
		go func() {
			ok, err := store.Resolve(ctx, "node", "token-1", "response")
			results <- response{ok, err}
		}()
	}
	winners := 0
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.accepted {
			winners++
		}
	}
	// An old snapshot saved after callback arrival must not overwrite it.
	if _, err := right.Commit(ctx, task.Lease, commit); !errors.Is(err, dflow.ErrConflict) {
		t.Fatal(err)
	}
	messages, err := left.Pending(ctx)
	if err != nil || winners != 1 || len(messages) != 2 {
		t.Fatalf("resolve not atomic: winners=%d messages=%v err=%v", winners, messages, err)
	}
	saved, err := left.GetResponse(ctx, "node", "token-1")
	if err != nil || saved == nil || saved.Value != "response" {
		t.Fatal("snapshot erased response", saved, err)
	}
	other, err := sqlite.Open(ctx, path, "other-run", fixtureCodec{})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.GetResponse(ctx, "node", "token-1"); !errors.Is(err, dflow.ErrWaitNotFound) {
		t.Fatal("scope leaked", err)
	}
	if err := other.Ack(ctx, messages[0].ID); err != nil {
		t.Fatal(err)
	}
	if pending, err := left.Pending(ctx); err != nil || len(pending) != 2 {
		t.Fatal("foreign scope acknowledged message", pending, err)
	}
}

func TestSQLiteOutboxFailuresRollbackNodeState(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "rollback.db")
	store, err := sqlite.Open(ctx, path, "run", fixtureCodec{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER reject_notification BEFORE INSERT ON dflow_outbox
BEGIN SELECT RAISE(ABORT, 'injected outbox failure'); END`); err != nil {
		t.Fatal(err)
	}
	state := suspendedSnapshot()
	if err := store.Initialize(ctx, dflow.Manifest{Hash: "v1", Roots: []string{"node"}}, nil); err != nil {
		t.Fatal(err)
	}
	task, err := store.ClaimNode(ctx, "node", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	commit := dflow.NodeCommit{NodeID: "node", ExpectedVersion: task.State.Version, State: state.Nodes["node"]}
	if _, err := store.Commit(ctx, task.Lease, commit); err == nil {
		t.Fatal("expected request notification failure")
	}
	if loaded, err := store.GetNode(ctx, "node"); err != nil || loaded.Status != dflow.Running {
		t.Fatal("suspension committed without request", loaded, err)
	}
	if _, err := store.GetResponse(ctx, "node", "token-1"); !errors.Is(err, dflow.ErrWaitNotFound) {
		t.Fatal("wait registration leaked from transaction", err)
	}
	if _, err := db.Exec(`DROP TRIGGER reject_notification`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit(ctx, task.Lease, commit); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_notification BEFORE INSERT ON dflow_outbox
BEGIN SELECT RAISE(ABORT, 'injected wake failure'); END`); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.Resolve(ctx, "node", "token-1", "answer"); err == nil || ok {
		t.Fatal("expected wake notification failure", ok, err)
	}
	response, err := store.GetResponse(ctx, "node", "token-1")
	if err != nil || response != nil {
		t.Fatal("response committed without wake", response, err)
	}
}

// Exit immediately after the suspension transaction commits, before Engine.Save
// can return and before lease cleanup. Simulates uncertain commit on process loss.
type crashAfterSuspend struct{ *sqlite.Infrastructure }

func (s *crashAfterSuspend) Commit(ctx context.Context, token lease.Token, commit dflow.NodeCommit) ([]string, error) {
	ready, err := s.Infrastructure.Commit(ctx, token, commit)
	if err != nil {
		return nil, err
	}
	if commit.NodeID == "wait" && commit.State.Status == dflow.Suspended {
		os.Exit(23)
	}
	return ready, nil
}

func crashDefinition() dflow.Definition {
	def := waitDefinition()
	def.Nodes = append([]dflow.NodeSpec{{ID: "before", Node: namedNode("before")}}, def.Nodes...)
	def.Edges = append(def.Edges, edge("before", "wait"))
	return def
}

func TestSQLiteCrashRecovery(t *testing.T) {
	if path := os.Getenv("DFLOW_CRASH_TEST_PATH"); path != "" {
		ctx := context.Background()
		store, err := sqlite.Open(ctx, path, "crash-run", fixtureCodec{})
		if err != nil {
			t.Fatal(err)
		}
		registry := asyncRegistry(t, asyncNode{
			run: func(context.Context, map[string]any) (dflow.Result, error) {
				return dflow.Result{Suspend: &dflow.ValueSuspension{Kind: "external", Data: "saved"}}, nil
			},
			resume: func(context.Context, map[string]any, dflow.ResumeInput) (dflow.Result, error) {
				// Simulate loss during Resume after its running state was committed.
				os.Exit(24)
				return dflow.Result{}, nil
			},
		})
		register(t, registry, "before", func(context.Context, map[string]any) (dflow.Result, error) {
			return dflow.Result{Output: "upstream-saved"}, nil
		})
		var infra dflow.Infrastructure = store
		if os.Getenv("DFLOW_CRASH_STAGE") == "suspend" {
			infra = &crashAfterSuspend{store}
		}
		e := engine(t, crashDefinition(), registry, infra, dflow.Options{Lease: lease.Options{TTL: 100 * time.Millisecond}})
		driveAfterLeaseExpires(t, e)
		t.Fatal("child did not exit")
	}
	path := filepath.Join(t.TempDir(), "crash.db")
	runChild := func(stage string, exitCode int) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestSQLiteCrashRecovery$")
		cmd.Env = append(os.Environ(), "DFLOW_CRASH_TEST_PATH="+path, "DFLOW_CRASH_STAGE="+stage)
		output, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != exitCode {
			t.Fatalf("unexpected child exit: %v\n%s", err, output)
		}
	}
	runChild("suspend", 23)
	ctx := context.Background()
	store, err := sqlite.Open(ctx, path, "crash-run", fixtureCodec{})
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Load(ctx)
	if err != nil || state.Nodes["wait"].Status != dflow.Suspended || state.Nodes["before"].Output != "upstream-saved" {
		t.Fatal(state, err)
	}
	token := state.Nodes["wait"].Suspension.Token
	if accepted, err := store.Resolve(ctx, "wait", token, "response"); err != nil || !accepted {
		t.Fatal(accepted, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	runChild("resume", 24)
	store, err = sqlite.Open(ctx, path, "crash-run", fixtureCodec{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	state, err = store.Load(ctx)
	if err != nil || state.Nodes["wait"].Status != dflow.Running || state.Nodes["wait"].Suspension.Token != token {
		t.Fatal("interrupted Resume lost state", state, err)
	}
	pending, err := store.Pending(ctx)
	if err != nil || len(pending) != 2 {
		t.Fatal("durable request/wake was lost", pending, err)
	}
	registry := asyncRegistry(t, asyncNode{
		run: func(context.Context, map[string]any) (dflow.Result, error) {
			return dflow.Result{}, errors.New("Run must not execute again")
		},
		resume: func(_ context.Context, input map[string]any, resume dflow.ResumeInput) (dflow.Result, error) {
			if input["original"] != "input" || resume.Suspension.(*dflow.ValueSuspension).Data != "saved" || resume.Token != token {
				return dflow.Result{}, errors.New("missing continuation/input")
			}
			return dflow.Result{Output: resume.Response}, nil
		},
	})
	register(t, registry, "before", func(context.Context, map[string]any) (dflow.Result, error) {
		return dflow.Result{}, errors.New("successful upstream node must not run again")
	})
	e := engine(t, crashDefinition(), registry, store, dflow.Options{})
	state = driveAfterLeaseExpires(t, e)
	if state.Nodes["end"].Status != dflow.Succeeded || state.Nodes["wait"].Output != "response" {
		t.Fatal(state)
	}
}

// driveAfterLeaseExpires polls until the node lease left by a crashed attempt
// expires: until then the node is owned by a live lease and drives nothing.
func driveAfterLeaseExpires(t *testing.T, e *observedEngine) *dflow.State {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, err := e.Run(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if state.Nodes["end"].Status == dflow.Succeeded {
			return state
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}
