package fluent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func readAll(t *testing.T, src string) []Event {
	t.Helper()
	er := NewEventReader(strings.NewReader(src), 0)
	var out []Event
	for {
		ev, err := er.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
}

func TestEventReaderSpec(t *testing.T) {
	src := "\xef\xbb\xbf: comment\n" +
		"data: a\n" +
		"data:b\n" +
		"\n" +
		"event: delta\r\n" +
		"id: 7\r\n" +
		"retry: 1500\r\n" +
		"data: {\"x\":1}\r\n" +
		"\r\n" +
		"id: 8\n\n" + // 无 data：不派发，但 id 生效
		"data: lone-cr\r\r" +
		"data\n\n" + // 只有字段名：data 为空字符串
		"data: dropped-without-blank-line"
	got := readAll(t, src)
	want := []Event{
		{Type: "message", Data: []byte("a\nb")},
		{ID: "7", Type: "delta", Data: []byte(`{"x":1}`), Retry: 1500 * time.Millisecond},
		{ID: "8", Type: "message", Data: []byte("lone-cr")},
		{ID: "8", Type: "message", Data: []byte("")},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d events: %+v", len(got), got)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.ID != w.ID || g.Type != w.Type || string(g.Data) != string(w.Data) || g.Retry != w.Retry {
			t.Errorf("event %d = %+v, want %+v", i, g, w)
		}
	}
}

func TestEventReaderCRAtBufferBoundary(t *testing.T) {
	// iotest 风格：一次只返回 1 字节，覆盖 \r 恰好落在缓冲区末尾的情况。
	er := NewEventReader(&oneByteReader{s: "data: x\r\n\r\ndata: y\r\r"}, 0)
	for _, want := range []string{"x", "y"} {
		ev, err := er.Next()
		if err != nil || string(ev.Data) != want {
			t.Fatalf("ev=%q err=%v, want %q", ev.Data, err, want)
		}
	}
	if _, err := er.Next(); err != io.EOF {
		t.Fatalf("err = %v", err)
	}
}

type oneByteReader struct {
	s string
	i int
}

func (r *oneByteReader) Read(p []byte) (int, error) {
	if r.i >= len(r.s) {
		return 0, io.EOF
	}
	p[0] = r.s[r.i]
	r.i++
	return 1, nil
}

func TestEventReaderTooLarge(t *testing.T) {
	er := NewEventReader(strings.NewReader("data: "+strings.Repeat("x", 100)+"\n\n"), 32)
	if _, err := er.Next(); !errors.Is(err, ErrEventTooLarge) {
		t.Fatalf("line: err = %v", err)
	}
	er = NewEventReader(strings.NewReader(strings.Repeat("data: 0123456789\n", 5)+"\n"), 32)
	if _, err := er.Next(); !errors.Is(err, ErrEventTooLarge) {
		t.Fatalf("event: err = %v", err)
	}
}

type chatReq struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

type chatChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
}

// sseHandler 模拟 OpenAI 兼容接口：逐条 flush，每条之间间隔 gap。
func sseHandler(t *testing.T, gap time.Duration, frames ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("Accept = %q", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		flusher := w.(http.Flusher)
		// 真实的 LLM 服务会先发送响应头，首个 token 之后再陆续推送事件。
		flusher.Flush()
		for _, f := range frames {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(gap):
			}
			fmt.Fprint(w, f)
			flusher.Flush()
		}
	}
}

func TestEventsOfOpenAIStyle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		frames := []string{
			": keep-alive\n\n",
			`data: {"choices":[{"delta":{"content":"Hel"}}]}` + "\n\n",
			`data: {"choices":[{"delta":{"content":"lo"}}]}` + "\n\n",
			"data: [DONE]\n\n",
		}
		// 每条间隔 40s：总时长 160s 远超 30s 的单次尝试超时，证明连接建立后改由空闲超时约束。
		c := newTestClient(t, sseHandler(t, 40*time.Second, frames...))
		var sb strings.Builder
		for chunk, err := range c.Post("/chat/completions").
			JSON(chatReq{Model: "m", Stream: true}).
			EventsOf[chatChunk](t.Context(), SkipData("[DONE]")) {
			if err != nil {
				t.Fatal(err)
			}
			sb.WriteString(chunk.Choices[0].Delta.Content)
		}
		if sb.String() != "Hello" {
			t.Fatalf("content = %q", sb.String())
		}
	})
}

func TestEventsAnthropicStyle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		frames := []string{
			"event: message_start\ndata: {\"type\":\"message_start\"}\n\n",
			"event: ping\ndata: {\"type\": \"ping\"}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\"}\n\n",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		}
		c := newTestClient(t, sseHandler(t, time.Millisecond, frames...))
		var types []string
		for ev, err := range c.Post("/messages").JSON(chatReq{}).Events(t.Context()) {
			if err != nil {
				t.Fatal(err)
			}
			if SkipType("ping")(ev) {
				continue
			}
			types = append(types, ev.Type)
		}
		if got := strings.Join(types, ","); got != "message_start,content_block_delta,message_stop" {
			t.Fatalf("types = %s", got)
		}
	})
}

