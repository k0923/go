package nodes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/k0923/go/dflow"
)

// HTTPNode receives url, headers, params, body and auth as resolved node inputs.
// Non-2xx responses are returned as output for downstream business decisions.
//
// HTTPNode 接收已经解析的 url、headers、params、body 和 auth 节点输入。
// 非 2xx 响应会作为输出返回，供下游业务决策。
type HTTPNode struct {
	Method string
	// BodyType accepts none, JSON or raw-text.
	//
	// BodyType 接受 none、JSON 或 raw-text。
	BodyType           string
	TimeoutMS          int64
	RetryTimes         int
	RetryNonIdempotent bool
	MaxResponseBytes   int64
	// Inject a client for custom transport/redirect policy.
	//
	// 注入 client，以自定义 transport 或 redirect 策略。
	Client *http.Client
}

func (n *HTTPNode) Validate() error {
	if n.TimeoutMS < 0 || n.TimeoutMS > 600_000 || n.RetryTimes < 0 || n.RetryTimes > 5 {
		return fmt.Errorf("http: timeoutMs must be 0..600000 and retryTimes 0..5")
	}
	if n.MaxResponseBytes < 0 || n.MaxResponseBytes == int64(^uint64(0)>>1) {
		return fmt.Errorf("http: invalid maxResponseBytes")
	}
	switch n.BodyType {
	case "", "none", "JSON", "raw-text":
	default:
		return fmt.Errorf("http: unsupported bodyType %q", n.BodyType)
	}
	method := n.Method
	if method == "" {
		method = http.MethodGet
	}
	_, err := http.NewRequest(method, "http://localhost", nil)
	return err
}

