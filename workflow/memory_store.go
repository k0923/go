package workflow

import (
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"
)

var _ WorkflowStore[any, any] = (*InMemoryWorkflowStore[any, any])(nil)

type inMemoryContinuationKey struct {
	runID      string
	branchPath string
}

type inMemoryContinuation struct {
	key        ContinuationKey
	status     ContinuationStatus
	ownerID    string
	generation uint64
	expiresAt  time.Time
	suspension *Suspension
}

type inMemoryBranchGroupKey struct {
	runID         string
	branchGroupID string
}

type inMemoryBranchGroup struct {
	parentKey ContinuationKey
	branchIDs []string
	remaining map[string]ContinuationKey
}

type InMemoryWorkflowStore[Input any, Output any] struct {
	lock              sync.RWMutex
	states            map[string]*RunState[Input, Output]
	events            map[string][]Event
	continuations     map[inMemoryContinuationKey]*inMemoryContinuation
	continuationOrder map[string][]inMemoryContinuationKey
	branchGroups      map[inMemoryBranchGroupKey]*inMemoryBranchGroup
}

func NewInMemoryWorkflowStore[Input any, Output any]() *InMemoryWorkflowStore[Input, Output] {
	store := &InMemoryWorkflowStore[Input, Output]{}
	store.initialize()
	return store
}

func (store *InMemoryWorkflowStore[Input, Output]) initialize() {
	if store.states == nil {
		store.states = make(map[string]*RunState[Input, Output])
	}
	if store.events == nil {
		store.events = make(map[string][]Event)
	}
	if store.continuations == nil {
		store.continuations = make(map[inMemoryContinuationKey]*inMemoryContinuation)
	}
	if store.continuationOrder == nil {
		store.continuationOrder = make(map[string][]inMemoryContinuationKey)
	}
	if store.branchGroups == nil {
		store.branchGroups = make(map[inMemoryBranchGroupKey]*inMemoryBranchGroup)
	}
}

func (store *InMemoryWorkflowStore[Input, Output]) CreateRun(
	runID string,
	input Input,
	ownerID string,
	leaseDuration time.Duration,
) (*RunState[Input, Output], ExecutionLease, error) {
	store.lock.Lock()
	defer store.lock.Unlock()
	store.initialize()

	if store.states[runID] != nil {
		return nil, ExecutionLease{}, ErrRunStateExists
	}
	now := time.Now()
	state := &RunState[Input, Output]{
		Status:    RunStatusRunning,
		Input:     input,
		CreatedAt: now,
		UpdatedAt: now,
	}
	key := ContinuationKey{RunID: runID}
	continuation := &inMemoryContinuation{
		key:        cloneContinuationKey(key),
		status:     ContinuationStatusRunning,
		ownerID:    ownerID,
		generation: 1,
		expiresAt:  now.Add(normalizeLeaseDuration(leaseDuration)),
	}
	mapKey := newInMemoryContinuationKey(key)
	store.states[runID] = state
	store.continuations[mapKey] = continuation
	store.continuationOrder[runID] = append(store.continuationOrder[runID], mapKey)
	return cloneRunState(state), continuation.lease(), nil
}

func (store *InMemoryWorkflowStore[Input, Output]) GetRunState(
	runID string,
) (*RunState[Input, Output], error) {
	store.lock.RLock()
	defer store.lock.RUnlock()
	state := store.states[runID]
	if state == nil {
		return nil, nil
	}
	return cloneRunState(state), nil
}

func (store *InMemoryWorkflowStore[Input, Output]) GetEvents(runID string) ([]Event, error) {
	store.lock.RLock()
	defer store.lock.RUnlock()
	return append([]Event(nil), store.events[runID]...), nil
}

func (store *InMemoryWorkflowStore[Input, Output]) ClaimContinuation(
	key ContinuationKey,
	ownerID string,
	leaseDuration time.Duration,
) (ExecutionLease, error) {
	store.lock.Lock()
	defer store.lock.Unlock()
	store.initialize()

	continuation := store.continuations[newInMemoryContinuationKey(key)]
	if continuation == nil {
		return ExecutionLease{}, ErrContinuationNotFound
	}
	lease, err := store.claimContinuationLocked(continuation, ownerID, leaseDuration)
	if err != nil {
		return ExecutionLease{}, err
	}
	store.refreshRunStatusLocked(key.RunID)
	return lease, nil
}

