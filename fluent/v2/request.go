package fluent

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Request 是单次调用的构建器。构建阶段的方法不是并发安全的；
// 构建完成后 Do/Exec/Into/Decode/Bytes/Stream 只读取 Request，可以并发调用或重复调用。
type Request struct {
	client     *Client
	method     string
	path       string
	pathParams map[string]string
	header     http.Header
	query      url.Values
	cookies    []*http.Cookie
	body       bodyFunc
	timeout    *time.Duration
	idle       *time.Duration
	retry      *RetryPolicy
	accept     func(int) bool
	// err 记录构建阶段的第一个错误，延迟到执行时以 ErrBuild 返回，保证链式调用不中断。
	err error
}

// payload 是一次 Do 内可重复打开的请求体。
type payload struct {
	open        func() (io.ReadCloser, error)
	contentType string
	length      int64 // -1 表示未知
	replayable  bool
}

type bodyFunc func(c *Client) (payload, error)

func (r *Request) setErr(err error) {
	if r.err == nil {
		r.err = err
	}
}

// Path 替换路径模板中的 {name} 占位符，值会被 PathEscape。
func (r *Request) Path(name, value string) *Request {
	if r.pathParams == nil {
		r.pathParams = make(map[string]string)
	}
	r.pathParams[name] = value
	return r
}

// Query 设置查询参数，多个值会编码为 ?k=a&k=b，覆盖同名的已有值。
func (r *Request) Query(key string, values ...string) *Request {
	r.query[key] = append([]string(nil), values...)
	return r
}

// Queries 合并 url.Values、map[string]string 或带 `query` 标签的 struct，按 key 覆盖。
func (r *Request) Queries(v any) *Request {
	vals, err := encodeValues(v, "query")
	if err != nil {
		r.setErr(err)
		return r
	}
	maps.Copy(r.query, vals)
	return r
}

// Header 设置请求头，覆盖 Client 级同名头。
func (r *Request) Header(key, value string) *Request {
	r.header.Set(key, value)
	return r
}

// Headers 批量设置请求头。
func (r *Request) Headers(h http.Header) *Request {
	for k, vs := range h {
		r.header[http.CanonicalHeaderKey(k)] = append([]string(nil), vs...)
	}
	return r
}

func (r *Request) Bearer(token string) *Request {
	return r.Header("Authorization", "Bearer "+token)
}

