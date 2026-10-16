package fluent

import (
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"time"
)

// Client 创建后不可变，可被任意多个 goroutine 共享。
type Client struct {
	base         *url.URL
	http         *http.Client // Transport 已经包装好中间件链
	userHTTP     *http.Client // 用户传入的原始 client，With 时用于重建链
	header       http.Header
	timeout      time.Duration
	idleTimeout  time.Duration
	maxEventSize int
	retry        RetryPolicy
	codec        Codec
	errorDecoder ErrorDecoder
	accept       func(int) bool
	middlewares  []Middleware
}

// Option 配置 Client。
type Option func(*Client) error

const (
	// DefaultTimeout 是单次尝试的默认超时。对 Events 只约束到收到响应头为止。
	DefaultTimeout = 30 * time.Second
	// DefaultIdleTimeout 是 Events 的默认空闲超时：连续这么久没有收到任何字节即中断。
	// 推理类模型首个 token 前可能长时间静默，必要时用 WithIdleTimeout / IdleTimeout 调大。
	DefaultIdleTimeout = 2 * time.Minute
)

// New 创建 Client；配置非法时返回 ErrBuild 类错误。
func New(opts ...Option) (*Client, error) {
	c := &Client{
		userHTTP:    http.DefaultClient,
		header:      make(http.Header),
		timeout:     DefaultTimeout,
		idleTimeout: DefaultIdleTimeout,
		codec:       DefaultCodec,
		accept:      is2xx,
	}
	return c.apply(opts)
}

// MustNew 与 New 相同，配置非法时 panic，适合包级变量初始化。
func MustNew(opts ...Option) *Client {
	c, err := New(opts...)
	if err != nil {
		panic(err)
	}
	return c
}

// With 基于当前配置派生新 Client，原 Client 不受影响。
// 中间件会追加在已有中间件之后（更靠内层）。
func (c *Client) With(opts ...Option) (*Client, error) {
	clone := *c
	clone.header = c.header.Clone()
	clone.middlewares = slices.Clone(c.middlewares)
	if c.base != nil {
		clone.base = c.base.Clone()
	}
	return clone.apply(opts)
}

func (c *Client) apply(opts []Option) (*Client, error) {
	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, &Error{Kind: ErrBuild, Err: err}
		}
	}
	hc := *c.userHTTP
	transport := hc.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	hc.Transport = chain(transport, c.middlewares)
	c.http = &hc
	return c, nil
}

// WithBaseURL 设置基础地址，请求路径会拼接在其 path 之后；请求使用绝对 URL 时忽略。
func WithBaseURL(raw string) Option {
	return func(c *Client) error {
		u, err := url.Parse(raw)
		if err != nil {
			return err
		}
		if u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("base url %q must be absolute", raw)
		}
		c.base = u
		return nil
	}
}

// WithHTTPClient 使用自定义的 http.Client（连接池、TLS、代理等）。其 Timeout 会与 WithTimeout 同时生效。
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) error {
		if hc == nil {
			return fmt.Errorf("http client is nil")
		}
		c.userHTTP = hc
		return nil
	}
}

// WithHeader 设置所有请求都会携带的头。
func WithHeader(key, value string) Option {
	return func(c *Client) error {
		c.header.Set(key, value)
		return nil
	}
}

// WithHeaders 批量设置公共请求头。
func WithHeaders(h http.Header) Option {
	return func(c *Client) error {
		maps.Copy(c.header, h.Clone())
		return nil
	}
}

// WithUserAgent 设置 User-Agent。
func WithUserAgent(ua string) Option { return WithHeader("User-Agent", ua) }

// WithTimeout 设置单次尝试的超时，0 表示不限制。下载大文件等长连接场景应调大或关闭。
func WithTimeout(d time.Duration) Option {
	return func(c *Client) error {
		if d < 0 {
			return fmt.Errorf("negative timeout %v", d)
		}
		c.timeout = d
		return nil
	}
}

// WithIdleTimeout 设置 Events 的空闲超时，0 表示不限制（此时仅由 ctx 控制）。
func WithIdleTimeout(d time.Duration) Option {
	return func(c *Client) error {
		if d < 0 {
			return fmt.Errorf("negative idle timeout %v", d)
		}
		c.idleTimeout = d
		return nil
	}
}

// WithMaxEventSize 设置 Events 单条事件与单行的上限（字节），默认 DefaultMaxEventSize。
func WithMaxEventSize(n int) Option {
	return func(c *Client) error {
		if n <= 0 {
			return fmt.Errorf("max event size must be positive, got %d", n)
		}
		c.maxEventSize = n
		return nil
	}
}

// WithRetry 设置默认重试策略，请求级 Retry 可覆盖。
func WithRetry(p RetryPolicy) Option {
	return func(c *Client) error {
		c.retry = p
		return nil
	}
}

// WithMiddleware 追加中间件。
func WithMiddleware(mws ...Middleware) Option {
	return func(c *Client) error {
		c.middlewares = append(c.middlewares, mws...)
		return nil
	}
}

// WithCodec 替换请求/响应的编解码器。
func WithCodec(codec Codec) Option {
	return func(c *Client) error {
		if codec == nil {
			return fmt.Errorf("codec is nil")
		}
		c.codec = codec
		return nil
	}
}

// WithErrorDecoder 设置非期望状态码时的业务错误解析器。
func WithErrorDecoder(d ErrorDecoder) Option {
	return func(c *Client) error {
		c.errorDecoder = d
		return nil
	}
}

// WithAcceptStatus 设置哪些状态码视为成功，默认 2xx。
func WithAcceptStatus(accept func(code int) bool) Option {
	return func(c *Client) error {
		if accept == nil {
			return fmt.Errorf("accept func is nil")
		}
		c.accept = accept
		return nil
	}
}

func is2xx(code int) bool { return code >= 200 && code < 300 }

// NewRequest 创建请求。path 可以是绝对 URL，也可以是相对 BaseURL 的路径，支持 {name} 占位符。
func (c *Client) NewRequest(method, path string) *Request {
	return &Request{
		client: c,
		method: method,
		path:   path,
		header: make(http.Header),
		query:  make(url.Values),
	}
}

func (c *Client) Get(path string) *Request    { return c.NewRequest(http.MethodGet, path) }
func (c *Client) Post(path string) *Request   { return c.NewRequest(http.MethodPost, path) }
func (c *Client) Put(path string) *Request    { return c.NewRequest(http.MethodPut, path) }
func (c *Client) Patch(path string) *Request  { return c.NewRequest(http.MethodPatch, path) }
func (c *Client) Delete(path string) *Request { return c.NewRequest(http.MethodDelete, path) }
func (c *Client) Head(path string) *Request   { return c.NewRequest(http.MethodHead, path) }

// Default 是零配置的共享 Client，供脚本与简单场景使用；服务内应创建自己的 Client。
var Default = MustNew()

func Get(url string) *Request    { return Default.Get(url) }
func Post(url string) *Request   { return Default.Post(url) }
func Put(url string) *Request    { return Default.Put(url) }
func Patch(url string) *Request  { return Default.Patch(url) }
func Delete(url string) *Request { return Default.Delete(url) }
func Head(url string) *Request   { return Default.Head(url) }
