// Package memory provides a transactional in-memory Store and FakeDispatcher.
// Use memory for local applications; process restarts discard its contents.
package memory

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/k0923/go/tflow"
)

type aggregate struct {
	main     *tflow.MainRecord
	subs     map[string]tflow.SubRecord
	commands map[string]tflow.CommandState
}

type Repository struct {
	mu           sync.Mutex
	scopes       map[string]*scope
	copyMainTask func(tflow.MainTask) tflow.MainTask
	copySubTask  func(tflow.SubTask) tflow.SubTask
	copyEvent    func(tflow.Event) tflow.Event
	commitPlan   func(tflow.MainTask, tflow.Plan) (tflow.MainTask, error)
	clock        tflow.Clock
}

type scope struct {
	mu   sync.Mutex
	data *aggregate
}

// Store is a handle bound to one logical main task.
type Store struct {
	repository *Repository
	id         string
	state      *scope
}

type Option func(*Repository)

func WithClock(clock tflow.Clock) Option {
	return func(r *Repository) { r.clock = clock }
}

// WithPlanCommit projects an application's Plan into persisted business
// progress. It must be pure, retry-safe and return an immutable task/copy.
// Without a hook, plans are stateless and preserve the current task object.
func WithPlanCommit(commit func(tflow.MainTask, tflow.Plan) (tflow.MainTask, error)) Option {
	return func(s *Repository) { s.commitPlan = commit }
}

// Copy hooks are adapter-owned. Without hooks, business objects must be immutable.
func WithMainTaskCopy(copy func(tflow.MainTask) tflow.MainTask) Option {
	return func(s *Repository) { s.copyMainTask = copy }
}
func WithSubTaskCopy(copy func(tflow.SubTask) tflow.SubTask) Option {
	return func(s *Repository) { s.copySubTask = copy }
}
func WithEventCopy(copy func(tflow.Event) tflow.Event) Option {
	return func(s *Repository) { s.copyEvent = copy }
}

var _ tflow.Store = (*Store)(nil)
var _ tflow.Repository = (*Repository)(nil)

func New(options ...Option) *Repository {
	s := &Repository{scopes: make(map[string]*scope), clock: tflow.ClockFunc(time.Now)}
	for _, option := range options {
		option(s)
	}
	return s
}

func (r *Repository) ForMain(id string) *Store {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.scopes[id]
	if state == nil {
		state = &scope{}
		r.scopes[id] = state
	}
	return &Store{repository: r, id: id, state: state}
}

func (r *Repository) Open(ctx context.Context, id string) (tflow.Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if id == "" {
		return nil, tflow.ErrInvalid
	}
	return r.ForMain(id), nil
}

func (s *Store) MainID() string { return s.id }

func (s *Store) copyMain(m tflow.MainRecord) tflow.MainRecord {
	if s.repository.copyMainTask != nil && !tflow.IsNil(m.Task) {
		m.Task = s.repository.copyMainTask(m.Task)
	}
	return m
}
func (s *Store) copySub(sub tflow.SubRecord) tflow.SubRecord {
	if s.repository.copySubTask != nil && !tflow.IsNil(sub.Task) {
		sub.Task = s.repository.copySubTask(sub.Task)
	}
	return sub
}
func (s *Store) copyCommand(c tflow.Command) tflow.Command {
	c.Main = s.copyMain(c.Main)
	c.Sub = s.copySub(c.Sub)
	if s.repository.copyEvent != nil && !tflow.IsNil(c.Event) {
		c.Event = s.repository.copyEvent(c.Event)
	}
	return c
}

func (s *Store) clone(a *aggregate) *aggregate {
	next := &aggregate{subs: make(map[string]tflow.SubRecord), commands: make(map[string]tflow.CommandState)}
	if a == nil {
		return next
	}
	if a.main != nil {
		m := s.copyMain(*a.main)
		next.main = &m
	}
	for id, sub := range a.subs {
		next.subs[id] = s.copySub(sub)
	}
	for id, c := range a.commands {
		c.Command = s.copyCommand(c.Command)
		next.commands[id] = c
	}
	return next
}

func (s *Store) mainRecord(a *aggregate) tflow.MainRecord {
	record := s.copyMain(*a.main)
	record.Subs = summarize(a)
	return record
}

func summarize(a *aggregate) tflow.SubSummary {
	var summary tflow.SubSummary
	for _, sub := range a.subs {
		switch sub.Status {
		case tflow.SubPending:
			summary.Pending++
		case tflow.SubRunning:
			summary.Running++
		case tflow.SubSucceeded:
			summary.Succeeded++
		case tflow.SubFailed:
			summary.Failed++
		case tflow.SubCanceled:
			summary.Canceled++
		}
	}
	return summary
}

