package fluent

import (
	"log/slog"
	"net/http"
	"time"
)

// Middleware 包装 http.RoundTripper，每一次尝试（包括重试）都会经过完整的中间件链。
// 列表中第一个中间件位于最外层。
type Middleware func(next http.RoundTripper) http.RoundTripper

// RoundTripperFunc 把函数适配为 http.RoundTripper。
type RoundTripperFunc func(*http.Request) (*http.Response, error)

func (f RoundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func chain(rt http.RoundTripper, mws []Middleware) http.RoundTripper {
	for i := len(mws) - 1; i >= 0; i-- {
		rt = mws[i](rt)
	}
	return rt
}

// Logging 为每次尝试记录一条结构化日志。
func Logging(logger *slog.Logger) Middleware {
	return func(next http.RoundTripper) http.RoundTripper {
		return RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			start := time.Now()
			resp, err := next.RoundTrip(req)
			attrs := []slog.Attr{
				slog.String("method", req.Method),
				slog.String("url", redact(req.URL)),
				slog.Int("attempt", AttemptFromContext(req.Context())),
				slog.Duration("elapsed", time.Since(start)),
			}
			level := slog.LevelInfo
			if err != nil {
				level = slog.LevelWarn
				attrs = append(attrs, slog.Any("error", err))
			} else {
				attrs = append(attrs, slog.Int("status", resp.StatusCode))
				if resp.StatusCode >= 500 {
					level = slog.LevelWarn
				}
			}
			logger.LogAttrs(req.Context(), level, "fluent request", attrs...)
			return resp, err
		})
	}
}

// Header 在每次尝试前设置请求头，值由函数动态计算（如定期刷新的 token）。
func Header(key string, value func(*http.Request) (string, error)) Middleware {
	return func(next http.RoundTripper) http.RoundTripper {
		return RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			v, err := value(req)
			if err != nil {
				return nil, err
			}
			// RoundTripper 不应修改入参，按约定先克隆。
			req = req.Clone(req.Context())
			req.Header.Set(key, v)
			return next.RoundTrip(req)
		})
	}
}
