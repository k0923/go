package debugger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/k0923/go/workflow"
	"github.com/k0923/go/workflow/memory"
)

// Client is a workflow.Infrastructure (plus memory.Outbox and
// WaitAnyInfrastructure) bound to ONE run on a debugger server. Create one
// Client per run.
// Client 是绑定到 debugger server 上单个运行的 workflow.Infrastructure
// （另加 memory.Outbox 和 WaitAnyInfrastructure）。每个运行应创建一个 Client。
type Client struct {
	base string
	run  string
	http *http.Client
}

var (
	_ workflow.Infrastructure        = (*Client)(nil)
	_ memory.Outbox                  = (*Client)(nil)
	_ workflow.WaitAnyInfrastructure = (*Client)(nil)
)

// NewClient binds to an existing or new run ID. An empty server uses DefaultServer.
// NewClient 绑定到一个已有或新的 run ID。server 为空时使用 DefaultServer。
func NewClient(server, run string) (*Client, error) {
	if server == "" {
		server = DefaultServer
	}
	if !runIDPattern.MatchString(run) {
		return nil, fmt.Errorf("%w: run id %q", workflow.ErrInvalid, run)
	}
	return &Client{base: strings.TrimRight(server, "/"), run: run, http: &http.Client{}}, nil
}

func (c *Client) RunID() string { return c.run }

// URL is the UI page of this run.
// URL 是该运行的 UI 页面。
func (c *Client) URL() string { return c.base + "/#/runs/" + url.PathEscape(c.run) }

var codeErrors = map[string]error{}

func init() {
	for _, candidate := range errorCodes {
		codeErrors[candidate.code] = candidate.err
	}
}

// remoteError keeps the server message while matching the original sentinel
// with errors.Is, so the engine classifies conflicts and changes correctly.
// remoteError 保留服务端消息，同时通过 errors.Is 匹配原始哨兵错误，
// 使引擎能正确分类 conflict 和 changed。
type remoteError struct {
	sentinel error
	message  string
}

func (e *remoteError) Error() string { return e.message }
func (e *remoteError) Unwrap() error { return e.sentinel }