func (s *Store) GetMain(ctx context.Context) (tflow.MainRecord, error) {
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return tflow.MainRecord{}, err
	}
	a := s.state.data
	if a == nil || a.main == nil {
		return tflow.MainRecord{}, tflow.ErrNotFound
	}
	return s.mainRecord(a), nil
}

func (s *Store) GetSub(ctx context.Context, key string) (tflow.SubRecord, error) {
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return tflow.SubRecord{}, err
	}
	a := s.state.data
	if a == nil {
		return tflow.SubRecord{}, tflow.ErrNotFound
	}
	sub, ok := a.subs[key]
	if !ok {
		return tflow.SubRecord{}, tflow.ErrNotFound
	}
	return s.copySub(sub), nil
}

type transaction struct {
	a      *aggregate
	active bool
	ctx    context.Context
	store  *Store
}

func (s *Store) WithTx(ctx context.Context, fn func(tflow.Tx) error) error {
	if s.id == "" || fn == nil {
		return tflow.ErrInvalid
	}
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	tx := &transaction{a: s.clone(s.state.data), active: true, ctx: ctx, store: s}
	defer func() { tx.active = false }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validate(tx.a); err != nil {
		return err
	}
	before := s.state.data
	if before != nil && before.main != nil && summarize(before) != summarize(tx.a) &&
		tx.a.main.Version <= before.main.Version {
		return fmt.Errorf("%w: subtask summary changed without advancing main Version", tflow.ErrInvalid)
	}
	s.state.data = s.clone(tx.a)
	return nil
}

func (tx *transaction) check() error {
	if !tx.active {
		return tflow.ErrInvalid
	}
	return tx.ctx.Err()
}

func (tx *transaction) Main() (tflow.MainRecord, error) {
	if err := tx.check(); err != nil {
		return tflow.MainRecord{}, err
	}
	if tx.a.main == nil {
		return tflow.MainRecord{}, tflow.ErrNotFound
	}
	return tx.store.mainRecord(tx.a), nil
}

func (tx *transaction) CreateMain(m tflow.MainRecord) error {
	if err := tx.check(); err != nil {
		return err
	}
	if tx.a.main != nil {
		return tflow.ErrAlreadyExists
	}
	if m.Version != 1 {
		return tflow.ErrInvalid
	}
	if tflow.IsNil(m.Task) {
		return tflow.ErrInvalid
	}
	if err := tflow.ValidateMainState(m.MainConfig, m.MainState); err != nil {
		return err
	}
	m = tx.store.copyMain(m)
	m.Subs = tflow.SubSummary{}
	tx.a.main = &m
	return nil
}

func (tx *transaction) CommitPlan(expected int64, next tflow.MainRecord, plan tflow.Plan) error {
	if err := tx.check(); err != nil {
		return err
	}
	if tflow.IsNil(plan) {
		return tflow.ErrInvalid
	}
	task := next.Task
	if expected == 0 {
		if tx.a.main != nil {
			return tflow.ErrAlreadyExists
		}
		if next.Version != 1 {
			return tflow.ErrInvalid
		}
	} else {
		if tx.a.main == nil {
			return tflow.ErrNotFound
		}
		if tx.a.main.Version != expected {
			return tflow.ErrConflict
		}
		if next.Version != expected+1 {
			return tflow.ErrInvalid
		}
		task = tx.a.main.Task
		next.MainConfig = tx.a.main.MainConfig
	}
	if tx.store.repository.commitPlan != nil {
		var err error
		task, err = tx.store.repository.commitPlan(tx.store.copyMain(tflow.MainRecord{Task: task}).Task, plan)
		if err != nil {
			return err
		}
	}
	next.Task = task
	if tflow.IsNil(task) {
		return tflow.ErrInvalid
	}
	if expected == 0 {
		return tx.CreateMain(next)
	}
	if err := tx.SaveMain(expected, next.MainState); err != nil {
		return err
	}
	tx.a.main.Task = tx.store.copyMain(next).Task
	return nil
}

func (tx *transaction) SaveMain(expected int64, state tflow.MainState) error {
	if err := tx.check(); err != nil {
		return err
	}
	if tx.a.main == nil {
		return tflow.ErrNotFound
	}
	if tx.a.main.Version != expected {
		return tflow.ErrConflict
	}
	if state.Version != expected+1 {
		return tflow.ErrInvalid
	}
	m := *tx.a.main
	m.MainState = state
	if err := tflow.ValidateMainState(m.MainConfig, m.MainState); err != nil {
		return err
	}
	m = tx.store.copyMain(m)
	tx.a.main = &m
	return nil
}

