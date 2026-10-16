// Package debugger provides a local, HTTP + SQLite backed workflow
// Infrastructure with a web UI. One server hosts many isolated runs; each run
// owns its records, leases, notifications, wait groups and event history.
//
// Start the server:
//
//	go run ./workflow/cmd/wfdebug -addr 127.0.0.1:7788 -db wfdebug.db
//
// Then drive a workflow against it:
//
//	result, err := debugger.Run(ctx, definition, input, debugger.Options{})
package debugger

import (
	"encoding/json"
	"time"

	"github.com/k0923/go/workflow"
)

// DefaultServer is the address used by Run and NewClient when none is given.
const DefaultServer = "http://127.0.0.1:7788"

// Run statuses reported by a worker. They extend workflow.Status with the
// states only a driver loop can observe.
const (
	RunCreated  = "CREATED"
	RunRetrying = "RETRYING"
)

// RunSpec registers a run. An empty ID generates one. Reusing an ID resumes
// the run: Drive replays durable records already stored on the server.
type RunSpec struct {
	ID       string          `json:"id"`
	Workflow string          `json:"workflow"`
	Version  string          `json:"version"`
	Input    json.RawMessage `json:"input,omitempty"`
}

type Progress struct {
	Total     int `json:"total"`
	Completed int `json:"completed"`
	Running   int `json:"running"`
	Suspended int `json:"suspended"`
	Failed    int `json:"failed"`
	Cancelled int `json:"cancelled"`
}

type RunInfo struct {
	ID        string             `json:"id"`
	Workflow  string             `json:"workflow"`
	Version   string             `json:"version"`
	Input     json.RawMessage    `json:"input,omitempty"`
	Output    json.RawMessage    `json:"output,omitempty"`
	Status    string             `json:"status"`
	Error     string             `json:"error,omitempty"`
	Pending   []workflow.Pending `json:"pending"`
	Drives    int                `json:"drives"`
	CreatedAt time.Time          `json:"created_at"`
	UpdatedAt time.Time          `json:"updated_at"`
	LastSeen  time.Time          `json:"last_seen"`
	Online    bool               `json:"online"`
	Progress  Progress           `json:"progress"`
}

// RecordView is both the RPC encoding of workflow.Record and the UI view.
// Empty payloads are omitted so nil survives the round trip (not "null").
type RecordView struct {
	Key       string          `json:"key"`
	Kind      workflow.Kind   `json:"kind"`
	Status    workflow.Status `json:"status"`
	Version   int64           `json:"version"`
	Input     json.RawMessage `json:"input,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     string          `json:"error,omitempty"`
	Until     time.Time       `json:"until"`
	StartedAt time.Time       `json:"started_at"`
	EndedAt   time.Time       `json:"ended_at"`
	UpdatedAt time.Time       `json:"updated_at"`
	Group     string          `json:"group,omitempty"`
}

type GroupView struct {
	Key      string                 `json:"key"`
	Requests []workflow.WaitRequest `json:"requests"`
	State    workflow.WaitAnyState  `json:"state"`
}

type MessageView struct {
	ID          string    `json:"id"`
	Key         string    `json:"key"`
	Purpose     string    `json:"purpose"`
	State       string    `json:"state"`
	AvailableAt time.Time `json:"available_at"`
	CreatedAt   time.Time `json:"created_at"`
	DoneAt      time.Time `json:"done_at"`
}

type RunDetail struct {
	Run      RunInfo       `json:"run"`
	Records  []RecordView  `json:"records"`
	Groups   []GroupView   `json:"groups"`
	Messages []MessageView `json:"messages"`
	Now      time.Time     `json:"now"`
}

type EventView struct {
	ID     int64           `json:"id"`
	Source string          `json:"source"`
	At     time.Time       `json:"at"`
	Event  json.RawMessage `json:"event"`
}

// Report is posted by the worker after every Drive.
type Report struct {
	Status  string             `json:"status"`
	Output  json.RawMessage    `json:"output,omitempty"`
	Error   string             `json:"error,omitempty"`
	Pending []workflow.Pending `json:"pending"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// RPC payloads of the Infrastructure endpoints.
type (
	keyRequest   struct{ Key string }
	claimRequest struct {
		Key string
		TTL time.Duration
	}
	leaseRequest struct {
		Lease workflow.Lease
		TTL   time.Duration
	}
	commitRequest struct {
		Lease    workflow.Lease
		Record   RecordView
		Expected int64
	}
	putRequest     struct{ Record RecordView }
	resolveRequest struct {
		Key      string
		Version  int64
		Response json.RawMessage
	}
	pendingRequest struct{ Now time.Time }
	ackRequest     struct{ ID string }
	waitAnyRequest struct {
		Key      string
		Requests []workflow.WaitRequest
	}
	failWaitRequest struct {
		Key     string
		Version int64
		Message string
	}
	recordResponse        struct{ Record *RecordView }
	leaseResponse         struct{ Lease *workflow.Lease }
	boolResponse          struct{ OK bool }
	notificationsResponse struct{ Notifications []workflow.Notification }
	stateResponse         struct{ State *workflow.WaitAnyState }
)

func toView(r *workflow.Record, group string) *RecordView {
	if r == nil {
		return nil
	}
	return &RecordView{
		Key: r.Key, Kind: r.Kind, Status: r.Status, Version: r.Version,
		Input: clone(r.Input), Result: clone(r.Result), Error: r.Error,
		Until: r.Until, StartedAt: r.StartedAt, EndedAt: r.EndedAt, Group: group,
	}
}

func fromView(v *RecordView) *workflow.Record {
	if v == nil {
		return nil
	}
	return &workflow.Record{
		Key: v.Key, Kind: v.Kind, Status: v.Status, Version: v.Version,
		Input: clone(v.Input), Result: clone(v.Result), Error: v.Error,
		Until: v.Until, StartedAt: v.StartedAt, EndedAt: v.EndedAt,
	}
}

func clone(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}
