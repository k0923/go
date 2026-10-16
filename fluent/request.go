package fluent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Request 是链式构建的单次 HTTP 请求。
// 构建阶段（各 setter）不是并发安全的；构建完成后 Build/Response/Send 可以并发调用。
type Request interface {
	Body(BodySupplier) Request
	JsonBody(data interface{}) Request
	FileBody(body FileBody) Request
	FormBody() Request
	JsonResult(data interface{}) Request
	FileResult(io.Writer) Request
	Client(client *http.Client) Request
	Result(ResultHandler) Request
	Form(key string, value interface{}) Request
	Forms(items ...interface{}) Request
	// replace exist values.
	Query(key string, value interface{}) Request
	// replace exist values. result will be ?a=1&a=2&a=3
	QueryArray(key string, values []string) Request
	Queries(items ...interface{}) Request
	Bearer(token string) Request
	BasicAuth(username string, password string) Request
	Cookie(key string, value interface{}) Request
	Cookies(items ...interface{}) Request
	Header(key string, value interface{}) Request
	Headers(items ...interface{}) Request
	Context(ctx context.Context) Request
	Build() (*http.Request, error)
	// Response 在重试耗尽时可能同时返回非 nil 的 response 和 error，调用方负责关闭 Body。
	Response() (*http.Response, error)
	Retry(strategy RetryStrategy) Request
	PreProcessor(processor PreProcessor) Request
	PostProcessor(processor PostProcessor) Request
	GetContext() context.Context
	Send() error
}

// bodyFunc 每次发送都会重新调用，返回请求体与需要覆盖的 Content-Type（为空则不覆盖）。
type bodyFunc func() (body io.Reader, contentType string, err error)

// discardLimit 是重试时丢弃旧响应前最多读取的字节数，读尽后连接才能复用。
const discardLimit = 4 << 10

func newRequest(url string, method string, options *RequestOptions) Request {
	var newOptions RequestOptions
	if options != nil {
		newOptions = options.Copy()
	} else {
		newOptions = NewRequestOptions()
	}
	return &request{
		RequestOptions: newOptions,
		url:            url,
		method:         method,
		forms:          make(map[string]string),
		queries:        make(map[string][]string),
	}
}

type request struct {
	RequestOptions
	url           string
	method        string
	queries       url.Values
	forms         map[string]string
	ctx           context.Context
	resultHandler ResultHandler
	body          bodyFunc
}

func (r *request) Retry(retryStrategy RetryStrategy) Request {
	r.RetryStrategy = retryStrategy
	return r
}

func (r *request) Client(client *http.Client) Request {
	r.RequestOptions.Client = client
	return r
}

type Context struct {
	context.Context
	ElapsedTime time.Duration
}

func (r *request) context() context.Context {
	if r.ctx == nil {
		return context.Background()
	}
	return r.ctx
}

func (r *request) httpClient() *http.Client {
	if r.RequestOptions.Client == nil {
		return http.DefaultClient
	}
	return r.RequestOptions.Client
}

func (r *request) Response() (*http.Response, error) {
	ctx := r.context()
	var retryHandler RetryHandler
	if r.RetryStrategy != nil {
		retryHandler = r.RetryStrategy()
	}

	for budget := MaxRetryTimes; ; budget-- {
		resp, err := r.do(ctx)
		if retryHandler == nil {
			return resp, err
		}
		result, needRetry := retryHandler(ctx, resp, err)
		if result == nil || !needRetry || budget == 0 {
			return resp, result
		}
		discard(resp)
	}
}

// do 执行一次完整的发送：构建请求、前后置处理器、Client.Do。
func (r *request) do(ctx context.Context) (*http.Response, error) {
	start := time.Now()
	req, err := r.build(ctx)
	if err != nil {
		return nil, err
	}
	if r.RequestOptions.PreProcessor != nil {
		r.RequestOptions.PreProcessor(ctx, req)
	}
	resp, err := r.httpClient().Do(req)
	if r.RequestOptions.PostProcessor != nil {
		r.RequestOptions.PostProcessor(Context{
			Context:     ctx,
			ElapsedTime: time.Since(start),
		}, resp, err)
	}
	return resp, err
}

func (r *request) Build() (*http.Request, error) {
	return r.build(r.context())
}

// build 只读取 request 的字段，不做任何写入，因此构建完成后可以并发调用。
func (r *request) build(ctx context.Context) (*http.Request, error) {
	var body io.Reader
	var contentType string
	var err error
	if r.body != nil {
		if body, contentType, err = r.body(); err != nil {
			return nil, err
		}
	}
	request, err := http.NewRequestWithContext(ctx, r.method, r.url, body)
	if err != nil {
		// 流式请求体（如 FileBody 的 pipe）未被消费时必须关闭，否则写端 goroutine 泄漏。
		if closer, ok := body.(io.Closer); ok {
			closer.Close()
		}
		return nil, err
	}
	for k, v := range r.RequestOptions.Headers {
		request.Header.Set(k, v)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if len(r.queries) > 0 {
		q := request.URL.Query()
		for k, values := range r.queries {
			for _, v := range values {
				q.Add(k, v)
			}
		}
		request.URL.RawQuery = q.Encode()
	}
	for k, v := range r.RequestOptions.Cookies {
		request.AddCookie(&http.Cookie{Name: k, Value: v, HttpOnly: true})
	}

	return request, nil
}

func (r *request) Send() error {
	response, err := r.Response()
	if err != nil {
		discard(response)
		return err
	}
	if response == nil {
		return nil
	}
	defer response.Body.Close()
	if r.resultHandler != nil {
		return r.resultHandler(r.context(), response)
	}
	return nil
}

// discard 读尽并关闭不再使用的响应，使底层连接可以复用。
func discard(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, discardLimit))
	_ = resp.Body.Close()
}

