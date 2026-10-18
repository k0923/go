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
//
// Package debugger 提供一个本地、由 HTTP + SQLite 支持的 workflow Infrastructure，
// 并带有 Web UI。一个 server 可承载多个隔离运行；每个运行拥有自己的记录、租约、
// 通知、等待组和事件历史。
//
// 启动服务：
//
//	go run ./workflow/cmd/wfdebug -addr 127.0.0.1:7788 -db wfdebug.db
//
// 然后让工作流连接到它执行：
//
//	result, err := debugger.Run(ctx, definition, input, debugger.Options{})
package debugger

import (
	"encoding/json"
	"time"

	"github.com/k0923/go/workflow"
	"github.com/k0923/go/workflow/memory"
)

// DefaultServer is the address used by Run and NewClient when none is given.
// DefaultServer 是 Run 和 NewClient 在未指定地址时使用的地址。
const DefaultServer = "http://127.0.0.1:7788"

// Run statuses reported by a worker. They extend workflow.Status with the
// states only a driver loop can observe.
// worker 上报的运行状态。它们在 workflow.Status 基础上扩展了只有驱动循环能观察到的状态。
const (
	RunCreated  = "CREATED"
	RunRetrying = "RETRYING"
)

// RunSpec registers a run. An empty ID generates one. Reusing an ID resumes
// the run: Drive replays durable records already stored on the server.
// RunSpec 注册一个运行。空 ID 会生成一个新 ID。复用 ID 会恢复运行：
// Drive 会回放已存储在服务端的持久记录。
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
// RecordView 既是 workflow.Record 的 RPC 编码，也是 UI 视图。
// 空负载会被省略，因此 nil 可以在往返中保留，而不是变成 "null"。
type RecordView struct {
	Key       string          `json:"key"`
	Path      workflow.Path   `json:"path,omitempty"`
	Kind      workflow.Kind   `json:"kind"`
	Status    workflow.Status `json:"status"`
	Version   int64           `json:"version"`
	Type      string          `json:"type,omitempty"`
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
// Report 由 worker 在每次 Drive 后提交。
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
// Infrastructure 端点的 RPC 负载。
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
	notificationsResponse struct{ Notifications []memory.Notification }
	stateResponse         struct{ State *workflow.WaitAnyState }
)

func toView(r *workflow.Record, group string) *RecordView {
	if r == nil {
		return nil
	}
	return &RecordView{
		Key: r.Key, Path: append(workflow.Path(nil), r.Path...), Kind: r.Kind, Status: r.Status, Version: r.Version, Type: r.Type,
		Input: clone(r.Input), Result: clone(r.Result), Error: r.Error,
		Until: r.Until, StartedAt: r.StartedAt, EndedAt: r.EndedAt, Group: group,
	}
}

func fromView(v *RecordView) *workflow.Record {
	if v == nil {
		return nil
	}
	return &workflow.Record{
		Key: v.Key, Path: append(workflow.Path(nil), v.Path...), Kind: v.Kind, Status: v.Status, Version: v.Version, Type: v.Type,
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
