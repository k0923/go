package condition

import (
	"errors"
	"fmt"
)

// ErrDivideByZero is returned by Match/Eval when a divisor evaluates to zero,
// and by Compile when a divisor is the literal zero.
var ErrDivideByZero = errors.New("condition: division by zero")

// Sentinel errors wrapped by *CompileError. Test them with errors.Is.
var (
	ErrUnknownOp    = errors.New("unknown operator")
	ErrUnknownField = errors.New("unknown field")
	ErrTypeMismatch = errors.New("type mismatch")
	ErrArity        = errors.New("wrong number of arguments")
	ErrInvalidNode  = errors.New("invalid node")
	ErrTooDeep      = errors.New("nesting too deep")
)

// CompileError reports why a Node could not be compiled and where.
// Path is a JSONPath-like location such as "$.args[1].args[0]".
type CompileError struct {
	Path string
	Err  error
}

func (e *CompileError) Error() string { return "condition: " + e.Path + ": " + e.Err.Error() }

func (e *CompileError) Unwrap() error { return e.Err }

func errAt(path string, kind error, format string, args ...any) error {
	return &CompileError{Path: path, Err: fmt.Errorf("%w: "+format, append([]any{kind}, args...)...)}
}
