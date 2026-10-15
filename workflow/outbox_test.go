package workflow

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestOutboxDeliveryIsAtLeastOnceUntilAcked(t *testing.T) {
	infrastructure := NewDebugInfrastructure()
	ctx := context.Background()

	signal := &WaitForSignalEvent{
		Key:       "approval|callback",
		Status:    FlowStatusSuspended,
		StartedAt: time.Now(),
		Request:   json.RawMessage(`"review"`),
	}
	if err := infrastructure.Suspend(ctx, signal); err != nil {
		t.Fatalf("suspend failed: %v", err)
	}

	first, err := infrastructure.PendingOutbox(ctx, time.Now())
	if err != nil {
		t.Fatalf("pending outbox failed: %v", err)
	}
	if len(first) != 1 || first[0].Key != "approval|callback" {
		t.Fatalf("expected one pending message, got %+v", first)
	}

	second, err := infrastructure.PendingOutbox(ctx, time.Now())
	if err != nil {
		t.Fatalf("pending outbox failed: %v", err)
	}
	if len(second) != 1 {
		t.Fatalf("expected message to stay pending until acked, got %+v", second)
	}

	// 返回值必须是深拷贝：改动它不应影响存储。
	first[0].Key = "mutated"
	third, err := infrastructure.PendingOutbox(ctx, time.Now())
	if err != nil {
		t.Fatalf("pending outbox failed: %v", err)
	}
	if len(third) != 1 || third[0].Key != "approval|callback" {
		t.Fatalf("expected stored message to be unaffected by caller mutation, got %+v", third)
	}

	acked, err := infrastructure.AckOutbox(ctx, "approval|callback")
	if err != nil || !acked {
		t.Fatalf("expected ack to succeed, got acked=%t err=%v", acked, err)
	}
	remaining, err := infrastructure.PendingOutbox(ctx, time.Now())
	if err != nil {
		t.Fatalf("pending outbox failed: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("expected acked message to disappear, got %+v", remaining)
	}

	if ackedAgain, err := infrastructure.AckOutbox(ctx, "approval|callback"); err != nil || !ackedAgain {
		t.Fatalf("expected repeated ack to be idempotent, got acked=%t err=%v", ackedAgain, err)
	}
	if missing, err := infrastructure.AckOutbox(ctx, "missing"); err != nil || missing {
		t.Fatalf("expected unknown key ack to return false, got acked=%t err=%v", missing, err)
	}
}

func TestOutboxHonoursAvailabilityDelay(t *testing.T) {
	infrastructure := NewDebugInfrastructure()
	ctx := context.Background()
	until := time.Now().Add(time.Hour)

	sleep := &SleepSignalEvent{
		Key:       "poll|sleep|1",
		Status:    FlowStatusSuspended,
		StartedAt: time.Now(),
		Until:     until,
	}
	if err := infrastructure.Suspend(ctx, sleep); err != nil {
		t.Fatalf("suspend failed: %v", err)
	}

	pending, err := infrastructure.PendingOutbox(ctx, time.Now())
	if err != nil {
		t.Fatalf("pending outbox failed: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("expected sleep message to be invisible before Until, got %+v", pending)
	}

	pending, err = infrastructure.PendingOutbox(ctx, until.Add(time.Second))
	if err != nil {
		t.Fatalf("pending outbox failed: %v", err)
	}
	if len(pending) != 1 || !pending[0].AvailableAt.Equal(until) {
		t.Fatalf("expected sleep message visible at Until, got %+v", pending)
	}
}

func TestSuspendIsIdempotentByKey(t *testing.T) {
	infrastructure := NewDebugInfrastructure()
	ctx := context.Background()

	first := &WaitForSignalEvent{
		Key:       "approval|callback",
		Status:    FlowStatusSuspended,
		StartedAt: time.Now(),
		Request:   json.RawMessage(`"one"`),
	}
	if err := infrastructure.Suspend(ctx, first); err != nil {
		t.Fatalf("suspend failed: %v", err)
	}

	second := &WaitForSignalEvent{
		Key:       "approval|callback",
		Status:    FlowStatusSuspended,
		StartedAt: time.Now(),
		Request:   json.RawMessage(`"two"`),
	}
	if err := infrastructure.Suspend(ctx, second); err != nil {
		t.Fatalf("repeat suspend failed: %v", err)
	}

	stored, err := infrastructure.GetEvent(ctx, "approval|callback")
	if err != nil {
		t.Fatalf("get event failed: %v", err)
	}
	wait, ok := stored.(*WaitForSignalEvent)
	if !ok {
		t.Fatalf("expected wait event, got %T", stored)
	}
	if string(wait.Request) != `"one"` {
		t.Fatalf("expected existing signal to be preserved, got %s", wait.Request)
	}

	messages, err := infrastructure.PendingOutbox(ctx, time.Now())
	if err != nil {
		t.Fatalf("pending outbox failed: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("expected a single idempotent outbox message, got %+v", messages)
	}

	conflict := &SleepSignalEvent{
		Key:       "approval|callback",
		Status:    FlowStatusSuspended,
		StartedAt: time.Now(),
		Until:     time.Now(),
	}
	if err := infrastructure.Suspend(ctx, conflict); err == nil {
		t.Fatal("expected type conflict on reused key")
	}
}
