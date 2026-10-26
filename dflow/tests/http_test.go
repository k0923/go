package tests

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/dflow/memory"
)

func TestHTTPNodeDAG(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		username, password, basic := r.BasicAuth()
		if r.Method != "POST" || string(body) != `{"message":"hello"}` ||
			r.URL.Query().Get("existing") != "yes" || strings.Join(r.URL.Query()["tag"], ",") != "one,two" ||
			r.Header.Get("Content-Type") != "application/json" || r.Header.Get("X-Custom") != "test" ||
			!basic || username != "user" || password != "pass" {
			t.Errorf("unexpected request: method=%s query=%s headers=%v body=%s", r.Method, r.URL.RawQuery, r.Header, body)
		}
		w.Header().Add("X-Multi", "first")
		w.Header().Add("X-Multi", "second")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	def := dflow.Definition{
		Nodes: []dflow.NodeSpec{
			{ID: "start", Node: &dflow.StartNode{}},
			{ID: "request", Node: &dflow.HTTPNode{Method: "POST", BodyType: "JSON"},
				Inputs: dflow.Parameters{
					"url":     dflow.Ref("start", "url"),
					"body":    dflow.Ref("start", "payload"),
					"headers": dflow.Constant(map[string]any{"X-Custom": "test"}),
					"params":  dflow.Constant(map[string]any{"tag": []string{"one", "two"}}),
					"auth":    dflow.Ref("start", "auth"),
				}},
			{ID: "parse", Node: &dflow.JSONParseNode{}, Inputs: dflow.Parameters{"json": dflow.Ref("request", "body")}},
		},
		Edges: []dflow.Edge{edge("start", "request"), edge("request", "parse")},
	}
	e := engine(t, def, nil, memory.New(), dflow.Options{})
	state, err := e.Run(context.Background(), map[string]any{
		"url": server.URL + "?existing=yes", "payload": map[string]any{"message": "hello"},
		"auth": map[string]any{"type": "basic", "basicUsername": "user", "basicPassword": "pass"},
	})
	if err != nil {
		t.Fatal(err)
	}
	output := state.Nodes["request"].Output
	headers := output["headers"].(map[string]any)
	if output["statusCode"] != 201 || len(headers["X-Multi"].([]string)) != 2 ||
		state.Nodes["parse"].Output["ok"] != true {
		t.Fatal("unexpected HTTP output", output)
	}
}

func TestHTTPNodeBodiesAndBearer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Error("bearer auth missing")
		}
		w.Header().Set("X-Content-Type", r.Header.Get("Content-Type"))
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write(data)
	}))
	defer server.Close()
	for _, tc := range []struct {
		bodyType string
		value    any
		expected string
		content  string
	}{
		{"JSON", `{"number":1}`, `{"number":1}`, "application/json"},
		{"JSON", nil, "null", "application/json"},
		{"raw-text", "hello", "hello", "text/plain"},
		{"none", nil, "", ""},
	} {
		node := &dflow.HTTPNode{Method: "POST", BodyType: tc.bodyType}
		result, err := node.Run(context.Background(), map[string]any{
			"url": server.URL, "body": tc.value, "auth": map[string]any{"type": "bearer", "bearerToken": "token"},
		})
		if err != nil {
			t.Fatal(err)
		}
		out := result.Output
		if out["body"] != tc.expected || out["statusCode"] != 422 ||
			out["headers"].(map[string]any)["X-Content-Type"] != tc.content {
			t.Fatal("non-2xx must preserve HTTP response", out)
		}
	}
}

func TestHTTPNodeValidationAndLimits(t *testing.T) {
	if _, err := (&dflow.JSONParseNode{}).Run(context.Background(), map[string]any{"json": "null"}); err == nil {
		t.Fatal("JSONParseNode accepted a non-object result")
	}
	for _, config := range []string{
		`{"timeoutMs":-1}`, `{"timeoutMs":600001}`, `{"retryTimes":6}`, `{"retryTimes":-1}`,
		`{"bodyType":"bad"}`, `{"method":"bad method"}`, `{"maxResponseBytes":-1}`,
	} {
		node := configuredFixture("http", json.RawMessage(config)).(*dflow.HTTPNode)
		if err := node.Validate(); err == nil {
			t.Fatal("invalid HTTP config accepted", config)
		}
	}
	node := &dflow.HTTPNode{BodyType: "JSON"}
	for _, input := range []map[string]any{
		{"url": "file:///tmp/a"},
		{"url": "http://", "body": nil},
		{"url": "http://localhost", "body": "invalid JSON"},
		{"url": "http://localhost", "params": "bad"},
		{"url": "http://localhost", "headers": map[string]any{"X": map[string]any{"nested": 1}}},
		{"url": "http://localhost", "auth": map[string]any{"type": "unknown"}},
	} {
		if _, err := node.Run(context.Background(), input); err == nil {
			t.Fatal("invalid HTTP input accepted", input)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("oversized"))
	}))
	defer server.Close()
	if _, err := (&dflow.HTTPNode{MaxResponseBytes: 3}).Run(context.Background(), map[string]any{"url": server.URL}); err == nil {
		t.Fatal("response limit was not enforced")
	}
}

func TestHTTPNodeRetryAndCancellation(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		_, _ = w.Write([]byte("success"))
	}))
	defer server.Close()
	result, err := (&dflow.HTTPNode{RetryTimes: 1}).Run(context.Background(), map[string]any{"url": server.URL})
	if err != nil || calls.Load() != 2 || result.Output["body"] != "success" {
		t.Fatal("retry failed", result, err, calls.Load())
	}
	before := calls.Load()
	if _, err := (&dflow.HTTPNode{Method: "POST", RetryTimes: 1}).Run(context.Background(), map[string]any{"url": server.URL}); err == nil || calls.Load() != before {
		t.Fatal("unsafe POST retry must be rejected before request")
	}
	result, err = (&dflow.HTTPNode{Method: "POST", RetryTimes: 1}).Run(context.Background(), map[string]any{
		"url": server.URL, "headers": map[string]any{"Idempotency-Key": "operation-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer unavailable.Close()
	_, err = (&dflow.HTTPNode{RetryTimes: 1, TimeoutMS: 20}).Run(context.Background(), map[string]any{"url": unavailable.URL})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("retry backoff ignored timeout", err)
	}
	blocking := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer blocking.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = (&dflow.HTTPNode{}).Run(ctx, map[string]any{"url": blocking.URL})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("HTTP request ignored caller context", err)
	}
}

func TestHTTPNodeClientInjection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/redirect", http.StatusFound)
	}))
	defer server.Close()
	node := &dflow.HTTPNode{Client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}}
	result, err := node.Run(context.Background(), map[string]any{"url": server.URL})
	if err != nil || result.Output["statusCode"] != 302 {
		t.Fatal("injected client was not preserved", result, err)
	}
}