func (r *Request) BasicAuth(username, password string) *Request {
	return r.Header("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(username+":"+password)))
}

// IdempotencyKey 设置 Idempotency-Key，使 POST/PATCH 也可以被安全重试。
func (r *Request) IdempotencyKey(key string) *Request {
	return r.Header("Idempotency-Key", key)
}

func (r *Request) Cookie(c *http.Cookie) *Request {
	r.cookies = append(r.cookies, c)
	return r
}

// Timeout 覆盖单次尝试的超时。
func (r *Request) Timeout(d time.Duration) *Request {
	r.timeout = new(d)
	return r
}

// IdleTimeout 覆盖流式响应（Events）的空闲超时：连续 d 时间未收到任何字节即中断，0 表示不限制。
func (r *Request) IdleTimeout(d time.Duration) *Request {
	r.idle = new(d)
	return r
}

// Retry 覆盖 Client 级重试策略。
func (r *Request) Retry(p RetryPolicy) *Request {
	r.retry = new(p)
	return r
}

// NoRetry 关闭本次请求的重试。
func (r *Request) NoRetry() *Request { return r.Retry(RetryPolicy{}) }

// AcceptStatus 指定本次请求视为成功的状态码，例如 AcceptStatus(200, 404) 让 404 交给调用方处理。
func (r *Request) AcceptStatus(codes ...int) *Request {
	r.accept = func(code int) bool {
		return slices.Contains(codes, code)
	}
	return r
}

// JSON 使用 Client 的 Codec 编码请求体（每次 Do 只编码一次，重试复用同一份字节）。
func (r *Request) JSON(v any) *Request {
	r.body = func(c *Client) (payload, error) {
		data, err := c.codec.Marshal(v)
		if err != nil {
			return payload{}, err
		}
		return bytesPayload(data, c.codec.ContentType()), nil
	}
	return r
}

// Form 以 application/x-www-form-urlencoded 发送，v 的规则同 Queries，struct 读取 `form` 标签。
func (r *Request) Form(v any) *Request {
	vals, err := encodeValues(v, "form")
	if err != nil {
		r.setErr(err)
		return r
	}
	data := []byte(vals.Encode())
	r.body = func(*Client) (payload, error) {
		return bytesPayload(data, "application/x-www-form-urlencoded"), nil
	}
	return r
}

// BodyBytes 发送原始字节。
func (r *Request) BodyBytes(data []byte, contentType string) *Request {
	r.body = func(*Client) (payload, error) { return bytesPayload(data, contentType), nil }
	return r
}

// BodyReader 发送一次性的流。流无法重放，因此该请求不会被重试；需要重试时使用 BodyFunc。
func (r *Request) BodyReader(body io.Reader, contentType string) *Request {
	r.body = func(*Client) (payload, error) {
		return payload{
			open:        func() (io.ReadCloser, error) { return io.NopCloser(body), nil },
			contentType: contentType,
			length:      -1,
		}, nil
	}
	return r
}

// BodyFunc 每次尝试都调用 open 获取新的请求体，因此可以重试。
func (r *Request) BodyFunc(open func() (io.ReadCloser, error), contentType string) *Request {
	r.body = func(*Client) (payload, error) {
		return payload{open: open, contentType: contentType, length: -1, replayable: true}, nil
	}
	return r
}

// Multipart 以流式 multipart/form-data 发送，每次尝试都会重新调用 write，因此可以重试。
// write 返回的错误会终止请求并以 ErrBuild 返回，不会发出残缺的请求体。
func (r *Request) Multipart(write func(w *multipart.Writer) error) *Request {
	r.body = func(*Client) (payload, error) {
		boundary := multipart.NewWriter(io.Discard).Boundary()
		return payload{
			open: func() (io.ReadCloser, error) {
				pr, pw := io.Pipe()
				mw := multipart.NewWriter(pw)
				if err := mw.SetBoundary(boundary); err != nil {
					return nil, err
				}
				go func() {
					err := write(mw)
					if err == nil {
						err = mw.Close()
					}
					if err != nil {
						err = &bodyError{err}
					}
					pw.CloseWithError(err)
				}()
				return pr, nil
			},
			contentType: "multipart/form-data; boundary=" + boundary,
			length:      -1,
			replayable:  true,
		}, nil
	}
	return r
}

// bodyError 标记由请求体生成引起的失败，这类错误不重试并归类为 ErrBuild。
type bodyError struct{ err error }

func (e *bodyError) Error() string { return "write request body: " + e.err.Error() }
func (e *bodyError) Unwrap() error { return e.err }

func bytesPayload(data []byte, contentType string) payload {
	return payload{
		open:        func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil },
		contentType: contentType,
		length:      int64(len(data)),
		replayable:  true,
	}
}

// prepared 是一次 Do 内各次尝试共享的不可变数据。
type prepared struct {
	url     *url.URL
	header  http.Header
	body    payload
	hasBody bool
}

func (r *Request) prepare() (*prepared, error) {
	if r.err != nil {
		return nil, r.err
	}
	u, err := r.resolveURL()
	if err != nil {
		return nil, err
	}
	header := r.client.header.Clone()
	maps.Copy(header, r.header)
	p := &prepared{url: u, header: header}
	if r.body != nil {
		if p.body, err = r.body(r.client); err != nil {
			return nil, fmt.Errorf("encode request body: %w", err)
		}
		p.hasBody = true
		if p.body.contentType != "" && header.Get("Content-Type") == "" {
			header.Set("Content-Type", p.body.contentType)
		}
	}
	return p, nil
}

