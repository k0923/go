package fluent

import (
	"context"
	"net/http"
)

type RetryStrategy func(ctx context.Context, resp *http.Response, err error) (error, bool)

type RequestOptions struct {
	Headers       map[string]string
	Cookies       map[string]string
	Client        *http.Client
	RetryStrategy RetryStrategy
	PreProcessor  PreProcessor
	PostProcessor PostProcessor
}

func (options *RequestOptions) Copy() RequestOptions {
	newOptions := NewRequestOptions()
	for k, v := range options.Headers {
		newOptions.Headers[k] = v
	}
	for k, v := range options.Cookies {
		newOptions.Cookies[k] = v
	}
	newOptions.RetryStrategy = options.RetryStrategy
	newOptions.PreProcessor = options.PreProcessor
	newOptions.PostProcessor = options.PostProcessor
	newOptions.Client = options.Client
	return newOptions
}

func NewRequestOptions() RequestOptions {
	return RequestOptions{
		Headers: make(map[string]string),
		Cookies: make(map[string]string),
		Client:  http.DefaultClient,
	}
}
