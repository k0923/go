package dflow

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"time"

	"github.com/dop251/goja"
)

type Script struct {
	Language string `json:"language,omitempty"`
	Content  string `json:"content"`
}

type CodeConfig struct {
	Script    Script `json:"script"`
	TimeoutMS int64  `json:"timeoutMs,omitempty"`
}

type CodeNode struct {
	program *goja.Program
	timeout time.Duration
}

// NewCodeNode compiles JavaScript without executing it. Each Run gets a fresh VM.
// Scripts define main({params}); legacy run(params) is also accepted.
// No filesystem, network, require, or event loop is installed.
func NewCodeNode(config CodeConfig) (*CodeNode, error) {
	if config.Script.Language != "" && config.Script.Language != "javascript" {
		return nil, fmt.Errorf("unsupported script language %q", config.Script.Language)
	}
	if strings.TrimSpace(config.Script.Content) == "" {
		return nil, fmt.Errorf("script is empty")
	}
	if config.TimeoutMS == 0 {
		config.TimeoutMS = 10_000
	}
	if config.TimeoutMS < 1 || config.TimeoutMS > math.MaxInt64/int64(time.Millisecond) {
		return nil, fmt.Errorf("invalid code timeout")
	}
	program, err := goja.Compile("dflow-code.js", `(function() {
"use strict";
`+config.Script.Content+`
if (typeof main === "function") return main;
if (typeof run === "function") return function(ctx) { return run(ctx.params); };
throw new Error("script must define main({ params })");
})()`, true)
	if err != nil {
		return nil, err
	}
	return &CodeNode{program: program, timeout: time.Duration(config.TimeoutMS) * time.Millisecond}, nil
}

func (n *CodeNode) Validate() error {
	if n == nil || n.program == nil || n.timeout <= 0 {
		return fmt.Errorf("code: construct with NewCodeNode")
	}
	return nil
}

func (n *CodeNode) Run(parent context.Context, input map[string]any) (Result, error) {
	if err := n.Validate(); err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithTimeout(parent, n.timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	vm := goja.New()
	vm.SetMaxCallStackSize(1000)
	stop := context.AfterFunc(ctx, func() { vm.Interrupt(ctx.Err()) })
	defer stop()
	// Capture the intrinsic before user code can replace JSON.stringify.
	stringifier, err := vm.RunString(`(function(stringify) {
		return function(value) { return stringify(value); };
	})(JSON.stringify)`)
	if err != nil {
		return Result{}, err
	}
	stringify, _ := goja.AssertFunction(stringifier)
	parse, _ := goja.AssertFunction(vm.Get("JSON").ToObject(vm).Get("parse"))
	toString, _ := goja.AssertFunction(vm.Get("String"))
	if err := vm.Set("sleep", func(call goja.FunctionCall) goja.Value {
		ms := call.Argument(0).ToFloat()
		if math.IsNaN(ms) || ms <= 0 {
			return goja.Undefined()
		}
		deadline, _ := ctx.Deadline()
		remaining := time.Until(deadline)
		duration := remaining
		if ms < float64(remaining)/float64(time.Millisecond) {
			duration = time.Duration(ms * float64(time.Millisecond))
		}
		timer := time.NewTimer(duration)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			vm.Interrupt(ctx.Err())
		case <-timer.C:
			if ctx.Err() != nil {
				vm.Interrupt(ctx.Err())
			}
		}
		return goja.Undefined()
	}); err != nil {
		return Result{}, err
	}
	failure := func(err error) (Result, error) {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return Result{}, err
	}
	entry, err := vm.RunProgram(n.program)
	if err != nil {
		return failure(err)
	}
	main, ok := goja.AssertFunction(entry)
	if !ok {
		return failure(fmt.Errorf("script must define main({ params })"))
	}
	// Marshal into a native JS object rather than exposing mutable Go maps.
	rawInput, err := json.Marshal(input)
	if err != nil {
		return failure(err)
	}
	nativeInput, err := parse(goja.Undefined(), vm.ToValue(string(rawInput)))
	if err != nil {
		return failure(err)
	}
	argument := vm.NewObject()
	if err := argument.Set("params", nativeInput); err != nil {
		return failure(err)
	}
	value, err := main(goja.Undefined(), argument)
	if err != nil {
		return failure(err)
	}
	if value.ExportType() == reflect.TypeFor[*goja.Promise]() {
		promise := value.Export().(*goja.Promise)
		switch promise.State() {
		case goja.PromiseStateFulfilled:
			value = promise.Result()
		case goja.PromiseStateRejected:
			// ToString may invoke user code, so keep it under the same VM deadline.
			reason, reasonErr := toString(goja.Undefined(), promise.Result())
			if reasonErr != nil {
				return failure(reasonErr)
			}
			return failure(fmt.Errorf("script promise rejected: %s", reason.String()))
		default:
			return failure(fmt.Errorf("script returned a pending promise; no event loop is installed"))
		}
	}
	// Stringification stays inside the interruptible VM (including custom toJSON).
	serialized, err := stringify(goja.Undefined(), value)
	if err != nil {
		return failure(fmt.Errorf("script output must be JSON: %w", err))
	}
	if goja.IsUndefined(serialized) {
		return failure(fmt.Errorf("script output must be JSON, got undefined"))
	}
	var output any
	if err := json.Unmarshal([]byte(serialized.String()), &output); err != nil {
		return failure(err)
	}
	if ctx.Err() != nil {
		return failure(ctx.Err())
	}
	return Result{Output: output}, nil
}
