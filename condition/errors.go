package condition

import (
	"errors"

	"github.com/k0923/go/condition/rule"
)

// ErrDivideByZero is returned by Match/Eval when a divisor evaluates to zero,
// and by Check when a divisor is the literal zero.
var ErrDivideByZero = errors.New("condition: division by zero")

// CompileError reports why a rule could not be parsed or checked and where.
// Path is a JSONPath-like location such as "$.items[1].x".
type CompileError = rule.Error

// Sentinel errors wrapped by *CompileError. Test them with errors.Is.
var (
	ErrUnknownOp    = rule.ErrUnknownOp
	ErrUnknownField = rule.ErrUnknownField
	ErrTypeMismatch = rule.ErrTypeMismatch
	ErrInvalidNode  = rule.ErrInvalidNode
	ErrTooDeep      = rule.ErrTooDeep
)
