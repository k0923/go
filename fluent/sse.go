package fluent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"iter"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Event 是一条 Server-Sent Event（https://html.spec.whatwg.org/multipage/server-sent-events.html）。
type Event struct {
	// ID 是本条或此前最近一条事件的 id 字段（规范要求 id 在后续事件中延续）。
	ID string
	// Type 是 event 字段，未设置时为 "message"。
	Type string
	// Data 是所有 data 行以 "\n" 连接后的内容。
	Data []byte
	// Retry 是服务端建议的重连间隔，未设置时为 0。
	Retry time.Duration
}

// DefaultMaxEventSize 是单条事件（所有 data 行合计）以及单行的默认上限。
const DefaultMaxEventSize = 1 << 20

// EventReader 从 text/event-stream 中逐条解析事件，不负责关闭底层流。
type EventReader struct {
	sc      *bufio.Scanner
	lastID  string
	max     int
	started bool
}

// NewEventReader 创建解析器；maxEventSize<=0 时使用 DefaultMaxEventSize。
// 可独立用于解析任意 SSE 流（例如测试或自定义传输）。
func NewEventReader(r io.Reader, maxEventSize int) *EventReader {
	if maxEventSize <= 0 {
		maxEventSize = DefaultMaxEventSize
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4<<10), maxEventSize)
	sc.Split(scanLines)
	return &EventReader{sc: sc, max: maxEventSize}
}

// ErrEventTooLarge 表示单行或单条事件超过上限。
var ErrEventTooLarge = errors.New("fluent: event exceeds max size")

// Next 返回下一条事件；流正常结束时返回 io.EOF（结束前未以空行收尾的半条事件按规范丢弃）。
// 返回的 Event.Data 归调用方所有，可以安全保留。
func (er *EventReader) Next() (Event, error) {
	var (
		data    []byte
		hasData bool
		ev      Event
	)
	for er.sc.Scan() {
		line := er.sc.Bytes()
		if !er.started {
			er.started = true
			line = bytes.TrimPrefix(line, bom)
		}
		if len(line) == 0 {
			// 空行分隔事件；没有 data 的事件按规范不派发，但其中的 id 仍然生效。
			if !hasData {
				ev = Event{}
				continue
			}
			ev.ID = er.lastID
			if ev.Type == "" {
				ev.Type = "message"
			}
			ev.Data = data
			return ev, nil
		}
		if line[0] == ':' {
			continue // 注释，常用作心跳
		}
		field, value, _ := bytes.Cut(line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "data":
			if hasData {
				data = append(data, '\n')
			}
			if len(data)+len(value) > er.max {
				return Event{}, ErrEventTooLarge
			}
			data = append(data, value...)
			hasData = true
		case "event":
			ev.Type = string(value)
		case "id":
			if !bytes.ContainsRune(value, 0) {
				er.lastID = string(value)
			}
		case "retry":
			if ms, perr := strconv.ParseUint(string(value), 10, 63); perr == nil {
				ev.Retry = time.Duration(ms) * time.Millisecond
			}
		}
	}
	switch err := er.sc.Err(); {
	case err == nil:
		return Event{}, io.EOF
	case errors.Is(err, bufio.ErrTooLong):
		return Event{}, ErrEventTooLarge
	default:
		return Event{}, err
	}
}

var bom = []byte("\xef\xbb\xbf")

// scanLines 按 SSE 规范切行：\r\n、\n、\r 都是行尾。
func scanLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		if data[i] == '\n' {
			return i + 1, data[:i], nil
		}
		if i+1 < len(data) {
			if data[i+1] == '\n' {
				return i + 2, data[:i], nil
			}
			return i + 1, data[:i], nil
		}
		// \r 恰好在缓冲区末尾：需要看到下一个字节才能区分 \r 与 \r\n。
		if atEOF {
			return i + 1, data[:i], nil
		}
		return 0, nil, nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// Events 以 text/event-stream 发起请求，并以迭代器逐条返回事件。
