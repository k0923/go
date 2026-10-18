package debugger

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/k0923/go/cflow"
)

func TestWriterLockPrecedesLeaseClock(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	token, err := f.store.Claim(ctx, "locked", "action", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// A second connection to the same database holds the actual SQLite lock.
	var path string
	if err := f.store.db.QueryRow("SELECT file FROM pragma_database_list WHERE name = 'main'").Scan(&path); err != nil {
		t.Fatal(err)
	}
	blocker, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	tx, err := blocker.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("UPDATE runs SET id = id WHERE id = 'locked'"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.store.Commit(ctx, "locked", *token,
			&cflow.Record{Key: "action", Kind: cflow.ActionKind, Status: cflow.Running}, 0)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("commit bypassed writer lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	f.advance(2 * time.Minute)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, cflow.ErrConflict) {
		t.Fatalf("expired lease accepted after lock wait: %v", err)
	}
	if record, err := f.store.Get(ctx, "locked", "action"); err != nil || record != nil {
		t.Fatalf("rejected commit changed record: %+v %v", record, err)
	}
}

func TestDeletedRunCannotBeResurrected(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	token, err := f.store.Claim(ctx, "deleted", "action", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Delete(ctx, "deleted"); err != nil {
		t.Fatal(err)
	}
	client, _ := NewClient(f.server.URL, "deleted")
	checks := []func() error{
		func() error { _, err := client.WaitWake(ctx, time.Nanosecond); return err },
		func() error { _, err := f.store.Register(ctx, RunSpec{ID: "deleted"}); return err },
		func() error { _, err := f.store.Claim(ctx, "deleted", "action", time.Minute); return err },
		func() error {
			_, err := f.store.Commit(ctx, "deleted", *token,
				&cflow.Record{Key: "action", Kind: cflow.ActionKind, Status: cflow.Running}, 0)
			return err
		},
		func() error { return f.store.Report(ctx, "deleted", Report{Status: RunRetrying}) },
	}
	for i, check := range checks {
		if err := check(); !errors.Is(err, cflow.ErrNotFound) {
			t.Fatalf("operation %d resurrected run: %v", i, err)
		}
	}
	if runs, err := f.store.ListRuns(ctx); err != nil || len(runs) != 0 {
		t.Fatalf("deleted run visible: %+v %v", runs, err)
	}
}

func TestWaitAnyEncodingFailureMetadataOverHTTP(t *testing.T) {
	f := newFixture(t)
	_, client := f.engine(t, "metadata")
	ctx := context.Background()
	requests := []cflow.WaitRequest{
		{Key: "bad", Kind: cflow.SignalKind, Error: "cannot encode"},
		{Key: "good", Kind: cflow.SignalKind, Request: json.RawMessage(`1`)},
	}
	state, err := client.WaitAny(ctx, "group", requests)
	if err != nil || len(state.Failures) != 1 || len(state.Pending) != 1 {
		t.Fatalf("%+v %v", state, err)
	}
	requests[0].Key = "changed"
	if _, err := client.WaitAny(ctx, "group", requests); !errors.Is(err, cflow.ErrChanged) {
		t.Fatalf("failed member identity not enforced: %v", err)
	}
	requests[0].Kind = cflow.Kind("unknown")
	if _, err := client.WaitAny(ctx, "other", requests[:1]); !errors.Is(err, cflow.ErrSignalUnsupported) {
		t.Fatalf("failed member bypassed kind validation: %v", err)
	}
	requests[0].Kind = cflow.SignalKind
	state, err = client.WaitAny(ctx, "all-failed", requests[:1])
	if err != nil || state.Status != cflow.Failed {
		t.Fatalf("all failed group: %+v %v", state, err)
	}
	wakes, err := client.WaitWake(ctx, time.Nanosecond)
	if err != nil || len(wakes) != 1 || wakes[0].Key != "all-failed" {
		t.Fatalf("all failed group wake: %+v %v", wakes, err)
	}
}

func TestWakeRetainedAcrossDriveAndAckFailures(t *testing.T) {
	f := newFixture(t)
	f.store.now = time.Now
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.store.Redrive(ctx, "ack-order"); err != nil {
		t.Fatal(err)
	}
	var calls, acks atomic.Int32
	var earlyAck atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/infra/ack") {
			if calls.Load() < 2 {
				earlyAck.Store(true)
			}
			if acks.Add(1) == 1 {
				http.Error(w, "temporary ack failure", http.StatusServiceUnavailable)
				return
			}
		}
		NewServer(f.store).ServeHTTP(w, r)
	}))
	defer proxy.Close()
	flow := definition(func(fc *cflow.FlowContext, input int) (int, error) {
		return fc.Run("retry", cflow.AsAction(func(context.Context, int) (int, error) {
			if calls.Add(1) == 1 {
				return 0, errors.New("temporary action failure")
			}
			return input, nil
		}), input)
	})
	result, err := Run(ctx, flow, 42, Options{Server: proxy.URL, RunID: "ack-order",
		RetryDelay: time.Millisecond, Logf: func(string, ...any) {}})
	if err != nil || result.Output != 42 || earlyAck.Load() || calls.Load() != 2 || acks.Load() != 2 {
		t.Fatalf("result=%+v err=%v early=%t calls=%d acks=%d", result, err, earlyAck.Load(), calls.Load(), acks.Load())
	}
	wakes, _, err := f.store.nextWake(ctx, "ack-order")
	if err != nil || len(wakes) != 0 {
		t.Fatalf("wake not acknowledged: %+v %v", wakes, err)
	}
}