func (n *HTTPNode) Run(parent context.Context, input map[string]any) (dflow.Result, error) {
	if err := n.Validate(); err != nil {
		return dflow.Result{}, err
	}
	timeout := n.TimeoutMS
	if timeout == 0 {
		timeout = 30_000
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(timeout)*time.Millisecond)
	defer cancel()
	address, ok := input["url"].(string)
	if !ok || address == "" {
		return dflow.Result{}, fmt.Errorf("http: url input must be a nonempty string")
	}
	target, err := url.Parse(address)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
		return dflow.Result{}, fmt.Errorf("http: url must be an absolute HTTP(S) URL")
	}
	params, err := httpValues(input["params"], "params")
	if err != nil {
		return dflow.Result{}, err
	}
	query := target.Query()
	for key, values := range params {
		query[key] = values
	}
	target.RawQuery = query.Encode()
	headers, err := httpValues(input["headers"], "headers")
	if err != nil {
		return dflow.Result{}, err
	}
	header := make(http.Header)
	for key, values := range headers {
		header[http.CanonicalHeaderKey(key)] = values
	}
	body, contentType, err := n.body(input["body"])
	if err != nil {
		return dflow.Result{}, err
	}
	if header.Get("Content-Type") == "" && contentType != "" {
		header.Set("Content-Type", contentType)
	}
	if err := httpAuth(header, input["auth"]); err != nil {
		return dflow.Result{}, err
	}
	method := n.Method
	if method == "" {
		method = http.MethodGet
	}
	retrySafe := n.RetryNonIdempotent || header.Get("Idempotency-Key") != ""
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete, http.MethodOptions, http.MethodTrace:
		retrySafe = true
	}
	if n.RetryTimes > 0 && !retrySafe {
		return dflow.Result{}, fmt.Errorf("http: retry requires an idempotent method, Idempotency-Key or retryNonIdempotent")
	}
	client := n.Client
	if client == nil {
		client = http.DefaultClient
	}
	limit := n.MaxResponseBytes
	if limit == 0 {
		limit = 8 << 20
	}
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return dflow.Result{}, err
		}
		req, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(body))
		if err != nil {
			return dflow.Result{}, err
		}
		req.Header = header.Clone()
		resp, requestErr := client.Do(req)
		if requestErr != nil {
			if ctx.Err() != nil {
				return dflow.Result{}, ctx.Err()
			}
			if attempt == n.RetryTimes {
				return dflow.Result{}, fmt.Errorf("http request: %w", requestErr)
			}
		} else {
			data, readErr := io.ReadAll(io.LimitReader(resp.Body, limit+1))
			closeErr := resp.Body.Close()
			if readErr != nil {
				return dflow.Result{}, fmt.Errorf("http response: %w", readErr)
			}
			if closeErr != nil {
				return dflow.Result{}, fmt.Errorf("http response close: %w", closeErr)
			}
			if int64(len(data)) > limit {
				return dflow.Result{}, fmt.Errorf("http response exceeds %d bytes", limit)
			}
			if (resp.StatusCode >= 200 && resp.StatusCode < 300) || attempt == n.RetryTimes {
				outHeaders := make(map[string]any, len(resp.Header))
				for key, values := range resp.Header {
					if len(values) == 1 {
						outHeaders[key] = values[0]
					} else {
						outHeaders[key] = append([]string(nil), values...)
					}
				}
				return dflow.Result{Output: map[string]any{
					"body": string(data), "headers": outHeaders, "statusCode": resp.StatusCode,
				}}, nil
			}
		}
		timer := time.NewTimer(time.Duration(3+2*attempt) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return dflow.Result{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (n *HTTPNode) body(value any) ([]byte, string, error) {
	switch n.BodyType {
	case "", "none":
		return nil, "", nil
	case "JSON":
		if text, ok := value.(string); ok {
			if !json.Valid([]byte(text)) {
				return nil, "", fmt.Errorf("http: JSON body string must contain valid JSON")
			}
			return []byte(text), "application/json", nil
		}
		data, err := json.Marshal(value)
		return data, "application/json", err
	case "raw-text":
		if value == nil {
			return nil, "text/plain", nil
		}
		text, ok := value.(string)
		if !ok {
			return nil, "", fmt.Errorf("http: raw-text body must be a string")
		}
		return []byte(text), "text/plain", nil
	default:
		return nil, "", fmt.Errorf("http: unsupported bodyType")
	}
}

// Normalize JSON objects from direct Go calls as well as engine-resolved inputs.
//
// 同时规范化直接 Go 调用和引擎解析输入中的 JSON 对象。
func httpValues(value any, field string) (url.Values, error) {
	result := make(url.Values)
	if value == nil {
		return result, nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var object any
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, err
	}
	values, ok := object.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("http: %s must be an object", field)
	}
	text := func(value any) (string, error) {
		switch value.(type) {
		case nil:
			return "", nil
		case string, bool, float64:
			return fmt.Sprint(value), nil
		default:
			return "", fmt.Errorf("http: %s values must be scalars or scalar arrays", field)
		}
	}
	for key, value := range values {
		items, array := value.([]any)
		if !array {
			items = []any{value}
		}
		for _, item := range items {
			s, err := text(item)
			if err != nil {
				return nil, err
			}
			result[key] = append(result[key], s)
		}
		if len(items) == 0 {
			result[key] = []string{}
		}
	}
	return result, nil
}

func httpAuth(header http.Header, value any) error {
	if value == nil {
		return nil
	}
	var auth struct {
		Type          string `json:"type"`
		BearerToken   string `json:"bearerToken"`
		BasicUsername string `json:"basicUsername"`
		BasicPassword string `json:"basicPassword"`
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return fmt.Errorf("http auth: must be an object")
	}
	if err := decoder.Decode(&auth); err != nil {
		return fmt.Errorf("http auth: %w", err)
	}
	switch auth.Type {
	case "", "none":
		return nil
	case "bearer":
		header.Set("Authorization", "Bearer "+auth.BearerToken)
	case "basic":
		req := &http.Request{Header: header}
		req.SetBasicAuth(auth.BasicUsername, auth.BasicPassword)
	default:
		return fmt.Errorf("http: unsupported auth type %q", auth.Type)
	}
	if strings.ContainsAny(header.Get("Authorization"), "\r\n") {
		return fmt.Errorf("http: invalid authorization header")
	}
	return nil
}
