package rule

import (
	"errors"
	"fmt"
)

// MaxDepth bounds the nesting of a rule accepted by Parse and by
// condition.Schema.Check, protecting against stack exhaustion.
const MaxDepth = 64

// Sentinel errors wrapped by *Error. Test them with errors.Is.
var (
	ErrUnknownOp    = errors.New("unknown type or opt")
	ErrUnknownField = errors.New("unknown field")
	ErrTypeMismatch = errors.New("type mismatch")
	ErrInvalidNode  = errors.New("invalid node")
	ErrTooDeep      = errors.New("nesting too deep")
)

// Error reports why a rule was rejected and where. Path is a JSONPath-like
// location such as "$.items[1].x".
type Error struct {
	Path string
	Err  error
}

func (e *Error) Error() string { return "condition: " + e.Path + ": " + e.Err.Error() }

func (e *Error) Unwrap() error { return e.Err }

// Errorf returns an *Error at path wrapping kind.
func Errorf(path string, kind error, format string, args ...any) error {
	return &Error{Path: path, Err: fmt.Errorf("%w: "+format, append([]any{kind}, args...)...)}
}