func (store *InMemoryWorkflowStore[Input, Output]) ClaimNextContinuation(
	runID string,
	ownerID string,
	leaseDuration time.Duration,
) (ExecutionLease, error) {
	store.lock.Lock()
	defer store.lock.Unlock()
	store.initialize()

	if store.states[runID] == nil {
		return ExecutionLease{}, ErrRunStateNotFound
	}
	now := time.Now()
	for _, key := range store.continuationOrder[runID] {
		continuation := store.continuations[key]
		if continuation == nil || !continuationClaimable(continuation, now) {
			continue
		}
		lease, err := store.claimContinuationLocked(continuation, ownerID, leaseDuration)
		if err != nil {
			continue
		}
		store.refreshRunStatusLocked(runID)
		return lease, nil
	}
	return ExecutionLease{}, ErrContinuationNotClaimable
}

func (store *InMemoryWorkflowStore[Input, Output]) RenewLease(
	lease ExecutionLease,
	leaseDuration time.Duration,
) (ExecutionLease, error) {
	store.lock.Lock()
	defer store.lock.Unlock()
	store.initialize()

	continuation, err := store.validateLeaseLocked(lease)
	if err != nil {
		return ExecutionLease{}, err
	}
	continuation.expiresAt = time.Now().Add(normalizeLeaseDuration(leaseDuration))
	return continuation.lease(), nil
}

func (store *InMemoryWorkflowStore[Input, Output]) ReleaseContinuation(
	lease ExecutionLease,
) error {
	store.lock.Lock()
	defer store.lock.Unlock()
	store.initialize()

	continuation, err := store.validateLeaseLocked(lease)
	if err != nil {
		return err
	}
	continuation.status = ContinuationStatusRunnable
	continuation.clearLease()
	store.refreshRunStatusLocked(lease.Key.RunID)
	return nil
}

func (store *InMemoryWorkflowStore[Input, Output]) AppendEvent(
	lease ExecutionLease,
	event Event,
) error {
	store.lock.Lock()
	defer store.lock.Unlock()
	store.initialize()

	if _, err := store.validateLeaseLocked(lease); err != nil {
		return err
	}
	if !slices.Equal(event.GetBase().BranchIds, lease.Key.BranchIDs) {
		return ErrContinuationLeaseInvalid
	}
	store.saveEventLocked(lease.Key.RunID, event)
	return nil
}

func (store *InMemoryWorkflowStore[Input, Output]) SuspendContinuation(
	lease ExecutionLease,
	event Event,
	suspension Suspension,
) error {
	store.lock.Lock()
	defer store.lock.Unlock()
	store.initialize()

	continuation, err := store.validateLeaseLocked(lease)
	if err != nil {
		return err
	}
	if event != nil {
		if !slices.Equal(event.GetBase().BranchIds, lease.Key.BranchIDs) {
			return ErrContinuationLeaseInvalid
		}
		if !IdentityOf(event).Equal(suspension.Event) {
			return ErrContinuationLeaseInvalid
		}
		store.saveEventLocked(lease.Key.RunID, event)
	}
	suspension.Event = cloneEventIdentity(suspension.Event)
	continuation.status = ContinuationStatusSuspended
	continuation.suspension = &suspension
	continuation.clearLease()
	store.refreshRunStatusLocked(lease.Key.RunID)
	return nil
}

func (store *InMemoryWorkflowStore[Input, Output]) ResolveSignal(
	runID string,
	awaited EventIdentity,
	resolved *SignalResolvedEvent,
	ownerID string,
	leaseDuration time.Duration,
) (ExecutionLease, error) {
	store.lock.Lock()
	defer store.lock.Unlock()
	store.initialize()

	key := ContinuationKey{
		RunID:     runID,
		BranchIDs: append([]string(nil), awaited.BranchIds...),
	}
	continuation := store.continuations[newInMemoryContinuationKey(key)]
	if continuation == nil ||
		continuation.status != ContinuationStatusSuspended ||
		continuation.suspension == nil ||
		continuation.suspension.Kind != SuspensionKindSignal ||
		!continuation.suspension.Event.Equal(awaited) {
		return ExecutionLease{}, ErrAwaitedSignalNotFound
	}
	if resolved.Name != awaited.Name ||
		!slices.Equal(resolved.BranchIds, awaited.BranchIds) ||
		resolved.Previous == nil ||
		!resolved.Previous.Equal(awaited) {
		return ExecutionLease{}, ErrContinuationLeaseInvalid
	}
	store.saveEventLocked(runID, resolved)
	continuation.suspension = nil
	lease := activateContinuationLocked(continuation, ownerID, leaseDuration)
	store.refreshRunStatusLocked(runID)
	return lease, nil
}