func (tx *transaction) SaveTask(expected int64, task tflow.MainTask, now time.Time) error {
	m, err := tx.Main()
	if err != nil {
		return err
	}
	if m.Version != expected {
		return tflow.ErrConflict
	}
	if tflow.IsNil(task) || m.Status.Terminal() {
		return tflow.ErrInvalid
	}
	m.Version++
	m.UpdatedAt = now
	if err := tx.SaveMain(expected, m.MainState); err != nil {
		return err
	}
	tx.a.main.Task = tx.store.copyMain(tflow.MainRecord{Task: task}).Task
	return nil
}

func (tx *transaction) Sub(id string) (tflow.SubRecord, error) {
	if err := tx.check(); err != nil {
		return tflow.SubRecord{}, err
	}
	sub, ok := tx.a.subs[id]
	if !ok {
		return tflow.SubRecord{}, tflow.ErrNotFound
	}
	return tx.store.copySub(sub), nil
}

func (tx *transaction) FindSubs(q tflow.SubQuery) ([]tflow.SubRecord, error) {
	if err := tx.check(); err != nil {
		return nil, err
	}
	return tx.store.findSubs(tx.a, q)
}

// FindSubs reads only the selected aggregate; it never clones or commits it.
func (s *Store) FindSubs(ctx context.Context, q tflow.SubQuery) ([]tflow.SubRecord, error) {
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a := s.state.data
	if a == nil || a.main == nil {
		return nil, tflow.ErrNotFound
	}
	return s.findSubs(a, q)
}

func (s *Store) findSubs(a *aggregate, q tflow.SubQuery) ([]tflow.SubRecord, error) {
	if q.Limit < 0 {
		return nil, tflow.ErrInvalid
	}
	result := make([]tflow.SubRecord, 0)
	for _, sub := range a.subs {
		if q.AfterCursor != nil && sub.Cursor <= *q.AfterCursor {
			continue
		}
		if len(q.Statuses) > 0 && !contains(q.Statuses, sub.Status) {
			continue
		}
		if len(q.Keys) > 0 && !contains(q.Keys, sub.Key) {
			continue
		}
		result = append(result, s.copySub(sub))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Cursor < result[j].Cursor })
	if q.Limit > 0 && len(result) > q.Limit {
		result = result[:q.Limit]
	}
	return result, nil
}

func contains[T comparable](values []T, value T) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

func (tx *transaction) CreateSubs(subs []tflow.SubRecord) error {
	if err := tx.check(); err != nil {
		return err
	}
	for _, sub := range subs {
		if sub.Version != 1 {
			return tflow.ErrInvalid
		}
		if err := tflow.ValidateSub(sub); err != nil {
			return err
		}
		for _, old := range tx.a.subs {
			if old.Key == sub.Key || old.Cursor == sub.Cursor {
				return &tflow.PlanError{
					MainID: tx.store.MainID(), Field: "identity", Index: -1, Cause: tflow.ErrInvalidPlan,
					Reason: fmt.Sprintf("subtask key %q / cursor %d conflicts with existing key %q / cursor %d", sub.Key, sub.Cursor, old.Key, old.Cursor),
				}
			}
		}
		tx.a.subs[sub.Key] = tx.store.copySub(sub)
	}
	return nil
}

func (tx *transaction) SaveSub(expected int64, state tflow.SubState) error {
	if err := tx.check(); err != nil {
		return err
	}
	old, ok := tx.a.subs[state.Key]
	if !ok {
		return tflow.ErrNotFound
	}
	if old.Version != expected {
		return tflow.ErrConflict
	}
	if state.Version != expected+1 || state.Cursor != old.Cursor {
		return tflow.ErrInvalid
	}
	sub := old
	sub.SubState = state
	if err := tflow.ValidateSub(sub); err != nil {
		return err
	}
	tx.a.subs[sub.Key] = tx.store.copySub(sub)
	return nil
}