func (r *Request) resolveURL() (*url.URL, error) {
	path, err := expandPath(r.path, r.pathParams)
	if err != nil {
		return nil, err
	}
	ref, err := url.Parse(path)
	if err != nil {
		return nil, err
	}
	var u *url.URL
	switch {
	case ref.IsAbs():
		u = ref
	case r.client.base == nil:
		return nil, fmt.Errorf("relative path %q requires WithBaseURL", r.path)
	default:
		u = r.client.base.Clone()
		joined := strings.TrimRight(u.EscapedPath(), "/") + "/" + strings.TrimLeft(ref.EscapedPath(), "/")
		if u.Path, err = url.PathUnescape(joined); err != nil {
			return nil, err
		}
		u.RawPath = joined
		q := u.Query()
		maps.Copy(q, ref.Query())
		u.RawQuery = q.Encode()
	}
	if len(r.query) > 0 {
		q := u.Query()
		maps.Copy(q, r.query)
		u.RawQuery = q.Encode()
	}
	return u, nil
}

// expandPath 替换 {name} 占位符；缺少参数或存在未使用的参数都视为构建错误，以尽早暴露拼写错误。
func expandPath(tmpl string, params map[string]string) (string, error) {
	if !strings.Contains(tmpl, "{") {
		if len(params) > 0 {
			return "", fmt.Errorf("path %q has no placeholders but got params", tmpl)
		}
		return tmpl, nil
	}
	var sb strings.Builder
	used := 0
	rest := tmpl
	for {
		before, after, found := strings.Cut(rest, "{")
		sb.WriteString(before)
		if !found {
			break
		}
		name, tail, ok := strings.Cut(after, "}")
		if !ok {
			return "", fmt.Errorf("path %q has unclosed placeholder", tmpl)
		}
		v, ok := params[name]
		if !ok {
			return "", fmt.Errorf("path %q missing param %q", tmpl, name)
		}
		sb.WriteString(url.PathEscape(v))
		used++
		rest = tail
	}
	if used < len(params) {
		return "", fmt.Errorf("path %q got unused params", tmpl)
	}
	return sb.String(), nil
}

func (r *Request) policy() RetryPolicy {
	if r.retry != nil {
		return *r.retry
	}
	return r.client.retry
}

func (r *Request) acceptFunc() func(int) bool {
	if r.accept != nil {
		return r.accept
	}
	return r.client.accept
}

func (r *Request) idleTimeout() time.Duration {
	if r.idle != nil {
		return *r.idle
	}
	return r.client.idleTimeout
}

func (r *Request) attemptTimeout() time.Duration {
	if r.timeout != nil {
		return *r.timeout
	}
	return r.client.timeout
}

// Do 执行请求（含重试），返回状态码被接受的响应，调用方必须关闭 resp.Body。
// 状态码不被接受时，响应体会被读取并关闭，以 ErrStatus 返回。
func (r *Request) Do(ctx context.Context) (*http.Response, error) {
	return r.do(ctx, false)
}