func (c *Client) call(ctx context.Context, method, path string, body, out any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.base+"/api/runs/"+url.PathEscape(c.run)+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode >= 300 {
		var apiErr apiError
		if json.Unmarshal(data, &apiErr) != nil || apiErr.Code == "" {
			return fmt.Errorf("debugger %s %s: HTTP %d: %s", method, path, response.StatusCode, bytes.TrimSpace(data))
		}
		if sentinel := codeErrors[apiErr.Code]; sentinel != nil {
			return &remoteError{sentinel: sentinel, message: apiErr.Message}
		}
		return errors.New(apiErr.Message)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (c *Client) infra(ctx context.Context, op string, body, out any) error {
	return c.call(ctx, http.MethodPost, "/infra/"+op, body, out)
}

func (c *Client) Claim(ctx context.Context, key string, ttl time.Duration) (*workflow.Lease, error) {
	var out leaseResponse
	err := c.infra(ctx, "claim", claimRequest{Key: key, TTL: ttl}, &out)
	return out.Lease, err
}

func (c *Client) Renew(ctx context.Context, lease workflow.Lease, ttl time.Duration) (*workflow.Lease, error) {
	var out leaseResponse
	err := c.infra(ctx, "renew", leaseRequest{Lease: lease, TTL: ttl}, &out)
	return out.Lease, err
}

func (c *Client) Release(ctx context.Context, lease workflow.Lease) error {
	return c.infra(ctx, "release", leaseRequest{Lease: lease}, nil)
}

func (c *Client) Get(ctx context.Context, key string) (*workflow.Record, error) {
	var out recordResponse
	if err := c.infra(ctx, "get", keyRequest{Key: key}, &out); err != nil {
		return nil, err
	}
	return fromView(out.Record), nil
}

func (c *Client) Commit(ctx context.Context, lease workflow.Lease, record *workflow.Record, expected int64) (*workflow.Record, error) {
	if record == nil {
		return nil, workflow.ErrInvalid
	}
	var out recordResponse
	if err := c.infra(ctx, "commit", commitRequest{Lease: lease, Record: *toView(record, ""), Expected: expected}, &out); err != nil {
		return nil, err
	}
	return fromView(out.Record), nil
}

func (c *Client) Put(ctx context.Context, record *workflow.Record) (*workflow.Record, error) {
	if record == nil {
		return nil, workflow.ErrInvalid
	}
	var out recordResponse
	if err := c.infra(ctx, "put", putRequest{Record: *toView(record, "")}, &out); err != nil {
		return nil, err
	}
	return fromView(out.Record), nil
}

func (c *Client) Resolve(ctx context.Context, key string, version int64, response []byte) (bool, error) {
	var out boolResponse
	err := c.infra(ctx, "resolve", resolveRequest{Key: key, Version: version, Response: clone(response)}, &out)
	return out.OK, err
}

func (c *Client) Pending(ctx context.Context, now time.Time) ([]memory.Notification, error) {
	var out notificationsResponse
	err := c.infra(ctx, "pending", pendingRequest{Now: now}, &out)
	return out.Notifications, err
}

func (c *Client) Ack(ctx context.Context, id string) error {
	return c.infra(ctx, "ack", ackRequest{ID: id}, nil)
}

func (c *Client) WaitAny(ctx context.Context, key string, requests []workflow.WaitRequest) (*workflow.WaitAnyState, error) {
	var out stateResponse
	err := c.infra(ctx, "wait_any", waitAnyRequest{Key: key, Requests: requests}, &out)
	return out.State, err
}

func (c *Client) FailWait(ctx context.Context, key string, version int64, message string) (bool, error) {
	var out boolResponse
	err := c.infra(ctx, "fail_wait", failWaitRequest{Key: key, Version: version, Message: message}, &out)
	return out.OK, err
}

// ---- debugger API ----
// ---- debugger API 接口 ----

func (c *Client) register(ctx context.Context, spec RunSpec) (RunInfo, error) {
	var info RunInfo
	spec.ID = c.run
	err := c.post(ctx, "/api/runs", spec, &info)
	return info, err
}

func (c *Client) post(ctx context.Context, path string, body, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		body, _ := io.ReadAll(response.Body)
		return fmt.Errorf("debugger POST %s: HTTP %d: %s", path, response.StatusCode, bytes.TrimSpace(body))
	}
	return json.NewDecoder(response.Body).Decode(out)
}

// Report records the outcome of one Drive for the UI.
// Report 为 UI 记录一次 Drive 的结果。
func (c *Client) Report(ctx context.Context, report Report) error {
	return c.call(ctx, http.MethodPost, "/report", report, nil)
}

// PushEvents forwards engine events to the server timeline.
// PushEvents 将引擎事件转发到服务端时间线。
func (c *Client) PushEvents(ctx context.Context, events []json.RawMessage) error {
	return c.call(ctx, http.MethodPost, "/events", events, nil)
}

// Submit answers a pending WaitFor, exactly like the UI's JSON form.
// Submit 回答一个 pending WaitFor，行为与 UI 的 JSON 表单完全一致。
func (c *Client) Submit(ctx context.Context, key string, response any) (bool, error) {
	raw, err := json.Marshal(response)
	if err != nil {
		return false, err
	}
	var out boolResponse
	err = c.call(ctx, http.MethodPost, "/submit", submitRequest{Key: key, Response: raw}, &out)
	return out.OK, err
}

// FireSleep completes a pending Sleep immediately.
// FireSleep 立即完成一个 pending Sleep。
func (c *Client) FireSleep(ctx context.Context, key string) error {
	return c.call(ctx, http.MethodPost, "/fire", submitRequest{Key: key}, nil)
}

// Detail returns the run snapshot shown by the UI.
// Detail 返回 UI 展示的运行快照。
func (c *Client) Detail(ctx context.Context) (*RunDetail, error) {
	var detail RunDetail
	err := c.call(ctx, http.MethodGet, "", nil, &detail)
	return &detail, err
}

// WaitWake blocks until a WAKE notification is due or timeout elapses.
// WaitWake 会阻塞，直到 WAKE 通知到期或超时结束。
func (c *Client) WaitWake(ctx context.Context, timeout time.Duration) ([]memory.Notification, error) {
	var out notificationsResponse
	err := c.call(ctx, http.MethodGet, "/wake?timeout="+url.QueryEscape(timeout.String()), nil, &out)
	return out.Notifications, err
}
