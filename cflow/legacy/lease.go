package cflow

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ExecutionLease 表示某个可恢复执行分支当前持有的租约。
type ExecutionLease struct {
	// Key 是执行分支的全局唯一标识。
	Key string
	// Generation 在租约被重新认领时递增，用作续租和释放时的 fencing token。
	Generation int64
	// ExpiresAt 是当前租约的过期时间。
	ExpiresAt time.Time
}

// LeaseInfrastructure 定义执行分支租约的存储能力。
type LeaseInfrastructure interface {
	// ClaimLease 尝试为 key 标识的执行分支申请有效期为 ttl 的租约。
	// 分支已被其他执行者持有时返回 (nil, nil)；存储异常通过 error 返回。
	ClaimLease(ctx context.Context, key string, ttl time.Duration) (*ExecutionLease, error)
	// RenewLease 使用 lease.Generation 进行 CAS 校验，并将租约有效期延长 ttl。
	// 租约已过期或 Generation 不匹配时返回 (nil, nil)；存储异常通过 error 返回。
	RenewLease(ctx context.Context, lease ExecutionLease, ttl time.Duration) (*ExecutionLease, error)
	// ReleaseLease 使用 lease.Generation 进行 CAS 校验并释放租约。
	// 返回值表示是否成功释放；租约不存在或 Generation 不匹配时返回 false。
	ReleaseLease(ctx context.Context, lease ExecutionLease) (bool, error)
}

// Lease 封装租约状态及其基础设施操作。
type Lease struct {
	infrastructure LeaseInfrastructure
	key            string
	ttl            time.Duration
	handler        func(context.Context)
	execution      ExecutionLease
}

func NewLease(
	infrastructure LeaseInfrastructure,
	key string,
	ttl time.Duration,
	handler func(context.Context),
) *Lease {
	return &Lease{
		infrastructure: infrastructure,
		key:            key,
		ttl:            ttl,
		handler:        handler,
	}
}

var ErrLeaseConflict = errors.New("lease conflict")

type LeaseOperation string

const (
	LeaseOperationClaim   LeaseOperation = "ClaimLease"
	LeaseOperationRenew   LeaseOperation = "RenewLease"
	LeaseOperationRelease LeaseOperation = "ReleaseLease"
)

type LeaseError struct {
	Operation LeaseOperation
	Cause     error
}

func (err *LeaseError) Error() string {
	return fmt.Sprintf("lease %s: %v", err.Operation, err.Cause)
}

func (err *LeaseError) Unwrap() error {
	return err.Cause
}

func (lease *Lease) Run(ctx context.Context) error {
	execution, err := lease.infrastructure.ClaimLease(ctx, lease.key, lease.ttl)
	if err != nil {
		return &LeaseError{Operation: LeaseOperationClaim, Cause: err}
	}
	if execution == nil {
		return ErrLeaseConflict
	}
	lease.execution = *execution

	executionContext, cancel := context.WithCancel(ctx)
	stopRenewal := make(chan struct{})
	renewalStopped := make(chan struct{})
	leaseFailure := make(chan error, 1)
	go lease.renew(executionContext, cancel, stopRenewal, renewalStopped, leaseFailure)

	var recovered any
	panicked := true
	func() {
		defer func() {
			if panicked {
				recovered = recover()
			}
		}()
		lease.handler(executionContext)
		panicked = false
	}()

	close(stopRenewal)
	<-renewalStopped

	var renewalErr error
	select {
	case renewalErr = <-leaseFailure:
	default:
	}

	released, releaseErr := lease.infrastructure.ReleaseLease(
		context.WithoutCancel(executionContext),
		lease.execution,
	)
	cancel()

	if renewalErr != nil {
		return renewalErr
	}
	if releaseErr != nil {
		return &LeaseError{Operation: LeaseOperationRelease, Cause: releaseErr}
	}
	if !released {
		return ErrLeaseConflict
	}
	if panicked {
		panic(recovered)
	}
	return nil
}

func (lease *Lease) renew(
	ctx context.Context,
	cancel context.CancelFunc,
	stop <-chan struct{},
	stopped chan<- struct{},
	failure chan<- error,
) {
	defer close(stopped)

	for {
		renewAfter := time.Until(lease.execution.ExpiresAt) / 3
		if renewAfter <= 0 {
			failure <- ErrLeaseConflict
			cancel()
			return
		}

		timer := time.NewTimer(renewAfter)
		select {
		case <-stop:
			timer.Stop()
			return
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		execution, err := lease.infrastructure.RenewLease(
			ctx,
			lease.execution,
			lease.ttl,
		)
		if err != nil {
			failure <- &LeaseError{Operation: LeaseOperationRenew, Cause: err}
			cancel()
			return
		}
		if execution == nil {
			failure <- ErrLeaseConflict
			cancel()
			return
		}
		lease.execution = *execution
	}
}
