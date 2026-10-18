package cflow

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type unsupportedEvent struct {
	BaseEvent
}

func (event *unsupportedEvent) GetEventType() string {
	return "UNSUPPORTED"
}

func TestWorkflowErrors(t *testing.T) {
	base := &StepResultEvent{BaseEvent{Key: "event"}, nil}
	cause := errors.New("cause")

	t.Run("error messages include available context", func(t *testing.T) {
		tests := []struct {
			name string
			err  error
			want string
		}{
			{"conflict", NewConflictError(base), "conflict: event"},
			{"context with event", NewContextError(base, cause), "context error: event: cause"},
			{"context without event", NewContextError(nil, cause), "context error: cause"},
			{"biz with event", NewBizError(base, cause), "biz error: event"},
			{"biz with cause", NewBizError(nil, cause), "cause"},
			{"legacy with event", NewLegacyBizError(base, cause), "legacy biz error: event"},
			{"legacy with cause", NewLegacyBizError(nil, cause), "cause"},
			{"system with event", NewSystemError(base, cause), "system error: event"},
			{"system with cause", NewSystemError(nil, cause), "cause"},
			{"infrastructure", NewInfrastructureError(base, cause, InfrastructureMethodGetEvent), "infrastructure error: GetEvent event: cause"},
			{"marshal with event", NewMarshalError(base, cause), "marshal error: event"},
			{"marshal with cause", NewMarshalError(nil, cause), "cause"},
			{"unmarshal with event", NewUnMarshalError(base, cause), "unmarshal error: event"},
			{"unmarshal with cause", NewUnMarshalError(nil, cause), "cause"},
			{"unknown", NewUnknownError("panic"), "unknown error: panic"},
			{"lease", &LeaseError{Operation: LeaseOperationRenew, Cause: cause}, "lease RenewLease: cause"},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				if got := test.err.Error(); got != test.want {
					t.Fatalf("expected %q, got %q", test.want, got)
				}
			})
		}
	})

	t.Run("nil errors return empty strings", func(t *testing.T) {
		var contextErr *ContextError
		var bizErr *BizError
		var legacyErr *LegacyBizError
		var systemErr *SystemError
		var infrastructureErr *InfrastructureError
		var marshalErr *MarshalError
		var unmarshalErr *UnMarshalError

		for name, got := range map[string]string{
			"context":        contextErr.Error(),
			"biz":            bizErr.Error(),
			"legacy":         legacyErr.Error(),
			"system":         systemErr.Error(),
			"infrastructure": infrastructureErr.Error(),
			"marshal":        marshalErr.Error(),
			"unmarshal":      unmarshalErr.Error(),
		} {
			if got != "" {
				t.Fatalf("expected empty %s error string, got %q", name, got)
			}
		}
	})

	t.Run("unwrap returns causes", func(t *testing.T) {
		if !errors.Is(NewContextError(base, cause), cause) {
			t.Fatal("context error should unwrap cause")
		}
		if !errors.Is(NewSystemError(base, cause), cause) {
			t.Fatal("system error should unwrap cause")
		}
		if !errors.Is(NewMarshalError(base, cause), cause) {
			t.Fatal("marshal error should unwrap cause")
		}
		if !errors.Is(NewUnMarshalError(base, cause), cause) {
			t.Fatal("unmarshal error should unwrap cause")
		}
		if !errors.Is((&LeaseError{Operation: LeaseOperationClaim, Cause: cause}), cause) {
			t.Fatal("lease error should unwrap cause")
		}
	})
}

