package fluent

import (
	"io"

	json "encoding/json/v2"
)

// Codec 负责请求体编码与响应体解码。
type Codec interface {
	ContentType() string
	Marshal(v any) ([]byte, error)
	// Decode 从流中解码，不要求先把整个响应体读入内存。
	Decode(r io.Reader, v any) error
}

type jsonCodec struct {
	opts json.Options
}

// JSON 返回标准库 JSON 编解码器，opts 会透传给 Marshal/UnmarshalRead。
// 不传 opts 时使用严格语义（字段名大小写敏感、拒绝重复键、nil slice 编码为 []）。
func JSON(opts ...json.Options) Codec {
	return jsonCodec{opts: json.JoinOptions(opts...)}
}

// DefaultCodec 是默认 JSON 编解码器。
var DefaultCodec = JSON()

func (jsonCodec) ContentType() string { return "application/json" }

func (c jsonCodec) Marshal(v any) ([]byte, error) { return json.Marshal(v, c.opts) }

func (c jsonCodec) Decode(r io.Reader, v any) error { return json.UnmarshalRead(r, v, c.opts) }
