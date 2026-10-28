// Package sqlite provides a durable reference Store. Business serialization is
// explicitly owned by this adapter and its caller, never by the tflow core.
package sqlite

import (
	"encoding/json"
	"fmt"

	"github.com/k0923/go/tflow"
)

// Codec encodes MainTask, SubTask, Event, PlanningPolicy and MainDecider.
// Payloads should carry a type identifier and schema version. Decode reconstructs
// dependencies as needed, including handlers on historical MainTask snapshots.
// Functions must be pure, concurrency-safe and never call back into the Store.
type Codec interface {
	Encode(any) ([]byte, error)
	Decode([]byte) (any, error)
}

type mainDTO struct {
	Task        []byte
	Policy      []byte
	Decider     []byte
	Concurrency int64
	State       tflow.MainState
	// Summary is only persisted in historical command snapshots.
	Summary tflow.SubSummary
}

type subDTO struct {
	Task  []byte
	State tflow.SubState
}

type commandDTO struct {
	ID, MainID, EventKey string
	Kind                 tflow.CommandKind
	Main                 *mainDTO
	Sub                  *subDTO
	Event                []byte
}

func (r *Repository) mainDTO(m tflow.MainRecord) (mainDTO, error) {
	dto := mainDTO{Concurrency: m.ConcurrencyLimit, State: m.MainState, Summary: m.Subs}
	var err error
	if dto.Task, err = r.codec.Encode(m.Task); err != nil {
		return dto, fmt.Errorf("encode main task: %w", err)
	}
	if dto.Policy, err = r.codec.Encode(m.PlanningPolicy); err != nil {
		return dto, fmt.Errorf("encode planning policy: %w", err)
	}
	if dto.Decider, err = r.codec.Encode(m.MainDecider); err != nil {
		return dto, fmt.Errorf("encode main decider: %w", err)
	}
	return dto, nil
}

func decodeAs[T any](codec Codec, data []byte) (T, error) {
	var zero T
	value, err := codec.Decode(data)
	if err != nil {
		return zero, err
	}
	result, ok := value.(T)
	if !ok || tflow.IsNil(result) {
		return zero, fmt.Errorf("%w: codec returned unexpected type %T", tflow.ErrInvalid, value)
	}
	return result, nil
}

func (r *Repository) mainFromDTO(dto mainDTO) (tflow.MainRecord, error) {
	m := tflow.MainRecord{
		MainConfig:   tflow.MainConfig{ConcurrencyLimit: dto.Concurrency},
		MainProgress: tflow.MainProgress{MainState: dto.State, Subs: dto.Summary},
	}
	var err error
	if m.Task, err = decodeAs[tflow.MainTask](r.codec, dto.Task); err != nil {
		return m, fmt.Errorf("decode main task: %w", err)
	}
	if m.PlanningPolicy, err = decodeAs[tflow.PlanningPolicy](r.codec, dto.Policy); err != nil {
		return m, fmt.Errorf("decode planning policy: %w", err)
	}
	if m.MainDecider, err = decodeAs[tflow.MainDecider](r.codec, dto.Decider); err != nil {
		return m, fmt.Errorf("decode main decider: %w", err)
	}
	return m, nil
}

func (r *Repository) encodeMain(m tflow.MainRecord) ([]byte, error) {
	dto, err := r.mainDTO(m)
	if err != nil {
		return nil, err
	}
	dto.Summary = tflow.SubSummary{}
	return json.Marshal(dto)
}

func (r *Repository) decodeMain(data []byte) (tflow.MainRecord, error) {
	var dto mainDTO
	if err := json.Unmarshal(data, &dto); err != nil {
		return tflow.MainRecord{}, fmt.Errorf("decode main record: %w", err)
	}
	return r.mainFromDTO(dto)
}

func (r *Repository) subDTO(s tflow.SubRecord) (subDTO, error) {
	task, err := r.codec.Encode(s.Task)
	return subDTO{Task: task, State: s.SubState}, err
}

func (r *Repository) subFromDTO(dto subDTO) (tflow.SubRecord, error) {
	task, err := decodeAs[tflow.SubTask](r.codec, dto.Task)
	if err != nil {
		return tflow.SubRecord{}, fmt.Errorf("decode subtask: %w", err)
	}
	return tflow.SubRecord{Task: task, SubState: dto.State}, nil
}

func (r *Repository) encodeSub(s tflow.SubRecord) ([]byte, error) {
	dto, err := r.subDTO(s)
	if err != nil {
		return nil, err
	}
	return json.Marshal(dto)
}

func (r *Repository) decodeSub(data []byte) (tflow.SubRecord, error) {
	var dto subDTO
	if err := json.Unmarshal(data, &dto); err != nil {
		return tflow.SubRecord{}, err
	}
	return r.subFromDTO(dto)
}

func (r *Repository) encodeCommand(c tflow.Command) ([]byte, error) {
	dto := commandDTO{ID: c.ID, MainID: c.MainID, Kind: c.Kind, EventKey: c.EventKey}
	switch c.Kind {
	case tflow.MainTerminated:
		m, err := r.mainDTO(c.Main)
		if err != nil {
			return nil, err
		}
		dto.Main = &m
	case tflow.StartSub, tflow.ResumeSub, tflow.CancelSub:
		s, err := r.subDTO(c.Sub)
		if err != nil {
			return nil, err
		}
		dto.Sub = &s
	case tflow.ReconcileMain:
	default:
		return nil, tflow.ErrInvalid
	}
	if !tflow.IsNil(c.Event) {
		data, err := r.codec.Encode(c.Event)
		if err != nil {
			return nil, err
		}
		dto.Event = data
	}
	return json.Marshal(dto)
}

func (r *Repository) decodeCommand(data []byte) (tflow.Command, error) {
	var dto commandDTO
	if err := json.Unmarshal(data, &dto); err != nil {
		return tflow.Command{}, err
	}
	c := tflow.Command{ID: dto.ID, MainID: dto.MainID, Kind: dto.Kind, EventKey: dto.EventKey}
	var err error
	if dto.Main != nil {
		if c.Main, err = r.mainFromDTO(*dto.Main); err != nil {
			return c, err
		}
	}
	if dto.Sub != nil {
		if c.Sub, err = r.subFromDTO(*dto.Sub); err != nil {
			return c, err
		}
	}
	if len(dto.Event) != 0 {
		c.Event, err = decodeAs[tflow.Event](r.codec, dto.Event)
	}
	return c, err
}
