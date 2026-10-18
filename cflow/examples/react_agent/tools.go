package main

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	"github.com/k0923/go/cflow"
)

// Tools dispatches one tool call. It is a single Action type so ChatFlow can
// fan calls out with f.Go regardless of which tool the model picked.
// Tools 分发一次工具调用。统一成一个 Action 类型，ChatFlow 就能用 f.Go 并行扇出，
// 而不必关心模型选了哪个工具。
type Tools struct {
	Console *Console
	Flaky   float64
}

func toolSpecs() []ToolSpec {
	return append(researchSpecs(),
		ToolSpec{Name: "research", Description: "派生一个子 Agent 调研主题（并行）"},
		ToolSpec{Name: "send_email", Description: "发送邮件", Sensitive: true},
		ToolSpec{Name: "delete_file", Description: "删除文件", Sensitive: true},
	)
}

// researchSpecs are the read-only tools a sub-agent may use.
// researchSpecs 是子 Agent 可用的只读工具。
func researchSpecs() []ToolSpec {
	return []ToolSpec{
		{Name: "get_weather", Description: "查询城市天气"},
		{Name: "calculate", Description: "计算四则运算表达式"},
		{Name: "current_time", Description: "获取当前时间"},
		{Name: "web_search", Description: "搜索网页"},
	}
}

func (t *Tools) Run(ctx context.Context, call ToolCall) (ToolResult, error) {
	// ActionKey is stable across retries and replays: a real email/payment API
	// would use it (plus the run ID) as the idempotency key.
	// ActionKey 在重试与回放间保持稳定：真实的邮件/支付 API 应把它（加上 run ID）作为幂等键。
	idem := cflow.ActionKey(ctx)
	t.Console.Println(dim, "  ⚙  [%s] 执行 %s(%s)", idem, call.Name, formatArgs(call.Args))
	latency := 200*time.Millisecond + time.Duration(rand.IntN(800))*time.Millisecond
	if err := sleepCtx(ctx, latency); err != nil {
		return ToolResult{}, err
	}
	if t.Flaky > 0 && rand.Float64() < t.Flaky {
		t.Console.Println(red, "  ⚠  [%s] 模拟超时（将由 RetryPolicy 重试）", idem)
		return ToolResult{}, errors.New("upstream timeout")
	}
	content, err := t.dispatch(call, idem)
	if err != nil {
		// Permanent: a bad argument will not get better by retrying. Run
		// returns *ActionError and the flow feeds it back to the model.
		// Permanent：参数错误重试也没用。Run 返回 *ActionError，Flow 把它作为观察结果交给模型。
		t.Console.Println(red, "  ✗  [%s] %v", idem, err)
		return ToolResult{}, cflow.Permanent(err)
	}
	t.Console.Println(green, "  ✓  [%s] %s (%s)", idem, content, latency.Round(time.Millisecond))
	return ToolResult{CallID: call.ID, Name: call.Name, Content: content}, nil
}

func (t *Tools) dispatch(call ToolCall, idem string) (string, error) {
	switch call.Name {
	case "get_weather":
		return weather(call.Args["city"], call.Args["date"]), nil
	case "calculate":
		v, err := evaluate(call.Args["expression"])
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s = %s", call.Args["expression"], strconv.FormatFloat(v, 'f', -1, 64)), nil
	case "current_time":
		loc, err := time.LoadLocation(call.Args["timezone"])
		if err != nil {
			loc = time.Local
		}
		return "现在是 " + time.Now().In(loc).Format("2006-01-02 15:04:05 MST"), nil
	case "web_search":
		return fmt.Sprintf("搜索「%s」的前 3 条结果：①%s - 维基百科 ②%s 入门教程 ③%s 最新动态", call.Args["query"], call.Args["query"], call.Args["query"], call.Args["query"]), nil
	case "send_email":
		return fmt.Sprintf("邮件已发送给 %s（message_id=msg_%x）", call.Args["to"], hash(idem)), nil
	case "delete_file":
		return fmt.Sprintf("已删除 %s（模拟，没有真的删除）", call.Args["path"]), nil
	default:
		return "", fmt.Errorf("unknown tool %q", call.Name)
	}
}

// weather is deterministic per city+date so repeated questions agree.
// weather 对同一城市+日期是确定的，重复提问结果一致。
func weather(city, date string) string {
	h := hash(city + date)
	conditions := []string{"晴", "多云", "阴", "小雨", "雷阵雨", "大风"}
	cond := conditions[h%uint32(len(conditions))]
	low := int(h>>8%15) + 5
	return fmt.Sprintf("%s %s：%s，%d~%d°C，湿度 %d%%", city, date, cond, low, low+int(h>>16%10)+3, 30+int(h>>4%60))
}

func hash(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}

// evaluate is a tiny recursive-descent parser for + - * / ^ and parentheses.
// evaluate 是一个支持 + - * / ^ 和括号的小型递归下降解析器。
func evaluate(expr string) (float64, error) {
	r := strings.NewReplacer("x", "*", "×", "*", "÷", "/", " ", "")
	p := &parser{s: r.Replace(expr)}
	v, err := p.sum()
	if err == nil && p.i < len(p.s) {
		err = fmt.Errorf("unexpected %q at %d", p.s[p.i], p.i)
	}
	return v, err
}

type parser struct {
	s string
	i int
}

func (p *parser) peek() byte {
	if p.i < len(p.s) {
		return p.s[p.i]
	}
	return 0
}

func (p *parser) sum() (float64, error) {
	v, err := p.product()
	for err == nil && (p.peek() == '+' || p.peek() == '-') {
		op := p.s[p.i]
		p.i++
		var r float64
		if r, err = p.product(); op == '+' {
			v += r
		} else {
			v -= r
		}
	}
	return v, err
}

func (p *parser) product() (float64, error) {
	v, err := p.power()
	for err == nil && (p.peek() == '*' || p.peek() == '/' || p.peek() == '%') {
		op := p.s[p.i]
		p.i++
		var r float64
		if r, err = p.power(); err != nil {
			break
		}
		switch {
		case op == '*':
			v *= r
		case r == 0:
			return 0, errors.New("division by zero")
		case op == '/':
			v /= r
		default:
			v = float64(int64(v) % int64(r))
		}
	}
	return v, err
}

func (p *parser) power() (float64, error) {
	v, err := p.atom()
	if err == nil && p.peek() == '^' {
		p.i++
		var r float64
		if r, err = p.power(); err == nil {
			result := 1.0
			for range int(r) {
				result *= v
			}
			v = result
		}
	}
	return v, err
}

func (p *parser) atom() (float64, error) {
	switch c := p.peek(); {
	case c == '(':
		p.i++
		v, err := p.sum()
		if err == nil && p.peek() != ')' {
			err = errors.New("missing )")
		}
		p.i++
		return v, err
	case c == '-':
		p.i++
		v, err := p.atom()
		return -v, err
	default:
		start := p.i
		for p.i < len(p.s) && (p.s[p.i] >= '0' && p.s[p.i] <= '9' || p.s[p.i] == '.') {
			p.i++
		}
		if start == p.i {
			return 0, fmt.Errorf("expected number at %d", start)
		}
		return strconv.ParseFloat(p.s[start:p.i], 64)
	}
}
