package debugger

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/k0923/go/workflow"
	"github.com/k0923/go/workflow/memory"
)

//go:embed ui
var uiFiles embed.FS

var runIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// Server exposes the Store over HTTP: Infrastructure RPC for workers, a JSON
// API and SSE stream for the UI, and the UI itself.
// Server 通过 HTTP 暴露 Store：面向 worker 的 Infrastructure RPC、面向 UI 的
// JSON API 和 SSE 流，以及 UI 本身。
type Server struct {
	store *Store
	mux   *http.ServeMux
}

func NewServer(store *Store) *Server {
	s := &Server{store: store, mux: http.NewServeMux()}
	ui, _ := fs.Sub(uiFiles, "ui")
	s.mux.Handle("GET /", http.FileServerFS(ui))

	s.mux.HandleFunc("GET /api/runs", s.listRuns)
	s.mux.HandleFunc("POST /api/runs", s.registerRun)
	s.mux.HandleFunc("GET /api/stream", s.stream)
	s.mux.HandleFunc("GET /api/runs/{run}", s.withRun(s.detail))
	s.mux.HandleFunc("DELETE /api/runs/{run}", s.withRun(s.deleteRun))
	s.mux.HandleFunc("GET /api/runs/{run}/events", s.withRun(s.events))
	s.mux.HandleFunc("POST /api/runs/{run}/events", s.withRun(s.appendEvents))
	s.mux.HandleFunc("POST /api/runs/{run}/report", s.withRun(s.report))
	s.mux.HandleFunc("GET /api/runs/{run}/wake", s.withRun(s.wake))
	s.mux.HandleFunc("POST /api/runs/{run}/submit", s.withRun(s.submit))
	s.mux.HandleFunc("POST /api/runs/{run}/fire", s.withRun(s.fire))
	s.mux.HandleFunc("POST /api/runs/{run}/fail", s.withRun(s.fail))
	s.mux.HandleFunc("POST /api/runs/{run}/redrive", s.withRun(s.redrive))
	s.mux.HandleFunc("POST /api/runs/{run}/infra/{op}", s.withRun(s.infra))
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) withRun(handler func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		run := r.PathValue("run")
		if !runIDPattern.MatchString(run) {
			writeError(w, fmt.Errorf("%w: run id %q", cflow.ErrInvalid, run))
			return
		}
		handler(w, r, run)
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// errorCodes is ordered: a joined error reports its most specific sentinel.
// errorCodes 是有序的：joined error 会报告其最具体的哨兵错误。
var errorCodes = []struct {
	code   string
	err    error
	status int
}{
	{"conflict", cflow.ErrConflict, http.StatusConflict},
	{"not_found", cflow.ErrNotFound, http.StatusNotFound},
	{"changed", cflow.ErrChanged, http.StatusConflict},
	{"signal_unsupported", cflow.ErrSignalUnsupported, http.StatusBadRequest},
	{"wait_any_unsupported", cflow.ErrWaitAnyUnsupported, http.StatusBadRequest},
	{"invalid", cflow.ErrInvalid, http.StatusBadRequest},
	{"canceled", context.Canceled, 499},
	{"deadline", context.DeadlineExceeded, http.StatusGatewayTimeout},
}

func writeError(w http.ResponseWriter, err error) {
	for _, candidate := range errorCodes {
		if errors.Is(err, candidate.err) {
			writeJSON(w, candidate.status, apiError{Code: candidate.code, Message: err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusInternalServerError, apiError{Code: "internal", Message: err.Error()})
}

func decode(r *http.Request, value any) error {
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 16<<20)).Decode(value); err != nil {
		return fmt.Errorf("%w: %v", cflow.ErrInvalid, err)
	}
	return nil
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.store.ListRuns(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

func (s *Server) registerRun(w http.ResponseWriter, r *http.Request) {
	var spec RunSpec
	if err := decode(r, &spec); err != nil {
		writeError(w, err)
		return
	}
	if spec.ID != "" && !runIDPattern.MatchString(spec.ID) {
		writeError(w, fmt.Errorf("%w: run id %q", cflow.ErrInvalid, spec.ID))
		return
	}
	info, err := s.store.Register(r.Context(), spec)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) detail(w http.ResponseWriter, r *http.Request, run string) {
	detail, err := s.store.Detail(r.Context(), run)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) deleteRun(w http.ResponseWriter, r *http.Request, run string) {
	if err := s.store.Delete(r.Context(), run); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, boolResponse{OK: true})
}

func (s *Server) events(w http.ResponseWriter, r *http.Request, run string) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := s.store.Events(r.Context(), run, after, limit)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

func (s *Server) appendEvents(w http.ResponseWriter, r *http.Request, run string) {
	var events []json.RawMessage
	if err := decode(r, &events); err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.AppendEvents(r.Context(), run, events); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, boolResponse{OK: true})
}

func (s *Server) report(w http.ResponseWriter, r *http.Request, run string) {
	var report Report
	if err := decode(r, &report); err != nil {
		writeError(w, err)
		return
	}
	sortPending(report.Pending)
	if err := s.store.Report(r.Context(), run, report); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, boolResponse{OK: true})
}

// wake long-polls for due WAKE notifications of one run. It returns as soon
// as one is due, when a scheduled one matures, or after the timeout.
// wake 对单个运行的到期 WAKE 通知进行长轮询。当有通知到期、计划通知成熟、
// 或超时后立即返回。
func (s *Server) wake(w http.ResponseWriter, r *http.Request, run string) {
	timeout, err := time.ParseDuration(r.URL.Query().Get("timeout"))
	if err != nil || timeout <= 0 || timeout > time.Minute {
		timeout = 20 * time.Second
	}
	updates, unsubscribe := s.store.hub.subscribe()
	defer unsubscribe()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		s.store.Touch(run)
		due, next, err := s.store.nextWake(r.Context(), run)
		if err != nil {
			writeError(w, err)
			return
		}
		if len(due) > 0 {
			writeJSON(w, http.StatusOK, notificationsResponse{Notifications: due})
			return
		}
		var mature <-chan time.Time
		if !next.IsZero() {
			timer := time.NewTimer(time.Until(next))
			defer timer.Stop()
			mature = timer.C
		}
		heartbeat := time.NewTimer(3 * time.Second)
	wait:
		for {
			select {
			case <-r.Context().Done():
				heartbeat.Stop()
				return
			case <-deadline.C:
				heartbeat.Stop()
				writeJSON(w, http.StatusOK, notificationsResponse{Notifications: []memory.Notification{}})
				return
			case <-mature:
				break wait
			case <-heartbeat.C:
				break wait
			case changed := <-updates:
				if changed == run {
					break wait
				}
			}
		}
		heartbeat.Stop()
	}
}

