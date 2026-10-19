// Package appcodec demonstrates an application-owned storage format.
// Production applications may instead use DTOs containing xjson.G interfaces.
package appcodec

import (
	"bytes"
	"encoding/gob"

	"github.com/k0923/go/dflow"
	"github.com/k0923/go/dflow/childflow"
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
	gob.Register(&dflow.EndNode{})
	gob.Register(&childflow.LoopNode{})
	gob.Register(dflow.ConstantParameter{})
	gob.Register(dflow.ReferenceParameter{})
	gob.Register(dflow.TemplateParameter{})
	gob.Register(&dflow.ValueSuspension{})
	gob.Register(&childflow.ChildSuspension{})
	gob.Register(&childflow.LoopSuspension{})
	gob.Register(childflow.ChildResponse{})
}