func TestWorkflowEvents(t *testing.T) {
	t.Run("base event accessors and status timestamps", func(t *testing.T) {
		event := &BaseEvent{Key: "base", StartedAt: time.Now(), Version: 7}
		event.SetError(errors.New("failed"))

		if event.GetKey() != "base" || event.GetVersion() != 7 || event.GetError() != "failed" {
			t.Fatalf("unexpected base event fields: %+v", event)
		}
		if event.GetStartedAt().IsZero() || event.GetEndedAt().IsZero() {
			t.Fatalf("expected started and ended timestamps, got %+v", event)
		}
		event.SetStatus(FlowStatusRunning)
		if !event.GetEndedAt().IsZero() {
			t.Fatalf("expected running event to clear ended at, got %+v", event)
		}
	})

	t.Run("result event type accessors", func(t *testing.T) {
		checkpoint := &CheckpointEvent{}
		checkpoint.SetResult([]byte(`"checkpoint"`))
		loop := &LoopEvent{}
		loop.SetResult([]byte(`"loop"`))

		tests := []struct {
			name      string
			eventType string
			result    string
		}{
			{(&StepResultEvent{}).GetEventType(), "STEP_RESULT", string((&StepResultEvent{Result: []byte(`"step"`)}).GetResult())},
			{checkpoint.GetEventType(), "CHECKPOINT", string(checkpoint.GetResult())},
			{(&SleepSignalEvent{}).GetEventType(), "SLEEP", ""},
			{loop.GetEventType(), "LOOP", string(loop.GetResult())},
			{(&WaitForSignalEvent{}).GetEventType(), "WAIT_FOR_SIGNAL", ""},
		}
		for _, test := range tests {
			if test.name != test.eventType {
				t.Fatalf("expected event type %q, got %q", test.eventType, test.name)
			}
			if test.result != "" && !strings.Contains(test.result, strings.ToLower(strings.Trim(test.eventType, "_RESULT"))) {
				t.Fatalf("unexpected result payload %q for %s", test.result, test.eventType)
			}
		}
	})

	t.Run("wait signal resolves matching type", func(t *testing.T) {
		target := &WaitForSignalEvent{}
		source := &WaitForSignalEvent{Response: []byte(`"ok"`)}
		if err := target.ResolveSignal(source); err != nil {
			t.Fatalf("resolve wait signal failed: %v", err)
		}
		if target.GetStatus() != FlowStatusCompleted || string(target.Response) != `"ok"` || target.GetEndedAt().IsZero() {
			t.Fatalf("unexpected resolved wait signal: %+v", target)
		}
		if err := target.ResolveSignal(&SleepSignalEvent{}); err == nil {
			t.Fatal("expected unsupported wait signal type error")
		}
	})

	t.Run("sleep signal resolves only when due and mutable", func(t *testing.T) {
		sleep := &SleepSignalEvent{BaseEvent{Key: "sleep", Status: FlowStatusSuspended}, time.Now().Add(time.Hour)}
		if err := sleep.ResolveSignal(&WaitForSignalEvent{}); err == nil {
			t.Fatal("expected unsupported sleep signal type error")
		}
		if err := sleep.ResolveSignal(&SleepSignalEvent{}); err == nil {
			t.Fatal("expected early sleep signal conflict")
		}

		sleep.Until = time.Now().Add(-time.Millisecond)
		if err := sleep.ResolveSignal(&SleepSignalEvent{}); err != nil {
			t.Fatalf("resolve due sleep failed: %v", err)
		}
		if sleep.GetStatus() != FlowStatusCompleted {
			t.Fatalf("expected sleep completed, got %+v", sleep)
		}
		if err := sleep.ResolveSignal(&SleepSignalEvent{}); err == nil {
			t.Fatal("expected completed sleep signal conflict")
		}
	})
}

func TestDebugInfrastructureBoundaries(t *testing.T) {
	t.Run("context cancellation is propagated", func(t *testing.T) {
		infrastructure := NewDebugInfrastructure()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if _, err := infrastructure.ClaimLease(ctx, "lease", time.Second); !errors.Is(err, context.Canceled) {
			t.Fatalf("expected claim context canceled, got %v", err)
		}
		if _, err := infrastructure.AddEvent(ctx, &StepResultEvent{}); !errors.Is(err, context.Canceled) {
			t.Fatalf("expected add context canceled, got %v", err)
		}
		if _, err := infrastructure.GetEvent(ctx, "missing"); !errors.Is(err, context.Canceled) {
			t.Fatalf("expected get context canceled, got %v", err)
		}
		if _, err := infrastructure.UpdateEvent(ctx, &StepResultEvent{}); !errors.Is(err, context.Canceled) {
			t.Fatalf("expected update context canceled, got %v", err)
		}
		if _, err := infrastructure.RenewLease(ctx, ExecutionLease{}, time.Second); !errors.Is(err, context.Canceled) {
			t.Fatalf("expected renew context canceled, got %v", err)
		}
		if _, err := infrastructure.ReleaseLease(ctx, ExecutionLease{}); !errors.Is(err, context.Canceled) {
			t.Fatalf("expected release context canceled, got %v", err)
		}
	})

	t.Run("invalid lease ttl and unsupported event types fail", func(t *testing.T) {
		infrastructure := NewDebugInfrastructure()
		ctx := context.Background()

		if _, err := infrastructure.ClaimLease(ctx, "lease", 0); err == nil {
			t.Fatal("expected invalid claim ttl error")
		}
		if _, err := infrastructure.RenewLease(ctx, ExecutionLease{Key: "lease"}, 0); err == nil {
			t.Fatal("expected invalid renew ttl error")
		}
		if _, err := infrastructure.AddEvent(ctx, &unsupportedEvent{}); err == nil {
			t.Fatal("expected unsupported add event error")
		}
		if _, err := infrastructure.UpdateEvent(ctx, &unsupportedEvent{BaseEvent{Key: "unsupported", Version: 1}}); err != nil {
			t.Fatalf("missing event should conflict before clone, got %v", err)
		}
		if _, err := infrastructure.AddEvent(ctx, &StepResultEvent{BaseEvent{Key: "step"}, nil}); err != nil {
			t.Fatalf("add step failed: %v", err)
		}
		if _, err := infrastructure.UpdateEvent(ctx, &unsupportedEvent{BaseEvent{Key: "step", Version: 1}}); err == nil {
			t.Fatal("expected unsupported update event error")
		}
	})
}
