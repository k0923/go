// Package fluent 是面向业务服务间调用的 HTTP 客户端。
//
// 设计要点：
//
//   - [Client] 创建后不可变，并发安全；用 [Client.With] 派生带不同配置的子客户端。
//   - [Request] 是单次调用的构建器，构建阶段不保证并发安全，构建完成后 Do 系列方法可以并发调用。
//   - context 作为执行方法的第一个参数显式传入，不再挂在构建器上。
//   - 默认把非 2xx 视为错误，所有错误统一为 [*Error]，用 errors.Is(err, fluent.ErrStatus) 等哨兵分类。
//   - 重试默认只作用于幂等方法与可重放的请求体，并遵守 Retry-After。
//   - 横切逻辑（日志、鉴权、指标）统一通过 [Middleware] 扩展，每次尝试都会经过中间件链。
//
// 典型用法：
//
//	c := fluent.MustNew(
//		fluent.WithBaseURL("https://api.example.com/api"),
//		fluent.WithRetry(fluent.RetryPolicy{MaxAttempts: 3}),
//	)
//	user, err := c.Get("/users/{id}").Path("id", "42").Decode[User](ctx)
package fluent