func (store *InMemoryWorkflowStore[Input, Output]) ForkParallel(
	lease ExecutionLease,
	branchGroupID string,
	started *ParallelStartedEvent,
	branchIDs []string,
	leaseDuration time.Duration,
) (ForkParallelResult, error) {
	if err := validateBranchIDs(branchIDs); err != nil {
		return ForkParallelResult{}, err
	}

	store.lock.Lock()
	defer store.lock.Unlock()
	store.initialize()

	parent, err := store.validateLeaseLocked(lease)
	if err != nil {
		return ForkParallelResult{}, err
	}
	if !slices.Equal(started.BranchIds, lease.Key.BranchIDs) {
		return ForkParallelResult{}, ErrContinuationLeaseInvalid
	}
	if !sameBranchIDs(started.Branches, branchIDs) {
		return ForkParallelResult{}, ErrBranchDefinitionMismatch
	}
	groupKey := inMemoryBranchGroupKey{
		runID:         lease.Key.RunID,
		branchGroupID: branchGroupID,
	}
	if group := store.branchGroups[groupKey]; group != nil {
		if !sameBranchIDs(group.branchIDs, branchIDs) {
			return ForkParallelResult{}, fmt.Errorf("%w: %s", ErrBranchDefinitionMismatch, branchGroupID)
		}
		return ForkParallelResult{Initialized: false}, nil
	}

	store.saveEventLocked(lease.Key.RunID, started)
	if len(branchIDs) == 0 {
		return ForkParallelResult{Initialized: true}, nil
	}

	now := time.Now()
	duration := normalizeLeaseDuration(leaseDuration)
	group := &inMemoryBranchGroup{
		parentKey: cloneContinuationKey(lease.Key),
		branchIDs: append([]string(nil), branchIDs...),
		remaining: make(map[string]ContinuationKey, len(branchIDs)),
	}
	result := ForkParallelResult{
		BranchLeases: make([]ExecutionLease, 0, len(branchIDs)),
		Initialized:  true,
	}
	for _, branchID := range branchIDs {
		childKey := ContinuationKey{
			RunID:     lease.Key.RunID,
			BranchIDs: append(append([]string(nil), lease.Key.BranchIDs...), branchID),
		}
		child := store.continuations[newInMemoryContinuationKey(childKey)]
		if child != nil && child.status != ContinuationStatusCompleted {
			return ForkParallelResult{}, fmt.Errorf("%w: %s", ErrBranchDefinitionMismatch, branchID)
		}
	}
	for _, branchID := range branchIDs {
		childKey := ContinuationKey{
			RunID:     lease.Key.RunID,
			BranchIDs: append(append([]string(nil), lease.Key.BranchIDs...), branchID),
		}
		mapKey := newInMemoryContinuationKey(childKey)
		child := store.continuations[mapKey]
		if child == nil {
			child = &inMemoryContinuation{
				key:        cloneContinuationKey(childKey),
				generation: 0,
			}
			store.continuations[mapKey] = child
			store.continuationOrder[lease.Key.RunID] = append(
				store.continuationOrder[lease.Key.RunID],
				mapKey,
			)
		}
		child.status = ContinuationStatusRunning
		child.ownerID = lease.OwnerID
		child.generation++
		child.expiresAt = now.Add(duration)
		child.suspension = nil
		group.remaining[branchID] = cloneContinuationKey(childKey)
		result.BranchLeases = append(result.BranchLeases, child.lease())
	}
	store.branchGroups[groupKey] = group
	parent.status = ContinuationStatusWaiting
	parent.clearLease()
	store.refreshRunStatusLocked(lease.Key.RunID)
	return result, nil
}

