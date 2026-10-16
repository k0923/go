package fluent

import (
	"context"
	"maps"
	"net/http"
)

type RetryHandler func(ctx context.Context, resp *http.Response, err error) (error, bool)

// RetryStrategy 每次发送请求时调用一次，返回本次请求专属的 RetryHandler，
// 因此有状态的计数器不会在并发请求之间共享。
type RetryStrategy func() RetryHandler

// RequestOptions 在 Client/Request 创建时被深拷贝；创建之后再修改原对象不会影响已创建的实例。
// 注意：修改 DefaultOptions 的 map 与并发发起请求之间没有同步，应只在程序初始化阶段修改。
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
	if options == nil {
		return newOptions
	}
	maps.Copy(newOptions.Headers, options.Headers)
	maps.Copy(newOptions.Cookies, options.Cookies)
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
