package fluent

import (
	"context"
	"io"
	"net/http"
)

var MaxRetryTimes uint = 100
var DefaultOptions = NewRequestOptions()

type BodySupplier func() (io.Reader, error)
type ResultHandler func(context.Context, *http.Response) error

type PreProcessor func(ctx context.Context, request *http.Request)

// PostProcessor 在ResultHandler之前执行; err是http.Client.Do(request)的报错，例如超时
type PostProcessor func(ctx Context, response *http.Response, err error)

func Post(url string) Request {
	return newRequest(url, http.MethodPost, &DefaultOptions)
}

func Get(url string) Request {
	return newRequest(url, http.MethodGet, &DefaultOptions)
}

func Put(url string) Request {
	return newRequest(url, http.MethodPut, &DefaultOptions)
}

func Patch(url string) Request {
	return newRequest(url, http.MethodPatch, &DefaultOptions)
}

func Delete(url string) Request {
	return newRequest(url, http.MethodDelete, &DefaultOptions)
}

func Options(url string) Request {
	return newRequest(url, http.MethodOptions, &DefaultOptions)
}

func Head(url string) Request {
	return newRequest(url, http.MethodHead, &DefaultOptions)
}
