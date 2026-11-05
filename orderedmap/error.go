package orderedmap

import (
	"errors"
	"fmt"
)

// ErrDuplicateKey identifies JSON objects containing a repeated key.
var ErrDuplicateKey = errors.New("orderedmap: duplicate JSON key")

// DuplicateKeyError reports the decoded key that occurred more than once.
type DuplicateKeyError struct {
	Key string
}

func (e *DuplicateKeyError) Error() string {
	return fmt.Sprintf("%s %q", ErrDuplicateKey, e.Key)
}

func (e *DuplicateKeyError) Unwrap() error {
	return ErrDuplicateKey
}
