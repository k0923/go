package workflow

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"sync/atomic"
	"time"
)

var runtimeIDFallback atomic.Uint64

func newRuntimeID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err == nil {
		return hex.EncodeToString(value[:])
	}
	return fmtRuntimeIDFallback()
}

func fmtRuntimeIDFallback() string {
	sequence := runtimeIDFallback.Add(1)
	return time.Now().UTC().Format("20060102T150405.000000000") + "-" +
		strconv.FormatUint(sequence, 10)
}

type recoveredPanic struct {
	value  any
	signal workflowSignal
}

func classifyPanic(value any) recoveredPanic {
	result := recoveredPanic{value: value}
	if value == nil {
		return result
	}
	if signal, ok := value.(workflowSignal); ok {
		result.signal = signal
		return result
	}
	return result
}

func SafeGo(f func()) {
	go func() {
		defer func() {
			_ = recover()
		}()

		f()
	}()
}