//
//	for ev, err := range c.Post("/chat/completions").JSON(req).Events(ctx) {
//		if err != nil {
//			return err
//		}
//		if string(ev.Data) == "[DONE]" {
//			break
//		}
//		...
//	}
//
// 语义：
//   - 连接建立前与 Do 完全一致：同样的重试、ErrStatus 与业务错误解码；
//     默认补齐 Accept: text/event-stream 与 Cache-Control: no-cache。
//   - 收到被接受的响应后，单次尝试超时（WithTimeout）不再生效，改由空闲超时（WithIdleTimeout）
//     约束：连续这么久没有收到任何字节（包括心跳注释）即以 ErrTimeout 结束。总时长由 ctx 控制。
//   - 连接建立后不会自动重连：LLM 流不可续传，断流应由调用方决定是否重新发起。
//     需要续传时可读取最后一条 Event.ID，用 Header("Last-Event-ID", id) 重新发起。
//   - 迭代中途 break 或 ctx 取消都会立即关闭连接。产生 error 后迭代结束。
//   - 单条事件或单行超过 WithMaxEventSize（默认 1MiB）时以 ErrDecode 结束，防止异常流耗尽内存。
func (r *Request) Events(ctx context.Context) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		resp, err := r.do(ctx, true)
		if err != nil {
			yield(Event{}, err)
			return
		}
		defer resp.Body.Close()
		if ct := resp.Header.Get("Content-Type"); !isEventStream(ct) && resp.StatusCode != http.StatusNoContent {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, decodeBodyLimit))
			e := r.newError(ErrDecode, nil, resp, errors.New("unexpected Content-Type "+strconv.Quote(ct)), attemptOf(resp))
			e.Body = body
			yield(Event{}, e)
			return
		}
		er := NewEventReader(resp.Body, r.client.maxEventSize)
		for {
			ev, err := er.Next()
			if err == io.EOF {
				return
			}
			if err != nil {
				yield(Event{}, r.streamError(ctx, resp, err))
				return
			}
			if !yield(ev, nil) {
				return
			}
		}
	}
}

// streamError 对流读取阶段的失败进行分类：调用方取消、超时、超限或连接中断。
func (r *Request) streamError(ctx context.Context, resp *http.Response, err error) error {
	kind := transportKind(err)
	switch {
	case ctx.Err() != nil:
		kind, err = ctxKind(ctx.Err()), ctx.Err()
	case errors.Is(err, ErrEventTooLarge):
		kind = ErrDecode
	}
	return r.newError(kind, nil, resp, err, attemptOf(resp))
}

func isEventStream(contentType string) bool {
	mt, _, _ := strings.Cut(contentType, ";")
	return strings.EqualFold(strings.TrimSpace(mt), "text/event-stream")
}

// EventsOf 把每条事件的 Data 用 Client 的 Codec 解码为 T，是 OpenAI / Anthropic 等 JSON 流的便捷形式。
// skip 返回 true 的事件会被跳过（例如 OpenAI 的 "[DONE]" 或 Anthropic 的 ping），可以为 nil。
//
// 通常直接使用方法形式 [Request.EventsOf]；该函数用于自定义的事件源（例如经过过滤的迭代器）。
func EventsOf[T any](events iter.Seq2[Event, error], codec Codec, skip func(Event) bool) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		for ev, err := range events {
			var v T
			if err != nil {
				yield(v, err)
				return
			}
			if skip != nil && skip(ev) {
				continue
			}
			if err := codec.Decode(bytes.NewReader(ev.Data), &v); err != nil {
				yield(v, &Error{Kind: ErrDecode, Body: bytes.Clone(ev.Data), Err: err})
				return
			}
			if !yield(v, nil) {
				return
			}
		}
	}
}

// EventsOf 是 Events + 按 Client Codec 解码的便捷方法（Go 1.27 泛型方法）。
//
//	for chunk, err := range c.Post("/chat/completions").JSON(req).EventsOf[ChatChunk](ctx, fluent.SkipData("[DONE]")) {
func (r *Request) EventsOf[T any](ctx context.Context, skip func(Event) bool) iter.Seq2[T, error] {
	return EventsOf[T](r.Events(ctx), r.client.codec, skip)
}

// SkipData 返回一个 skip 函数，跳过 Data 恰好等于任一给定值的事件（如 OpenAI 的 "[DONE]"）。
func SkipData(values ...string) func(Event) bool {
	return func(ev Event) bool {
		return slices.Contains(values, string(ev.Data))
	}
}

// SkipType 返回一个 skip 函数，跳过指定 event 类型（如 Anthropic 的 "ping"）。
func SkipType(types ...string) func(Event) bool {
	return func(ev Event) bool {
		return slices.Contains(types, ev.Type)
	}
}