// do 是 Do 与 Events 的共同实现。stream=true 时补齐 SSE 请求头，
// 并在收到被接受的响应后把单次尝试超时切换为空闲超时。
func (r *Request) do(ctx context.Context, stream bool) (*http.Response, error) {
	p, err := r.prepare()
	if err != nil {
		return nil, r.newError(ErrBuild, nil, nil, err, 0)
	}
	if stream {
		if p.header.Get("Accept") == "" {
			p.header.Set("Accept", "text/event-stream")
		}
		if p.header.Get("Cache-Control") == "" {
			p.header.Set("Cache-Control", "no-cache")
		}
	}
	policy := r.policy()
	maxAttempts := max(policy.MaxAttempts, 1)
	canRetry := p.body.replayable || !p.hasBody
	canRetry = canRetry && (policy.RetryNonIdempotent || isIdempotent(r.method, p.header))
	accept := r.acceptFunc()

	for attempt := 1; ; attempt++ {
		resp, err := r.attempt(ctx, p, attempt)
		if ctxErr := ctx.Err(); ctxErr != nil {
			discard(resp)
			return nil, r.newError(ctxKind(ctxErr), p.url, nil, ctxErr, attempt)
		}
		if err == nil && accept(resp.StatusCode) {
			if stream {
				resp.Body.(*cancelBody).startIdle(r.idleTimeout())
			}
			return resp, nil
		}
		if _, ok := errors.AsType[*bodyError](err); ok {
			return nil, r.newError(ErrBuild, p.url, nil, err, attempt)
		}

		retry := canRetry && attempt < maxAttempts && policy.shouldRetry(resp, err)
		var wait time.Duration
		if retry {
			wait, retry = policy.delay(attempt, resp)
		}
		if !retry {
			if err != nil {
				return nil, r.newError(transportKind(err), p.url, nil, err, attempt)
			}
			return nil, r.statusError(resp, attempt)
		}
		discard(resp)
		if err := sleep(ctx, wait); err != nil {
			return nil, r.newError(ctxKind(err), p.url, nil, err, attempt)
		}
	}
}

var (
	errAttemptTimeout = fmt.Errorf("fluent: attempt timeout: %w", context.DeadlineExceeded)
	errIdleTimeout    = fmt.Errorf("fluent: stream idle timeout: %w", context.DeadlineExceeded)
)

func (r *Request) attempt(ctx context.Context, p *prepared, n int) (*http.Response, error) {
	// 不用 context.WithTimeout：deadline 一旦设置无法撤销，而流式响应需要在收到响应头后
	// 把单次尝试超时替换为空闲超时。这里用计时器 + CancelCause 实现可替换的超时。
	ctx, cancel := context.WithCancelCause(context.WithValue(ctx, attemptKey{}, n))
	body := &cancelBody{ctx: ctx, cancel: cancel}
	if d := r.attemptTimeout(); d > 0 {
		body.timer = time.AfterFunc(d, func() { cancel(errAttemptTimeout) })
	}
	req := &http.Request{
		Method:     r.method,
		URL:        p.url,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     p.header.Clone(),
		Host:       p.url.Host,
	}
	req = req.WithContext(ctx)
	if p.hasBody {
		reqBody, err := p.body.open()
		if err != nil {
			body.release()
			return nil, &bodyError{err}
		}
		req.Body = reqBody
		req.ContentLength = p.body.length
		if p.body.replayable {
			req.GetBody = p.body.open
		}
	}
	for _, c := range r.cookies {
		req.AddCookie(c)
	}
	resp, err := r.client.http.Do(req)
	if err != nil {
		err = body.wrap(err)
		body.release()
		return nil, err
	}
	body.ReadCloser = resp.Body
	resp.Body = body
	return resp, nil
}

// cancelBody 持有单次尝试的 context 与超时计时器，关闭时释放它们。
type cancelBody struct {
	io.ReadCloser
	ctx    context.Context
	cancel context.CancelCauseFunc
	timer  *time.Timer
	idle   time.Duration
}

// startIdle 停止单次尝试超时，改为空闲超时：每次读到数据都会重新计时，d<=0 表示不限制。
// 只能在读取响应体的同一个 goroutine 中、开始读取之前调用。
func (b *cancelBody) startIdle(d time.Duration) {
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	b.idle = d
	if d > 0 {
		b.timer = time.AfterFunc(d, func() { b.cancel(errIdleTimeout) })
	}
}

func (b *cancelBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 && b.idle > 0 {
		b.timer.Reset(b.idle)
	}
	if err != nil && err != io.EOF {
		err = b.wrap(err)
	}
	return n, err
}

// wrap 在由本包计时器触发取消时，把根因（单次超时 / 空闲超时）挂到错误链上，
// 使 errors.Is(err, context.DeadlineExceeded) 成立并被归类为 ErrTimeout。
func (b *cancelBody) wrap(err error) error {
	if cause := context.Cause(b.ctx); cause == errAttemptTimeout || cause == errIdleTimeout {
		if !errors.Is(err, cause) {
			return fmt.Errorf("%w: %w", cause, err)
		}
	}
	return err
}