type submitRequest struct {
	Key      string          `json:"key"`
	Response json.RawMessage `json:"response"`
	Message  string          `json:"message"`
}

func (s *Server) submit(w http.ResponseWriter, r *http.Request, run string) {
	var body submitRequest
	if err := decode(r, &body); err != nil {
		writeError(w, err)
		return
	}
	accepted, err := s.store.Submit(r.Context(), run, body.Key, body.Response)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, boolResponse{OK: accepted})
}

func (s *Server) fire(w http.ResponseWriter, r *http.Request, run string) {
	var body submitRequest
	if err := decode(r, &body); err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.FireSleep(r.Context(), run, body.Key); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, boolResponse{OK: true})
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, run string) {
	var body submitRequest
	if err := decode(r, &body); err != nil {
		writeError(w, err)
		return
	}
	if body.Message == "" {
		body.Message = "failed from debugger"
	}
	failed, err := s.store.FailMember(r.Context(), run, body.Key, body.Message)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, boolResponse{OK: failed})
}

func (s *Server) redrive(w http.ResponseWriter, r *http.Request, run string) {
	if err := s.store.Redrive(r.Context(), run); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, boolResponse{OK: true})
}

// stream pushes the IDs of changed runs; the UI refetches what it shows.
// stream 推送发生变化的运行 ID；UI 会重新拉取当前展示的内容。
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	updates, unsubscribe := s.store.hub.subscribe()
	defer unsubscribe()
	fmt.Fprint(w, "retry: 2000\n\n")
	flusher.Flush()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case run := <-updates:
			data, _ := json.Marshal(run)
			fmt.Fprintf(w, "event: run\ndata: %s\n\n", data)
			flusher.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) infra(w http.ResponseWriter, r *http.Request, run string) {
	ctx := r.Context()
	var (
		response any
		err      error
	)
	switch op := r.PathValue("op"); op {
	case "get":
		var body keyRequest
		if err = decode(r, &body); err == nil {
			var record *cflow.Record
			record, err = s.store.Get(ctx, run, body.Key)
			response = recordResponse{Record: toView(record, "")}
		}
	case "claim":
		var body claimRequest
		if err = decode(r, &body); err == nil {
			var lease *cflow.Lease
			lease, err = s.store.Claim(ctx, run, body.Key, body.TTL)
			response = leaseResponse{Lease: lease}
		}
	case "renew":
		var body leaseRequest
		if err = decode(r, &body); err == nil {
			var lease *cflow.Lease
			lease, err = s.store.Renew(ctx, run, body.Lease, body.TTL)
			response = leaseResponse{Lease: lease}
		}
	case "release":
		var body leaseRequest
		if err = decode(r, &body); err == nil {
			err = s.store.Release(ctx, run, body.Lease)
			response = boolResponse{OK: true}
		}
	case "commit":
		var body commitRequest
		if err = decode(r, &body); err == nil {
			var record *cflow.Record
			record, err = s.store.Commit(ctx, run, body.Lease, fromView(&body.Record), body.Expected)
			response = recordResponse{Record: toView(record, "")}
		}
	case "put":
		var body putRequest
		if err = decode(r, &body); err == nil {
			var record *cflow.Record
			record, err = s.store.Put(ctx, run, fromView(&body.Record))
			response = recordResponse{Record: toView(record, "")}
		}
	case "resolve":
		var body resolveRequest
		if err = decode(r, &body); err == nil {
			var ok bool
			ok, err = s.store.Resolve(ctx, run, body.Key, body.Version, body.Response)
			response = boolResponse{OK: ok}
		}
	case "pending":
		var body pendingRequest
		if err = decode(r, &body); err == nil {
			var notifications []memory.Notification
			notifications, err = s.store.Pending(ctx, run, body.Now)
			response = notificationsResponse{Notifications: notifications}
		}
	case "ack":
		var body ackRequest
		if err = decode(r, &body); err == nil {
			err = s.store.Ack(ctx, run, body.ID)
			response = boolResponse{OK: true}
		}
	case "wait_any":
		var body waitAnyRequest
		if err = decode(r, &body); err == nil {
			var state *cflow.WaitAnyState
			state, err = s.store.WaitAny(ctx, run, body.Key, body.Requests)
			response = stateResponse{State: state}
		}
	case "fail_wait":
		var body failWaitRequest
		if err = decode(r, &body); err == nil {
			var ok bool
			ok, err = s.store.FailWait(ctx, run, body.Key, body.Version, body.Message)
			response = boolResponse{OK: ok}
		}
	default:
		err = fmt.Errorf("%w: unknown operation %q", cflow.ErrInvalid, op)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// ListenAndServe opens the database and serves until ctx is cancelled.
// ListenAndServe 打开数据库并提供服务，直到 ctx 被取消。
func ListenAndServe(ctx context.Context, addr, dbPath string) error {
	store, err := OpenStore(dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	serviceCtx, stop := context.WithCancel(ctx)
	defer stop()
	server := &http.Server{
		Addr: addr, Handler: NewServer(store), ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return serviceCtx },
	}
	stopped := make(chan struct{})
	var shutdownErr error
	go func() {
		defer close(stopped)
		<-serviceCtx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if shutdownErr = server.Shutdown(shutdown); shutdownErr != nil {
			shutdownErr = errors.Join(shutdownErr, server.Close())
		}
	}()
	log.Printf("workflow debugger listening on http://%s (db: %s)", addr, dbPath)
	err = server.ListenAndServe()
	stop()
	<-stopped
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return errors.Join(err, shutdownErr)
}
