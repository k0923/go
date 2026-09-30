package workflow

import "errors"

var ErrRunStateExists = errors.New("workflow run state already exists")
var ErrRunStateNotFound = errors.New("workflow run state not found")
var ErrAwaitedSignalNotFound = errors.New("awaited workflow signal not found")
var ErrStepNameRequired = errors.New("step name is required")
var ErrDuplicateStepName = errors.New("step name must be unique")
var ErrSignalNameRequired = errors.New("signal name is required")
var ErrSleepNameRequired = errors.New("sleep name is required")
var ErrSleepThresholdNegative = errors.New("sleep in-process threshold must not be negative")
var ErrParallelNameRequired = errors.New("parallel name is required")
var ErrBranchIDRequired = errors.New("parallel branch ID is required")
var ErrDuplicateBranchID = errors.New("parallel branch ID must be unique")
var ErrBranchGroupNotFound = errors.New("parallel branch group not found")
var ErrBranchDefinitionMismatch = errors.New("stored branch definition does not match current parallel definition")
var ErrContinuationNotFound = errors.New("workflow continuation not found")
var ErrContinuationNotClaimable = errors.New("workflow continuation is not claimable")
var ErrContinuationLeaseInvalid = errors.New("workflow continuation lease is invalid")
