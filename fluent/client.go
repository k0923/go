package fluent

import (
	"net/http"
)

var DefaultClient = &client{}

type Client interface {
	Post(url string) Request
	Get(url string) Request
	Put(url string) Request
	Patch(url string) Request
	Delete(url string) Request
	Options(url string) Request
	Head(url string) Request
}

func NewClient(options *RequestOptions) Client {
	return &client{
		RequestOptions: options.Copy(),
	}
}

type client struct {
	RequestOptions
}

func (client *client) Post(url string) Request {
	return newRequest(url, http.MethodPost, &client.RequestOptions)
}
func (client *client) Get(url string) Request {
	return newRequest(url, http.MethodGet, &client.RequestOptions)
}
func (client *client) Put(url string) Request {
	return newRequest(url, http.MethodPut, &client.RequestOptions)
}
func (client *client) Patch(url string) Request {
	return newRequest(url, http.MethodPatch, &client.RequestOptions)
}
func (client *client) Delete(url string) Request {
	return newRequest(url, http.MethodDelete, &client.RequestOptions)
}
func (client *client) Options(url string) Request {
	return newRequest(url, http.MethodOptions, &client.RequestOptions)
}
func (client *client) Head(url string) Request {
	return newRequest(url, http.MethodHead, &client.RequestOptions)
}

// Deprecated: Use Init Options instead.
func (client *client) Header(key string, value interface{}) Client {
	client.RequestOptions.Headers[key] = GetValue(value)
	return client
}

// Deprecated: Use Init Options instead.
func (client *client) Headers(items ...interface{}) Client {
	for _, item := range items {
		SetMap(item, client.RequestOptions.Headers, "header")
	}
	return client
}

// Deprecated: Use Init Options instead.
func (client *client) HeaderObj(items ...interface{}) Client {
	return client.Headers(items...)
}

// Deprecated: Use Init Options instead.
func (client *client) Retry(strategy RetryStrategy) Client {
	client.RetryStrategy = strategy
	return client
}
