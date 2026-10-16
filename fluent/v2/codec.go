package fluent

import (
	"io"

	jsonv1 "encoding/json"
	"encoding/json/v2"
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

// JSON 返回基于 encoding/json/v2 的编解码器，opts 会透传给 Marshal/UnmarshalRead。
// 不传 opts 时使用 v2 的严格语义（字段名大小写敏感、拒绝重复键、nil slice 编码为 []）。
func JSON(opts ...json.Options) Codec {
	return jsonCodec{opts: json.JoinOptions(opts...)}
}

// DefaultCodec 使用 v1 兼容语义（字段名大小写不敏感等），从 v1 迁移时行为不会悄然改变。
// 新服务建议显式使用 WithCodec(JSON()) 以获得 v2 的严格语义。
var DefaultCodec = JSON(jsonv1.DefaultOptionsV1())

func (jsonCodec) ContentType() string { return "application/json" }

func (c jsonCodec) Marshal(v any) ([]byte, error) { return json.Marshal(v, c.opts) }

func (c jsonCodec) Decode(r io.Reader, v any) error { return json.UnmarshalRead(r, v, c.opts) }