func (store *InMemoryWorkflowStore[Input, Output]) CompleteContinuation(
	lease ExecutionLease,
	branchGroupID string,
	branchCompleted *BranchCompletedEvent,
	parallelCompleted *ParallelCompletedEvent,
	leaseDuration time.Duration,
) (CompleteContinuationResult, error) {
	store.lock.Lock()
	defer store.lock.Unlock()
	store.initialize()

	continuation, err := store.validateLeaseLocked(lease)
	if err != nil {
		return CompleteContinuationResult{}, err
	}
	group := store.branchGroups[inMemoryBranchGroupKey{
		runID:         lease.Key.RunID,
		branchGroupID: branchGroupID,
	}]
	if group == nil {
		return CompleteContinuationResult{}, fmt.Errorf("%w: %s", ErrBranchGroupNotFound, branchGroupID)
	}
	if len(lease.Key.BranchIDs) == 0 {
		return CompleteContinuationResult{}, ErrContinuationLeaseInvalid
	}
	branchID := lease.Key.BranchIDs[len(lease.Key.BranchIDs)-1]
	storedKey, exists := group.remaining[branchID]
	if !exists || !continuationKeysEqual(storedKey, lease.Key) {
		return CompleteContinuationResult{}, fmt.Errorf("%w: %s", ErrBranchDefinitionMismatch, branchID)
	}
	if !slices.Equal(branchCompleted.BranchIds, lease.Key.BranchIDs) {
		return CompleteContinuationResult{}, ErrContinuationLeaseInvalid
	}
	if branchCompleted.BranchID != branchID ||
		!slices.Equal(parallelCompleted.BranchIds, group.parentKey.BranchIDs) {
		return CompleteContinuationResult{}, ErrContinuationLeaseInvalid
	}

	continuation.status = ContinuationStatusCompleted
	continuation.clearLease()
	store.saveEventLocked(lease.Key.RunID, branchCompleted)
	delete(group.remaining, branchID)
	result := CompleteContinuationResult{
		RemainingBranchIDs: group.remainingBranchIDs(),
	}
	if len(group.remaining) == 0 {
		parent := store.continuations[newInMemoryContinuationKey(group.parentKey)]
		if parent == nil || parent.status != ContinuationStatusWaiting {
			return CompleteContinuationResult{}, ErrContinuationNotFound
		}
		store.saveEventLocked(lease.Key.RunID, parallelCompleted)
		parent.status = ContinuationStatusRunning
		parent.ownerID = lease.OwnerID
		parent.generation++
		parent.expiresAt = time.Now().Add(normalizeLeaseDuration(leaseDuration))
		parent.suspension = nil
		parentLease := parent.lease()
		result.ParentLease = &parentLease
	}
	store.refreshRunStatusLocked(lease.Key.RunID)
	return result, nil
}

func (store *InMemoryWorkflowStore[Input, Output]) CompleteRun(
	lease ExecutionLease,
	output Output,
) error {
	store.lock.Lock()
	defer store.lock.Unlock()
	store.initialize()

	continuation, err := store.validateLeaseLocked(lease)
	if err != nil {
		return err
	}
	if len(lease.Key.BranchIDs) != 0 {
		return ErrContinuationLeaseInvalid
	}
	state := store.states[lease.Key.RunID]
	if state == nil {
		return ErrRunStateNotFound
	}
	continuation.status = ContinuationStatusCompleted
	continuation.clearLease()
	state.Status = RunStatusFinished
	state.Output = output
	state.UpdatedAt = time.Now()
	return nil
}

func (store *InMemoryWorkflowStore[Input, Output]) claimContinuationLocked(
	continuation *inMemoryContinuation,
	ownerID string,
	leaseDuration time.Duration,
) (ExecutionLease, error) {
	now := time.Now()
	if !continuationClaimable(continuation, now) {
		return ExecutionLease{}, ErrContinuationNotClaimable
	}
	return activateContinuationLocked(continuation, ownerID, leaseDuration), nil
}

func activateContinuationLocked(
	continuation *inMemoryContinuation,
	ownerID string,
	leaseDuration time.Duration,
) ExecutionLease {
	continuation.status = ContinuationStatusRunning
	continuation.ownerID = ownerID
	continuation.generation++
	continuation.expiresAt = time.Now().Add(normalizeLeaseDuration(leaseDuration))
	continuation.suspension = nil
	return continuation.lease()
}

func continuationClaimable(
	continuation *inMemoryContinuation,
	now time.Time,
) bool {
	switch continuation.status {
	case ContinuationStatusRunnable:
		return true
	case ContinuationStatusSuspended:
		return continuation.suspension == nil ||
			continuation.suspension.Kind == SuspensionKindSleep
	case ContinuationStatusRunning:
		return !continuation.expiresAt.After(now)
	default:
		return false
	}
}

