// Run with: go run ./condition/examples/integration
// 运行方式：go run ./condition/examples/integration
//
// How condition fits into a real service without xjson.G:
//   - rules embedded in business config as json.RawMessage, compiled strictly;
//   - custom business logic exposed to JSON rules as computed Schema fields;
//   - one compiled rule shared by many goroutines.
//
// 在不依赖 xjson.G 的前提下，将 condition 接入真实服务：
//   - 规则以 json.RawMessage 嵌入业务配置，并进行严格编译；
//   - 自定义业务逻辑以 Schema 计算字段的形式暴露给 JSON 规则；
//   - 一条编译后的规则被多个 goroutine 共享。
package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	c "github.com/k0923/go/condition"
)

type Request struct {
	UserID    int64
	Path      string
	Header    map[string]string
	Timestamp time.Time
}

// RouteConfig is a typical business config. The rule stays raw until it is
// compiled, so decoding is strict (unknown keys rejected) and errors carry a
// JSON path.
// RouteConfig 是典型的业务配置。规则在编译前保持原始 JSON，
// 这样解码是严格的（拒绝未知字段），错误也带有 JSON 路径。
type RouteConfig struct {
	Name    string          `json:"name"`
	Backend string          `json:"backend"`
	Rule    json.RawMessage `json:"rule"`
}

type Route struct {
	Name    string
	Backend string
	Match   c.Condition[Request]
}

// requestSchema is the extension point for JSON rules: anything expressible
// as a Go function of the input becomes a field. There is no plugin registry
// and no interface to implement.
// requestSchema 是 JSON 规则的扩展点：任何以输入为参数的 Go 函数都可以注册为字段。
// 不需要插件注册表，也不需要实现任何接口。
func requestSchema() *c.Schema[Request] {
	return c.NewSchema[Request]().
		Number("user_id", func(r Request) int64 { return r.UserID }).
		String("path", func(r Request) string { return r.Path }).
		// Computed fields: derived values and business predicates.
		// 计算字段：派生值与业务谓词。
		Number("user_bucket", func(r Request) int64 { return r.UserID % 100 }).
		String("client", func(r Request) string { return strings.ToLower(r.Header["X-Client"]) }).
		Bool("is_weekend", func(r Request) bool {
			d := r.Timestamp.Weekday()
			return d == time.Saturday || d == time.Sunday
		}).
		Strings("path_segments", func(r Request) []string { return strings.Split(strings.Trim(r.Path, "/"), "/") })
}

const config = `[
  {"name": "canary", "backend": "v2", "rule": {"type": "and", "items": [
    {"type": "string", "x": "path", "opt": "starts_with", "y": "/api/"},
    {"type": "number", "x": "user_bucket", "opt": "lt", "y": 10}
  ]}},
  {"name": "mobile-weekend", "backend": "mobile", "rule": {"type": "and", "items": [
    {"type": "string", "x": "client", "opt": "in", "y": ["ios", "android"]},
    {"type": "boolean", "x": "is_weekend", "opt": "eq", "y": true}
  ]}},
  {"name": "admin", "backend": "internal", "rule":
    {"type": "array", "x": "path_segments", "opt": "any", "y": {"type": "string", "x": "$", "opt": "eq", "y": "admin"}}
  },
  {"name": "default", "backend": "v1", "rule": {"type": "and"}}
]`

func main() {
	routes, err := loadRoutes([]byte(config), requestSchema())
	if err != nil {
		panic(err)
	}

	saturday := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	monday := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	reqs := []Request{
		{UserID: 1205, Path: "/api/orders", Timestamp: monday},
		{UserID: 1299, Path: "/api/orders", Header: map[string]string{"X-Client": "iOS"}, Timestamp: saturday},
		{UserID: 1299, Path: "/ops/admin/users", Timestamp: monday},
		{UserID: 1299, Path: "/home", Timestamp: monday},
	}
	for _, r := range reqs {
		route, pickErr := pick(routes, r)
		if pickErr != nil {
			panic(pickErr)
		}
		fmt.Printf("user=%d path=%-18s -> %s (%s)\n", r.UserID, r.Path, route.Backend, route.Name)
	}

	// Compiled rules are immutable: share them freely across goroutines.
	// 编译后的规则不可变：可在多个 goroutine 之间自由共享。
	var canary atomic.Int64
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 1000 {
				r := Request{UserID: int64(g*1000 + i), Path: "/api/x", Timestamp: monday}
				if ok, _ := routes[0].Match.Match(r); ok {
					canary.Add(1)
				}
			}
		})
	}
	wg.Wait()
	fmt.Printf("canary share over 8000 requests: %d%%\n", canary.Load()*100/8000)

	// A broken config fails at load time, naming the route and the JSON path.
	// 错误的配置在加载时即失败，并指出路由名称与 JSON 路径。
	_, err = loadRoutes([]byte(`[{"name":"typo","backend":"x","rule":{"type":"number","x":"user_buckt","opt":"lt","y":10}}]`), requestSchema())
	fmt.Println("load error:", err)
}

func loadRoutes(data []byte, schema *c.Schema[Request]) ([]Route, error) {
	var cfgs []RouteConfig
	if err := json.Unmarshal(data, &cfgs); err != nil {
		return nil, err
	}
	routes := make([]Route, 0, len(cfgs))
	for _, cfg := range cfgs {
		m, err := schema.CompileJSON(cfg.Rule)
		if err != nil {
			return nil, fmt.Errorf("route %q: %w", cfg.Name, err)
		}
		routes = append(routes, Route{Name: cfg.Name, Backend: cfg.Backend, Match: m})
	}
	return routes, nil
}

func pick(routes []Route, r Request) (Route, error) {
	for _, route := range routes {
		ok, err := route.Match.Match(r)
		if err != nil {
			return Route{}, fmt.Errorf("route %q: %w", route.Name, err)
		}
		if ok {
			return route, nil
		}
	}
	return Route{}, fmt.Errorf("no route for %s", r.Path)
}