func TestStaleSuspensionRechecksDurableWait(t *testing.T) {
	f := newFixture(t)
	engine, client := f.engine(t, "stale")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	flow := definition(func(fc *cflow.FlowContext, input int) (int, error) {
		return fc.WaitFor[int]("approval", input), nil
	})
	result, err := engine.Drive(ctx, flow, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Submit(ctx, "main|approval", 2); err != nil {
		t.Fatal(err)
	}
	wakes, err := client.WaitWake(ctx, time.Nanosecond)
	if err != nil || len(wakes) != 1 {
		t.Fatalf("%+v %v", wakes, err)
	}
	if err := client.Ack(ctx, wakes[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := waitForWake(ctx, client, result.Pending); err != nil {
		t.Fatalf("stale worker waited for already consumed wake: %v", err)
	}
}

func TestAutomaticRunIDsIsolateConcurrentRuns(t *testing.T) {
	f := newFixture(t)
	f.store.now = time.Now
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	flow := definition(func(_ *cflow.FlowContext, input int) (int, error) { return input, nil })
	const count = 16
	done := make(chan error, count)
	for i := range count {
		go func() {
			result, err := Run(ctx, flow, i, Options{Server: f.server.URL, Logf: func(string, ...any) {}})
			if err == nil && result.Output != i {
				err = errors.New("output from another run")
			}
			done <- err
		}()
	}
	for range count {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	runs, err := f.store.ListRuns(ctx)
	if err != nil || len(runs) != count {
		t.Fatalf("concurrent runs collided: count=%d err=%v", len(runs), err)
	}
}

func TestEventRetryDeduplicatesUnknownDelivery(t *testing.T) {
	f := newFixture(t)
	var posts atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") && r.Method == http.MethodPost && posts.Add(1) == 1 {
			var batch []json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
				t.Error(err)
			}
			if err := f.store.AppendEvents(r.Context(), "events", batch); err != nil {
				t.Error(err)
			}
			http.Error(w, "response lost after commit", http.StatusServiceUnavailable)
			return
		}
		NewServer(f.store).ServeHTTP(w, r)
	}))
	defer proxy.Close()
	client, _ := NewClient(proxy.URL, "events")
	var logs atomic.Int32
	forwarder := newForwarder(client, func(string, ...any) { logs.Add(1) })
	forwarder.add(context.Background(), cflow.Event{ID: "same-event"})
	forwarder.flush()
	forwarder.close()
	events, err := f.store.Events(context.Background(), "events", 0, 0)
	if err != nil || len(events) != 1 || posts.Load() != 2 || logs.Load() == 0 {
		t.Fatalf("events=%d posts=%d logs=%d err=%v", len(events), posts.Load(), logs.Load(), err)
	}
}

func TestServerCancellationClosesActiveSSE(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- ListenAndServe(ctx, addr, filepath.Join(t.TempDir(), "sse.db")) }()
	client := &http.Client{Timeout: 2 * time.Second}
	var response *http.Response
	deadline := time.Now().Add(2 * time.Second)
	for {
		response, err = client.Get("http://" + addr + "/api/stream")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not join shutdown")
	}
	for {
		if _, err := reader.ReadString('\n'); err != nil {
			break
		}
	}
}

func TestEventOutageRetainsBatchAndReportsIncompleteTimeline(t *testing.T) {
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, "outage")
	var incomplete atomic.Bool
	f := newForwarder(client, func(format string, _ ...any) {
		if strings.Contains(format, "timeline incomplete") {
			incomplete.Store(true)
		}
	})
	f.add(context.Background(), cflow.Event{ID: "retained"})
	f.flush()
	f.mu.Lock()
	buffered := len(f.buffer)
	f.mu.Unlock()
	if buffered != 1 {
		t.Fatalf("failed batch discarded: %d", buffered)
	}
	f.close()
	if posts.Load() < 6 || !incomplete.Load() {
		t.Fatalf("outage not retried or reported: posts=%d incomplete=%t", posts.Load(), incomplete.Load())
	}
}

func TestStoreRejectsDatabaseWithoutPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// The records table as it existed before Record.Path.
	// Record.Path 引入之前的 records 表结构。
	_, err = db.Exec(`CREATE TABLE records (run_id TEXT NOT NULL, key TEXT NOT NULL, kind TEXT NOT NULL,
		status TEXT NOT NULL, version INTEGER NOT NULL, input BLOB, result BLOB, error TEXT NOT NULL DEFAULT '',
		until INTEGER NOT NULL DEFAULT 0, started_at INTEGER NOT NULL DEFAULT 0, ended_at INTEGER NOT NULL DEFAULT 0,
		updated_at INTEGER NOT NULL DEFAULT 0, group_key TEXT NOT NULL DEFAULT '', type_path TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (run_id, key))`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if store, err := OpenStore(path); err == nil || !strings.Contains(err.Error(), "predates Record.Path") {
		if store != nil {
			store.Close()
		}
		t.Fatalf("old schema must be rejected: %v", err)
	}
}

func TestStorePersistsPath(t *testing.T) {
	f := newFixture(t)
	_, client := f.engine(t, "pathrun")
	ctx := context.Background()
	want := cflow.Path{{Name: "agent", ID: "a|b"}, {Name: "approve"}}
	if _, err := client.Put(ctx, &cflow.Record{Key: cflow.DefaultKey(want), Path: want, Kind: cflow.SignalKind,
		Status: cflow.Suspended, Input: json.RawMessage(`1`)}); err != nil {
		t.Fatal(err)
	}
	record, err := client.Get(ctx, `agent[a\|b]|approve`)
	if err != nil || record == nil || !record.Path.Equal(want) {
		t.Fatalf("%+v %v", record, err)
	}
}