func (tx *transaction) PutCommand(c tflow.Command) error {
	if err := tx.check(); err != nil {
		return err
	}
	if c.MainID != tx.store.MainID() || c.ID == "" {
		return tflow.ErrInvalid
	}
	if old, ok := tx.a.commands[c.ID]; ok {
		original := old.Command
		same := original.Kind == c.Kind && original.MainID == c.MainID &&
			original.Sub.Execution(c.MainID).Key() == c.Sub.Execution(c.MainID).Key() &&
			original.EventKey == c.EventKey
		if c.Kind == tflow.CancelSub {
			same = same && original.Sub.ExternalRef == c.Sub.ExternalRef
		}
		if c.Kind == tflow.MainTerminated {
			// Compare protocol fields only; business objects stay opaque.
			same = same && original.Main.Version == c.Main.Version &&
				original.Main.Status == c.Main.Status && original.Main.DispatchCursor == c.Main.DispatchCursor &&
				original.Main.DispatchExhausted == c.Main.DispatchExhausted &&
				original.Main.Subs == c.Main.Subs &&
				original.Main.Error == c.Main.Error
		}
		if !same {
			return tflow.ErrConflict
		}
		return nil
	}
	tx.a.commands[c.ID] = tflow.CommandState{Command: tx.store.copyCommand(c)}
	return nil
}

func validate(a *aggregate) error {
	if a.main == nil {
		return tflow.ErrNotFound
	}
	for _, sub := range a.subs {
		if err := tflow.ValidateSub(sub); err != nil {
			return err
		}
	}
	m := *a.main
	m.Subs = summarize(a)
	if err := tflow.ValidateMain(m); err != nil {
		return fmt.Errorf("%w: aggregate summary disagrees with main state", err)
	}
	return nil
}

func (s *Store) ClaimCommand(ctx context.Context, ttl time.Duration) (*tflow.Delivery, error) {
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ttl <= 0 {
		return nil, tflow.ErrInvalid
	}
	now := s.repository.clock.Now()
	a := s.state.data
	if a == nil {
		return nil, tflow.ErrNotFound
	}
	keys := make([]string, 0)
	for key, c := range a.commands {
		if !c.Done && !c.Command.NotBefore.After(now) && !c.ExpiresAt.After(now) {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := a.commands[keys[i]].Command.NotBefore, a.commands[keys[j]].Command.NotBefore
		if !a.Equal(b) {
			return a.Before(b)
		}
		return keys[i] < keys[j]
	})
	if len(keys) == 0 {
		return nil, nil
	}
	c := a.commands[keys[0]]
	c.Generation++
	c.ExpiresAt = now.Add(ttl)
	a.commands[keys[0]] = c
	return &tflow.Delivery{Command: s.copyCommand(c.Command), Generation: c.Generation, ExpiresAt: c.ExpiresAt}, nil
}

func (s *Store) updateDelivery(ctx context.Context, d tflow.Delivery, update func(*tflow.CommandState, time.Time)) error {
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	a := s.state.data
	now := s.repository.clock.Now()
	if a == nil || d.Command.MainID != s.id {
		return tflow.ErrDeliveryLeaseLost
	}
	c, ok := a.commands[d.Command.ID]
	if !ok || c.Done || c.Generation != d.Generation || !c.ExpiresAt.After(now) {
		return tflow.ErrDeliveryLeaseLost
	}
	update(&c, now)
	a.commands[d.Command.ID] = c
	return nil
}

func (s *Store) AckCommand(ctx context.Context, d tflow.Delivery) error {
	return s.updateDelivery(ctx, d, func(c *tflow.CommandState, _ time.Time) { c.Done = true; c.ExpiresAt = time.Time{} })
}

func (s *Store) ReleaseCommand(ctx context.Context, d tflow.Delivery, retryAfter time.Duration, lastError string) error {
	if retryAfter < 0 {
		return tflow.ErrInvalid
	}
	return s.updateDelivery(ctx, d, func(c *tflow.CommandState, now time.Time) {
		c.ExpiresAt = time.Time{}
		c.Command.NotBefore = now.Add(retryAfter)
		c.LastError = lastError
	})
}

func (s *Store) PendingCommands(ctx context.Context) ([]tflow.CommandState, error) {
	return s.FindCommands(ctx, tflow.CommandQuery{})
}

func (s *Store) FindCommands(ctx context.Context, q tflow.CommandQuery) ([]tflow.CommandState, error) {
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if q.Limit < 0 {
		return nil, tflow.ErrInvalid
	}
	a := s.state.data
	if a == nil {
		return nil, tflow.ErrNotFound
	}
	result := make([]tflow.CommandState, 0)
	for _, c := range a.commands {
		if !c.Done && c.Command.ID > q.AfterID && (len(q.Kinds) == 0 || contains(q.Kinds, c.Command.Kind)) {
			c.Command = s.copyCommand(c.Command)
			result = append(result, c)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Command.ID < result[j].Command.ID })
	if q.Limit > 0 && len(result) > q.Limit {
		result = result[:q.Limit]
	}
	return result, nil
}
