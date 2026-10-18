package dflow

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Suspension is one complete pending operation: dispatch and resume use the
// same typed payload. MarshalJSON returns payload data, not the type envelope.
type Suspension interface {
	Type() string
	MarshalJSON() ([]byte, error)
}

type SuspensionEnvelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

func EncodeSuspension(value Suspension) (SuspensionEnvelope, error) {
	if isNil(value) || value.Type() == "" {
		return SuspensionEnvelope{}, ErrInvalidSuspension
	}
	raw, err := value.MarshalJSON()
	if err != nil {
		return SuspensionEnvelope{}, err
	}
	if !json.Valid(raw) {
		return SuspensionEnvelope{}, ErrInvalidSuspension
	}
	return SuspensionEnvelope{Type: value.Type(), Data: append(json.RawMessage(nil), raw...)}, nil
}

type SuspensionFactory func() Suspension

// RegisterSuspension follows node registration: discover a stable Type using a
// factory, reject duplicates, and create fresh instances for each decode.
func (r *Registry) RegisterSuspension(factory SuspensionFactory) error {
	if factory == nil {
		return ErrInvalidSuspension
	}
	value := factory()
	if isNil(value) || value.Type() == "" {
		return ErrInvalidSuspension
	}
	kind := value.Type()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.suspensions == nil {
		r.suspensions = map[string]SuspensionFactory{}
	}
	if _, exists := r.suspensions[kind]; exists {
		return fmt.Errorf("%w: suspension %q already registered", ErrInvalidSuspension, kind)
	}
	r.suspensions[kind] = factory
	return nil
}

func (r *Registry) DecodeSuspension(envelope SuspensionEnvelope) (Suspension, error) {
	if !json.Valid(envelope.Data) {
		return nil, ErrInvalidSuspension
	}
	r.mu.RLock()
	factory := r.suspensions[envelope.Type]
	r.mu.RUnlock()
	if factory == nil {
		return nil, fmt.Errorf("%w: unknown suspension %q", ErrInvalidSuspension, envelope.Type)
	}
	value := factory()
	if isNil(value) || value.Type() != envelope.Type {
		return nil, ErrInvalidSuspension
	}
	if err := decodeConfig(envelope.Data, value); err != nil {
		return nil, err
	}
	if value.Type() != envelope.Type {
		return nil, ErrInvalidSuspension
	}
	return value, nil
}

func (r *Registry) SuspensionTypes() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	types := make([]string, 0, len(r.suspensions))
	for kind := range r.suspensions {
		types = append(types, kind)
	}
	sort.Strings(types)
	return types
}

// JSONSuspension is an explicit generic external-operation payload. Applications
// can instead register domain types (approval, upload, etc.) implementing Suspension.
type JSONSuspension struct {
	Data any `json:"data"`
}

func (*JSONSuspension) Type() string { return "dflow.json" }
func (n *JSONSuspension) MarshalJSON() ([]byte, error) {
	type plain JSONSuspension
	return json.Marshal((*plain)(n))
}
