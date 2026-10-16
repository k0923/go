package fluent

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Kind 是错误分类，本身实现了 error，可直接作为 errors.Is 的目标。
type Kind uint8

const (
	// ErrBuild 请求构建失败：URL 非法、路径参数缺失、请求体编码失败等，请求未发出。
	ErrBuild Kind = iota + 1
	// ErrTransport 网络层失败：DNS、连接、TLS、连接被重置等，未拿到响应。
	ErrTransport
	// ErrTimeout 超时：请求级超时或 ctx deadline 到期。
	ErrTimeout
	// ErrCanceled 调用方取消了 ctx。
	ErrCanceled
	// ErrStatus 收到了响应，但状态码不在期望范围内。
	ErrStatus
	// ErrDecode 响应体无法解码到目标类型。
	ErrDecode
)

var kindNames = [...]string{
	ErrBuild:     "build",
	ErrTransport: "transport",
	ErrTimeout:   "timeout",
	ErrCanceled:  "canceled",
	ErrStatus:    "status",
	ErrDecode:    "decode",
}

func (k Kind) String() string {
	if int(k) < len(kindNames) && kindNames[k] != "" {
		return kindNames[k]
	}
	return fmt.Sprintf("kind(%d)", k)
}

func (k Kind) Error() string { return "fluent: " + k.String() + " error" }

// Error 是本包返回的唯一错误类型。
type Error struct {
	Kind   Kind
	Method string
	// URL 已脱敏（去掉 userinfo 中的密码）。
	URL string
	// StatusCode 仅在 ErrStatus / ErrDecode 时非 0。
	StatusCode int
	Header     http.Header
	// Body 是响应体的前若干字节（ErrStatus 最多 64KiB，ErrDecode 最多 4KiB），便于排查与业务错误解析。
	Body []byte
	// Attempts 是实际发出的尝试次数。
	Attempts int
	// Err 是根因：网络错误、解码错误，或 ErrorDecoder 解析出的业务错误。
	Err error
}

func (e *Error) Error() string {
	var sb strings.Builder
	sb.WriteString("fluent: ")
	if e.Method != "" {
		sb.WriteString(e.Method)
		sb.WriteByte(' ')
		sb.WriteString(e.URL)
		sb.WriteString(": ")
	}
	sb.WriteString(e.Kind.String())
	if e.StatusCode != 0 {
		fmt.Fprintf(&sb, " %d %s", e.StatusCode, http.StatusText(e.StatusCode))
	}
	if e.Attempts > 1 {
		fmt.Fprintf(&sb, " (after %d attempts)", e.Attempts)
	}
	if e.Err != nil {
		sb.WriteString(": ")
		sb.WriteString(e.Err.Error())
	}
	return sb.String()
}

func (e *Error) Unwrap() error { return e.Err }

// Is 使 errors.Is(err, fluent.ErrStatus) 这类分类判断成立。
func (e *Error) Is(target error) bool {
	k, ok := target.(Kind)
	return ok && k == e.Kind
}

// StatusCode 返回错误链中 *Error 的状态码；不是 HTTP 状态错误时返回 0。
func StatusCode(err error) int {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.StatusCode
	}
	return 0
}

// ErrorDecoder 把非期望状态的响应解析为业务错误，结果放在 Error.Err 中，可用 errors.As 取出。
// body 已被读出（最多 64KiB），resp.Body 不可再读。返回 nil 表示没有可解析的业务错误。
type ErrorDecoder func(resp *http.Response, body []byte) error