func (r *request) Context(ctx context.Context) Request {
	r.ctx = ctx
	return r
}

func (r *request) GetContext() context.Context {
	return r.context()
}

func (r *request) Body(supplier BodySupplier) Request {
	if supplier == nil {
		r.body = nil
		return r
	}
	r.body = func() (io.Reader, string, error) {
		body, err := supplier()
		return body, "", err
	}
	return r
}

func (r *request) JsonBody(data interface{}) Request {
	r.RequestOptions.Headers["Content-Type"] = "application/json"
	return r.Body(func() (io.Reader, error) {
		if data, err := json.Marshal(data); err != nil {
			return nil, err
		} else {
			return bytes.NewReader(data), nil
		}
	})
}

func (r *request) PreProcessor(processor PreProcessor) Request {
	r.RequestOptions.PreProcessor = processor
	return r
}

func (r *request) PostProcessor(processor PostProcessor) Request {
	r.RequestOptions.PostProcessor = processor
	return r
}

func (r *request) JsonResult(data interface{}) Request {
	r.resultHandler = JsonResultHandler(data)
	return r
}

func (r *request) FileResult(writer io.Writer) Request {
	r.resultHandler = func(ctx context.Context, r *http.Response) error {
		if r.Body == nil {
			return nil
		}
		defer r.Body.Close()
		_, err := io.Copy(writer, r.Body)
		return err
	}
	return r
}

func (r *request) Result(handler ResultHandler) Request {
	r.resultHandler = handler
	return r
}

func (r *request) FormBody() Request {
	r.RequestOptions.Headers["Content-Type"] = "application/x-www-form-urlencoded"
	return r.Body(func() (io.Reader, error) {
		form := make(url.Values, len(r.forms))
		for k, v := range r.forms {
			form.Set(k, v)
		}
		return strings.NewReader(form.Encode()), nil
	})
}

func (r *request) Form(key string, value interface{}) Request {
	r.forms[key] = GetValue(value)
	return r
}

func (r *request) Forms(items ...interface{}) Request {
	for _, item := range items {
		SetMap(item, r.forms, "form")
	}
	return r
}

func (r *request) Query(key string, value interface{}) Request {
	r.queries[key] = []string{GetValue(value)}
	return r
}

func (r *request) QueryArray(key string, values []string) Request {
	// 拷贝一份，避免调用方后续修改切片影响已构建的请求。
	r.queries[key] = slices.Clone(values)
	return r
}

func (r *request) Queries(items ...interface{}) Request {
	kvMap := map[string]string{}
	for _, item := range items {
		SetMap(item, kvMap, "form")
	}
	for k, v := range kvMap {
		r.queries[k] = []string{v}
	}
	return r
}

func (r *request) Bearer(token string) Request {
	var bearer = "Bearer " + token
	return r.Header("Authorization", bearer)
}

func (r *request) BasicAuth(username string, password string) Request {
	r.RequestOptions.Headers["Authorization"] = "Basic " + BasicAuth(username, password)
	return r
}

func (r *request) Cookie(key string, value interface{}) Request {
	r.RequestOptions.Cookies[key] = GetValue(value)
	return r
}

func (r *request) Cookies(items ...interface{}) Request {
	for _, item := range items {
		SetMap(item, r.RequestOptions.Cookies, "cookie")
	}
	return r
}

func (r *request) Header(key string, value interface{}) Request {
	r.RequestOptions.Headers[key] = GetValue(value)
	return r
}

func (r *request) Headers(items ...interface{}) Request {
	for _, item := range items {
		SetMap(item, r.RequestOptions.Headers, "header")
	}
	return r
}

func (r *request) FileBody(body FileBody) Request {
	r.body = func() (io.Reader, string, error) {
		pipeReader, pipeWriter := io.Pipe()
		writer := multipart.NewWriter(pipeWriter)
		go func() {
			// 任何一步失败都通过 CloseWithError 传给读端，最终由 Client.Do 返回给调用方。
			pipeWriter.CloseWithError(writeMultipart(writer, body, r.forms))
		}()
		return pipeReader, writer.FormDataContentType(), nil
	}
	return r
}

func writeMultipart(writer *multipart.Writer, body FileBody, forms map[string]string) error {
	part, err := writer.CreateFormFile(body.FileFieldName, body.FileName)
	if err != nil {
		return err
	}
	reader, err := body.ReaderSupplier()
	if err != nil {
		return err
	}
	if closer, ok := reader.(io.Closer); ok {
		defer closer.Close()
	}
	if _, err = io.Copy(part, reader); err != nil {
		return err
	}
	for k, v := range forms {
		if err = writer.WriteField(k, v); err != nil {
			return err
		}
	}
	return writer.Close()
}

type FileBody struct {
	FileName      string
	FileFieldName string
	// 防止在有全局重试的情况下，上传失败但是文件已被读取的情况
	ReaderSupplier func() (io.Reader, error)
}