func (store *InMemoryWorkflowStore[Input, Output]) validateLeaseLocked(
	lease ExecutionLease,
) (*inMemoryContinuation, error) {
	continuation := store.continuations[newInMemoryContinuationKey(lease.Key)]
	if continuation == nil {
		return nil, ErrContinuationNotFound
	}
	if continuation.status != ContinuationStatusRunning ||
		continuation.ownerID != lease.OwnerID ||
		continuation.generation != lease.Generation ||
		time.Now().After(continuation.expiresAt) {
		return nil, ErrContinuationLeaseInvalid
	}
	return continuation, nil
}

func (store *InMemoryWorkflowStore[Input, Output]) refreshRunStatusLocked(runID string) {
	state := store.states[runID]
	if state == nil || state.Status == RunStatusFinished {
		return
	}
	root := store.continuations[newInMemoryContinuationKey(ContinuationKey{RunID: runID})]
	if root != nil && root.status == ContinuationStatusCompleted {
		state.Status = RunStatusFinished
		state.UpdatedAt = time.Now()
		return
	}
	state.Status = RunStatusPaused
	for _, key := range store.continuationOrder[runID] {
		continuation := store.continuations[key]
		if continuation == nil {
			continue
		}
		if continuation.status == ContinuationStatusRunning ||
			continuation.status == ContinuationStatusRunnable {
			state.Status = RunStatusRunning
			break
		}
	}
	state.UpdatedAt = time.Now()
}

func (store *InMemoryWorkflowStore[Input, Output]) saveEventLocked(runID string, event Event) {
	for index, storedEvent := range store.events[runID] {
		if sameEventIdentity(storedEvent, event) {
			store.events[runID][index] = event
			return
		}
	}
	store.events[runID] = append(store.events[runID], event)
}

func (continuation *inMemoryContinuation) lease() ExecutionLease {
	return ExecutionLease{
		Key:        cloneContinuationKey(continuation.key),
		OwnerID:    continuation.ownerID,
		Generation: continuation.generation,
		ExpiresAt:  continuation.expiresAt,
	}
}

func (continuation *inMemoryContinuation) clearLease() {
	continuation.ownerID = ""
	continuation.expiresAt = time.Time{}
}

func (group *inMemoryBranchGroup) remainingBranchIDs() []string {
	remaining := make([]string, 0, len(group.remaining))
	for _, branchID := range group.branchIDs {
		if _, exists := group.remaining[branchID]; exists {
			remaining = append(remaining, branchID)
		}
	}
	return remaining
}

func newInMemoryContinuationKey(key ContinuationKey) inMemoryContinuationKey {
	branchIDs, _ := json.Marshal(key.BranchIDs)
	return inMemoryContinuationKey{
		runID:      key.RunID,
		branchPath: string(branchIDs),
	}
}

func cloneContinuationKey(key ContinuationKey) ContinuationKey {
	key.BranchIDs = append([]string(nil), key.BranchIDs...)
	return key
}

func continuationKeysEqual(left ContinuationKey, right ContinuationKey) bool {
	return left.RunID == right.RunID && slices.Equal(left.BranchIDs, right.BranchIDs)
}

func cloneEventIdentity(identity EventIdentity) EventIdentity {
	identity.BranchIds = append([]string(nil), identity.BranchIds...)
	return identity
}

func cloneRunState[Input any, Output any](
	state *RunState[Input, Output],
) *RunState[Input, Output] {
	cloned := *state
	return &cloned
}

func normalizeLeaseDuration(duration time.Duration) time.Duration {
	if duration <= 0 {
		return defaultLeaseDuration
	}
	return duration
}

func validateBranchIDs(branchIDs []string) error {
	seen := make(map[string]struct{}, len(branchIDs))
	for _, branchID := range branchIDs {
		if branchID == "" {
			return ErrBranchIDRequired
		}
		if _, exists := seen[branchID]; exists {
			return fmt.Errorf("%w: %s", ErrDuplicateBranchID, branchID)
		}
		seen[branchID] = struct{}{}
	}
	return nil
}

func sameBranchIDs(left []string, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	leftCopy := append([]string(nil), left...)
	rightCopy := append([]string(nil), right...)
	slices.Sort(leftCopy)
	slices.Sort(rightCopy)
	return slices.Equal(leftCopy, rightCopy)
}
