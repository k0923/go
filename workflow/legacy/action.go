package workflow

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/google/uuid"
)

type ActionRunOption struct{}

type Action[Input any, Output any] interface {
	Run(ctx *ActionContext, input Input) Output
}

type ActionContext struct {
	context.Context
	key       string
	namespace []string
	api       Infrastructure
	events    map[string]Event
	lock      *sync.Mutex
}

func (ctx ActionContext) GetKey() string {
	return ctx.key
}

func (ctx ActionContext) apiKey(key string) string {
	if ctx.key == "" {
		return key
	}
	return fmt.Sprintf("%s|%s", ctx.key, key)
}

func (ctx ActionContext) WaitFor[Request any, Response any](
	key string,
	option WaitForOption[Request],
) Response {
	ensureContext(ctx.Context, nil)
	return ctx.waitFor[Request, Response](ctx.apiKey(key), option)
}

func (ctx ActionContext) Sleep(key string, duration time.Duration) {
	ensureContext(ctx.Context, nil)
	ctx.sleep(ctx.apiKey(key), duration)
}

func (ctx ActionContext) SleepUntil(key string, until time.Time) {
	ensureContext(ctx.Context, nil)
	ctx.sleepUntil(ctx.apiKey(key), until)
}

func (ctx ActionContext) Now(key string) time.Time {
	ensureContext(ctx.Context, nil)
	return ctx.now(ctx.apiKey(key))
}

func (ctx ActionContext) UUID(key string) string {
	ensureContext(ctx.Context, nil)
	return ctx.uuid(ctx.apiKey(key))
}

func newActionContext(
	ctx context.Context,
	key string,
	api Infrastructure,
) *ActionContext {
	return &ActionContext{
		Context: ctx,
		key:     key,
		api:     api,
		events:  make(map[string]Event),
		lock:    &sync.Mutex{},
	}
}

func (ctx ActionContext) waitFor[Request any, Response any](
	key string,
	option WaitForOption[Request],
) Response {
	waitForSignalEvent := ctx.getEvent[*WaitForSignalEvent](key)
	if waitForSignalEvent != nil {
		switch waitForSignalEvent.Status {
		case FlowStatusCompleted:
			return ctx.unmarshal[Response](waitForSignalEvent.Response, waitForSignalEvent)
		case FlowStatusFailed:
			panic(NewLegacyBizError(waitForSignalEvent, fmt.Errorf("wait for signal %s failed", key)))
		case FlowStatusRunning, FlowStatusSuspended:
			// 信号尚未被外部解决：重新以挂起信号结束本次驱动，由 Lease 在同一事务内
			// 幂等重写信号并释放租约。绝不能以 ConflictError 结束，否则会被上层
			// 持久化为 FAILED，导致重复驱动永久打坏流程。
			panic(waitForSignalEvent)
		default:
			panic(NewSystemError(waitForSignalEvent, fmt.Errorf("unknown status %s", waitForSignalEvent.Status)))
		}
	}
	waitForSignalEvent = &WaitForSignalEvent{
		Key:       key,
		Status:    FlowStatusSuspended,
		StartedAt: time.Now(),
		Version:   0,
	}
	waitForSignalEvent.Request = ctx.marshal(option.Request, waitForSignalEvent)
	// 信号不在挂起点落库，由 Lease 在挂起提交事务内持久化，
	// 保证信号对外可见不早于执行单元可恢复。
	panic(waitForSignalEvent)
}

func (ctx ActionContext) now(key string) time.Time {
	return ctx.checkpoint(key, func(now time.Time) time.Time {
		return now
	})
}

func (ctx ActionContext) uuid(key string) string {
	return ctx.checkpoint(key, func(time.Time) string {
		return uuid.NewString()
	})
}

func (ctx ActionContext) checkpoint[T any](key string, generate func(time.Time) T) T {
	checkpointEvent := ctx.getEvent[*CheckpointEvent](key)
	if checkpointEvent != nil {
		return ctx.unmarshal[T](checkpointEvent.GetResult(), checkpointEvent)
	}

	now := time.Now()
	value := generate(now)
	checkpointEvent = &CheckpointEvent{
		Key:       key,
		StartedAt: now,
		EndedAt:   now,
		Status:    FlowStatusCompleted,
		Version:   0,
	}
	checkpointEvent.SetResult(ctx.marshal(value, checkpointEvent))
	ctx.addEvent(checkpointEvent)
	return value
}