func TestEventsAttemptTimeoutBeforeHeaders(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(time.Hour):
			}
		}, WithTimeout(5*time.Second))
		start := time.Now()
		for _, err := range c.Get("/s").Events(t.Context()) {
			if !errors.Is(err, ErrTimeout) || !errors.Is(err, errAttemptTimeout) {
				t.Fatalf("err = %v", err)
			}
		}
		if time.Since(start) != 5*time.Second {
			t.Fatalf("elapsed = %v, attempt timeout must bound time-to-headers", time.Since(start))
		}
	})
}

func TestEventsIdleTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newTestClient(t, sseHandler(t, 0, "data: 1\n\n", "data: 2\n\n"), WithIdleTimeout(10*time.Second))
		// 第二条前服务端静默 1 小时。
		c2 := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: 1\n\n")
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
			case <-time.After(time.Hour):
			}
		}, WithIdleTimeout(10*time.Second))

		n := 0
		for _, err := range c.Get("/s").Events(t.Context()) {
			if err != nil {
				t.Fatal(err)
			}
			n++
		}
		if n != 2 {
			t.Fatalf("n = %d", n)
		}

		start := time.Now()
		var last error
		n = 0
		for _, err := range c2.Get("/s").Events(t.Context()) {
			if err != nil {
				last = err
				break
			}
			n++
		}
		if n != 1 || !errors.Is(last, ErrTimeout) || !errors.Is(last, errIdleTimeout) {
			t.Fatalf("n=%d err=%v", n, last)
		}
		if elapsed := time.Since(start); elapsed != 10*time.Second {
			t.Fatalf("elapsed = %v, want idle timeout 10s", elapsed)
		}
	})
}

func TestEventsBreakClosesConnection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var closed atomic.Bool
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			for i := 0; ; i++ {
				if _, err := fmt.Fprintf(w, "data: %d\n\n", i); err != nil {
					closed.Store(true)
					return
				}
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
					closed.Store(true)
					return
				case <-time.After(time.Second):
				}
			}
		})
		n := 0
		for _, err := range c.Get("/s").Events(t.Context()) {
			if err != nil {
				t.Fatal(err)
			}
			if n++; n == 3 {
				break
			}
		}
		// 推进假时钟，让服务端从计时器中醒来并观察到断开（Go 1.27 synctest.Sleep）。
		synctest.Sleep(2 * time.Second)
		if !closed.Load() {
			t.Fatal("server must observe disconnect after break")
		}
	})
}

func TestEventsCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newTestClient(t, sseHandler(t, time.Second, "data: 1\n\n", "data: 2\n\n", "data: 3\n\n"))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var last error
		n := 0
		for _, err := range c.Get("/s").Events(ctx) {
			if err != nil {
				last = err
				break
			}
			if n++; n == 1 {
				cancel()
			}
		}
		if !errors.Is(last, ErrCanceled) {
			t.Fatalf("err = %v", last)
		}
	})
}

func TestEventsStatusErrorAndRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			switch calls.Add(1) {
			case 1:
				w.WriteHeader(http.StatusTooManyRequests)
			case 2:
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"code":"BAD_MODEL","message":"unknown model"}`)
			}
		}, WithRetry(RetryPolicy{MaxAttempts: 3, RetryNonIdempotent: true, Backoff: ConstantBackoff(time.Second)}),
			WithErrorDecoder(func(resp *http.Response, body []byte) error {
				var e apiError
				if DefaultCodec.Decode(strings.NewReader(string(body)), &e) != nil {
					return nil
				}
				return &e
			}))
		var last error
		for _, err := range c.Post("/chat").JSON(chatReq{}).Events(t.Context()) {
			last = err
		}
		biz, ok := errors.AsType[*apiError](last)
		if !errors.Is(last, ErrStatus) || !ok || biz.Code != "BAD_MODEL" || calls.Load() != 2 {
			t.Fatalf("err=%v calls=%d", last, calls.Load())
		}
	})
}

func TestEventsWrongContentType(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"error":"stream not supported"}`)
	})
	for _, err := range c.Get("/s").Events(t.Context()) {
		fe, ok := errors.AsType[*Error](err)
		if !ok || fe.Kind != ErrDecode || !strings.Contains(string(fe.Body), "stream not supported") {
			t.Fatalf("err = %v", err)
		}
	}
}

func TestEventsOfDecodeError(t *testing.T) {
	c := newTestClient(t, sseHandler(t, 0, "data: not-json\n\n"))
	for _, err := range c.Get("/s").EventsOf[chatChunk](t.Context(), nil) {
		if !errors.Is(err, ErrDecode) {
			t.Fatalf("err = %v", err)
		}
	}
}

func TestEventsMaxEventSize(t *testing.T) {
	c := newTestClient(t, sseHandler(t, 0, "data: "+strings.Repeat("x", 64)+"\n\n"), WithMaxEventSize(16))
	for _, err := range c.Get("/s").Events(t.Context()) {
		if !errors.Is(err, ErrDecode) || !errors.Is(err, ErrEventTooLarge) {
			t.Fatalf("err = %v", err)
		}
	}
}

func BenchmarkEventReader(b *testing.B) {
	frame := `data: {"choices":[{"delta":{"content":"hello world"}}]}` + "\n\n"
	src := strings.Repeat(frame, 1000)
	b.SetBytes(int64(len(src)))
	for b.Loop() {
		er := NewEventReader(strings.NewReader(src), 0)
		for {
			if _, err := er.Next(); err != nil {
				break
			}
		}
	}
}
