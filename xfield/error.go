package xfield

import (
	"errors"
	"fmt"
)

var (
	ErrInvalidSchema = errors.New("invalid schema")
	ErrInvalidValue  = errors.New("invalid field value")
)

// PathError preserves the underlying error for errors.Is/As.
type PathError struct {
	Path string
	Err  error
}

func (e *PathError) Error() string {
	if e.Path == "" {
		return e.Err.Error()
	}
	return e.Path + ": " + e.Err.Error()
}

func (e *PathError) Unwrap() error { return e.Err }

func wrapPathError(path string, err error) error {
	if err == nil || path == "" {
		return err
	}
	if nested, ok := err.(*PathError); ok {
		return &PathError{Path: joinValidationPath(path, nested.Path), Err: nested.Err}
	}
	return &PathError{Path: path, Err: err}
}

func joinValidationPath(parent, child string) string {
	if parent == "" {
		return child
	}
	if child == "" {
		return parent
	}
	if child[0] == '[' {
		return parent + child
	}
	return parent + "." + child
}

func joinIndexPath(parent string, index int) string {
	return fmt.Sprintf("%s[%d]", parent, index)
}

func invalidValue(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidValue, fmt.Sprintf(format, args...))
}

func invalidSchema(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidSchema, fmt.Sprintf(format, args...))
}