func (ctx ActionContext) sleep(key string, duration time.Duration) {
	until := time.Now().Add(duration)
	ctx.sleepUntil(key, until)
}

func (ctx ActionContext) sleepUntil(key string, until time.Time) {
	sleepEvent := ctx.getEvent[*SleepSignalEvent](key)
	if sleepEvent == nil {
		if !time.Now().Before(until) {
			// 首次执行且已到期：直接落库为 COMPLETED，重放时无需再计算。
			ctx.addEvent(&SleepSignalEvent{
				Key:       key,
				StartedAt: time.Now(),
				EndedAt:   time.Now(),
				Status:    FlowStatusCompleted,
				Version:   0,
				Until:     until,
			})
			return
		}
		// 未到期：信号不在挂起点落库，由 Lease 在挂起提交事务内持久化。
		panic(&SleepSignalEvent{
			Key:       key,
			StartedAt: time.Now(),
			Status:    FlowStatusSuspended,
			Version:   0,
			Until:     until,
		})
	}
	if time.Until(sleepEvent.Until) <= 0 {
		if sleepEvent.Status != FlowStatusCompleted {
			sleepEvent.Status = FlowStatusCompleted
			sleepEvent.EndedAt = time.Now()
			ctx.updateEvent(sleepEvent)
		}
		return
	}
	panic(sleepEvent)
}

func (ctx ActionContext) unmarshal[T any](result json.RawMessage, event Event) T {
	var t T
	if err := json.Unmarshal(result, &t); err != nil {
		panic(NewUnMarshalError(event, err))
	}
	return t
}

func (ctx ActionContext) marshal(data any, event Event) json.RawMessage {
	result, err := json.Marshal(data)
	if err != nil {
		panic(NewMarshalError(event, err))
	}
	return json.RawMessage(result)
}

func (ctx ActionContext) convertEvent[T Event](event Event) T {
	if evt, ok := event.(T); ok {
		return evt
	} else {
		panic(NewSystemError(event, fmt.Errorf("event %s is %s, not %s", event.GetKey(), event.GetEventType(), reflect.TypeOf((*T)(nil)).Elem())))
	}
}

func (ctx ActionContext) getEvent[T Event](key string) T {
	ctx.lock.Lock()
	cached, exists := ctx.events[key]
	ctx.lock.Unlock()
	if exists {
		return ctx.convertEvent[T](cached)
	}
	event, err := ctx.api.GetEvent(ctx.Context, key)
	if err != nil {
		panic(NewInfrastructureError(nil, err, InfrastructureMethodGetEvent))
	}
	if event == nil {
		var t T
		return t
	}
	return ctx.convertEvent[T](event)
}

func (ctx ActionContext) addEvent[T Event](event T) T {
	result, err := ctx.api.AddEvent(ctx.Context, event)
	if err != nil {
		panic(NewInfrastructureError(event, err, InfrastructureMethodAddEvent))
	}
	if result == nil {
		panic(NewConflictError(event))
	}
	if evt, ok := result.(T); ok {
		return evt
	} else {
		panic(NewSystemError(event, fmt.Errorf("event %s is %s, not %s", result, event.GetEventType(), reflect.TypeOf((*T)(nil)).Elem())))
	}
}

func (ctx ActionContext) updateEvent[T Event](event T) T {
	result, err := ctx.api.UpdateEvent(ctx.Context, event)
	if err != nil {
		panic(NewInfrastructureError(event, err, InfrastructureMethodUpdateEvent))
	}
	if result == nil {
		panic(NewConflictError(event))
	}
	if evt, ok := result.(T); ok {
		return evt
	} else {
		panic(NewSystemError(event, fmt.Errorf("event %s is %s, not %s", result, event.GetEventType(), reflect.TypeOf((*T)(nil)).Elem())))
	}
}
