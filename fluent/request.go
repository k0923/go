package fluent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"
)

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
	Response() (*http.Response, error)
	Retry(strategy RetryStrategy) Request
	PreProcessor(processor PreProcessor) Request
	PostProcessor(processor PostProcessor) Request
	GetContext() context.Context
	Send() error
}

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
	queries       map[string][]string
	forms         map[string]string
	ctx           context.Context
	resultHandler ResultHandler
	bodySupplier  BodySupplier
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

func (r *request) Response() (*http.Response, error) {
	var sendFn func() (*http.Response, error)
	maxCount := MaxRetryTimes

	var retryHandler RetryHandler
	if r.RetryStrategy != nil {
		retryHandler = r.RetryStrategy()
	}

	sendFn = func() (*http.Response, error) {
		start := time.Now()
		if request, err := r.Build(); err != nil {
			return nil, err
		} else {
			if r.RequestOptions.PreProcessor != nil {
				r.RequestOptions.PreProcessor(r.ctx, request)
			}
			resp, err := r.RequestOptions.Client.Do(request)
			if r.RequestOptions.PostProcessor != nil {
				r.RequestOptions.PostProcessor(Context{
					Context:     r.ctx,
					ElapsedTime: time.Since(start),
				}, resp, err)
			}
			if maxCount > 0 {
				maxCount--
				if retryHandler != nil {
					if result, needRetry := retryHandler(r.ctx, resp, err); result != nil && needRetry {
						return sendFn()
					} else {
						err = result
					}
				}
			}
			return resp, err
		}
	}

	return sendFn()
}

func (r *request) Build() (*http.Request, error) {
	var body io.Reader
	var err error
	if r.bodySupplier != nil {
		if body, err = r.bodySupplier(); err != nil {
			return nil, err
		}
	}
	if r.RequestOptions.Client == nil {
		r.RequestOptions.Client = http.DefaultClient
	}
	var request *http.Request

	if r.ctx == nil {
		r.ctx = context.Background()
	}
	if request, err = http.NewRequestWithContext(r.ctx, r.method, r.url, body); err != nil {
		return nil, err
	}
	for k, v := range r.RequestOptions.Headers {
		request.Header.Set(k, v)
	}
	q := request.URL.Query()
	for k, values := range r.queries {
		for _, v := range values {
			q.Add(k, v)
		}
	}
	request.URL.RawQuery = q.Encode()
	for k, v := range r.RequestOptions.Cookies {
		request.AddCookie(&http.Cookie{Name: k, Value: v, HttpOnly: true})
	}

	return request, nil
}

func (r *request) Send() error {
	response, err := r.Response()
	if err != nil {
		return err
	}
	if response != nil && r.resultHandler != nil {
		defer response.Body.Close()
		return r.resultHandler(r.ctx, response)
	}

	return nil
}

func (r *request) Context(ctx context.Context) Request {
	r.ctx = ctx
	return r
}

func (r *request) GetContext() context.Context {
	return r.ctx
}

func (r *request) Body(supplier BodySupplier) Request {
	r.bodySupplier = supplier
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
	r.bodySupplier = func() (io.Reader, error) {
		form := make(url.Values)
		for k, v := range r.forms {
			form.Set(k, v)
		}
		return strings.NewReader(form.Encode()), nil
	}
	return r
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
	r.queries[key] = values
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
	r.bodySupplier = func() (io.Reader, error) {
		pipeReader, pipeWriter := io.Pipe()
		writer := multipart.NewWriter(pipeWriter)
		go func() {
			defer pipeWriter.Close()
			defer writer.Close()
			part, err := writer.CreateFormFile(body.FileFieldName, body.FileName)
			if err != nil {
				fmt.Println(err.Error())
				return
			}
			reader, err := body.ReaderSupplier()
			if err != nil {
				fmt.Printf("fail to get file body with err:%v\n", err)
				return
			}
			if closer, ok := reader.(io.Closer); ok {
				defer closer.Close()
			}
			_, err = io.Copy(part, reader)
			if err != nil {
				fmt.Println(err.Error())
				return
			}
			fmt.Println(r.forms)
			for k, v := range r.forms {
				_ = writer.WriteField(k, v)
			}
		}()

		r.RequestOptions.Headers["Content-Type"] = writer.FormDataContentType()
		return pipeReader, nil
	}

	return r
}

type FileBody struct {
	FileName      string
	FileFieldName string
	// 防止在有全局重试的情况下，上传失败但是文件已被读取的情况
	ReaderSupplier func() (io.Reader, error)
}
