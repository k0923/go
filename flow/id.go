package flow

import (
	"strconv"
	"sync/atomic"
	"time"
)

type IDGenerator struct {
	prefix   string
	sequence atomic.Uint64
}

func NewIDGenerator(prefix string) *IDGenerator {
	generator := &IDGenerator{
		prefix: prefix,
	}
	generator.sequence.Store(uint64(time.Now().UnixMicro()))
	return generator
}

func (generator *IDGenerator) Next() string {
	sequence := generator.sequence.Add(1)
	return generator.prefix + strconv.FormatUint(sequence, 10)
}