func (b *cancelBody) release() {
	if b.timer != nil {
		b.timer.Stop()
	}
	b.cancel(context.Canceled)
}

func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.release()
	return err
}

const (
	statusBodyLimit = 64 << 10
	decodeBodyLimit = 4 << 10
	discardLimit    = 4 << 10
)

func (r *Request) statusError(resp *http.Response, attempts int) error {
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, statusBodyLimit))
	var cause error
	if r.client.errorDecoder != nil {
		cause = r.client.errorDecoder(resp, body)
	}
	e := r.newError(ErrStatus, nil, resp, cause, attempts)
	e.Body = body
	return e
}

func (r *Request) newError(kind Kind, u *url.URL, resp *http.Response, cause error, attempts int) *Error {
	e := &Error{Kind: kind, Method: r.method, URL: r.path, Attempts: attempts, Err: cause}
	if u != nil {
		e.URL = redact(u)
	}
	if resp != nil {
		e.StatusCode = resp.StatusCode
		e.Header = resp.Header
		if resp.Request != nil {
			e.URL = redact(resp.Request.URL)
		}
	}
	return e
}

func redact(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.Redacted()
}

func ctxKind(err error) Kind {
	if errors.Is(err, context.Canceled) {
		return ErrCanceled
	}
	return ErrTimeout
}

func transportKind(err error) Kind {
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrTimeout
	}
	if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
		return ErrTimeout
	}
	return ErrTransport
}

// discard 读尽少量剩余数据后关闭，使连接可以复用。
func discard(resp *http.Response) {
	if resp == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, discardLimit))
	_ = resp.Body.Close()
}

// Exec 执行请求并丢弃响应体，适用于只关心成功与否的调用。
func (r *Request) Exec(ctx context.Context) error {
	resp, err := r.Do(ctx)
	if err != nil {
		return err
	}
	discard(resp)
	return nil
}

// Into 把响应体解码到 v（必须是指针）。204、304 与 HEAD 请求不解码。
func (r *Request) Into(ctx context.Context, v any) error {
	resp, err := r.Do(ctx)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if r.method == http.MethodHead || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified {
		return nil
	}
	head := &prefixWriter{limit: decodeBodyLimit}
	if err := r.client.codec.Decode(io.TeeReader(resp.Body, head), v); err != nil {
		kind := ErrDecode
		if errors.Is(err, context.DeadlineExceeded) {
			kind = ErrTimeout
		}
		e := r.newError(kind, nil, resp, err, attemptOf(resp))
		e.Body = head.buf
		return e
	}
	return nil
}

// Decode 把响应体解码为 T。
//
//	user, err := c.Get("/users/{id}").Path("id", id).Decode[User](ctx)
func (r *Request) Decode[T any](ctx context.Context) (T, error) {
	var v T
	err := r.Into(ctx, &v)
	return v, err
}

// Bytes 读取完整响应体。
func (r *Request) Bytes(ctx context.Context) ([]byte, error) {
	resp, err := r.Do(ctx)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, r.newError(transportKind(err), nil, resp, err, attemptOf(resp))
	}
	return data, nil
}

// Stream 把响应体写入 w，适用于下载。注意单次尝试超时同样约束下载时长。
func (r *Request) Stream(ctx context.Context, w io.Writer) (int64, error) {
	resp, err := r.Do(ctx)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	n, err := io.Copy(w, resp.Body)
	if err != nil {
		return n, r.newError(transportKind(err), nil, resp, err, attemptOf(resp))
	}
	return n, nil
}

func attemptOf(resp *http.Response) int {
	if resp.Request == nil {
		return 0
	}
	return AttemptFromContext(resp.Request.Context())
}

// prefixWriter 只保留写入的前 limit 字节。
type prefixWriter struct {
	buf   []byte
	limit int
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	if room := w.limit - len(w.buf); room > 0 {
		w.buf = append(w.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}
