// Package appcodec demonstrates an application-owned storage format. Production
// applications may instead use DTOs containing xjson.G interfaces.
//
// Package appcodec 演示由应用拥有的存储格式。生产应用也可以改用包含 xjson.G
// 接口的 DTO。
package appcodec

import (
	"bytes"
	"encoding/gob"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/dflow/examples/nodes"
)

type Codec struct{}

func (Codec) Encode(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := gob.NewEncoder(&buffer)
	if payload, ok := value.(dflow.Suspension); ok {
		err := encoder.Encode(&payload)
		return buffer.Bytes(), err
	}
	err := encoder.Encode(value)
	return buffer.Bytes(), err
}

func (Codec) Decode(raw []byte, target any) error {
	return gob.NewDecoder(bytes.NewReader(raw)).Decode(target)
}

func init() {
	gob.Register(map[string]any{})
	gob.Register([]any{})
	gob.Register(&nodes.EndNode{})
	gob.Register(dflow.ConstantParameter{})
	gob.Register(dflow.ReferenceParameter{})
	gob.Register(dflow.TemplateParameter{})
	gob.Register(&dflow.ValueSuspension{})
}
