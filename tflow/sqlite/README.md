# SQLite Reference Adapter

`github.com/k0923/go/tflow/sqlite` is a durable Store implementation for business
teams to copy or adapt. Serialization lives here and in the business codec.
The tflow core has no serialization or type registry.

Run the complete example twice against the same database:

```sh
go run ./tflow/examples/sqlite -db /tmp/tflow-reference.db
go run ./tflow/examples/sqlite -db /tmp/tflow-reference.db
```

The first run executes twenty rows. The second reads the completed batch without
submitting it again. [The example](../examples/sqlite/main.go) contains its entire
business DTO, tagged versioned JSON codec, pagination mapping, and worker loop.
Its print-only executor is a demonstration, not durable business output.

## Integration

Create a repository with `sqlite.New(ctx, path, Options{Codec, CommitPlan})`, open
a bound Store with `repository.Open(ctx, mainID)`, and pass it to `tflow.New`.
Close the repository when its application owner shuts down.

The adapter's `Codec` has `Encode(any)` and `Decode([]byte)` methods. Supported
objects are MainTask, SubTask, Event, PlanningPolicy, and MainDecider. The caller
defines type identifiers, payload schema versions, validation, and migrations.
The returned decoded value must implement the expected protocol. Function
closures are intentionally not serialized; use named policy/decider types.

Main tasks can retain notification routing in their DTOs. Decode should inject
current clients, including when reconstructing historical command tasks.
Never substitute the latest task for a historical cancellation or notification.
Codecs and CommitPlan must be pure and safe for concurrent calls.

CommitPlan receives the previously persisted MainTask and the concrete business
Plan after CAS validation. It returns a new immutable task with the updated
source checkpoint. Do not re-run Tasks, HasMore, Plan, or remote IO in this hook.
Nil CommitPlan is suitable only for stateless plans.

Use `Engine.UpdateTask(ctx, version, nextTask)` for validation/approval facts.
It commits Task, Version, and a Reconcile command atomically. Keep the task's
identity and checkpoint intact. Conflicts require rereading the current task.
The public Tx does not expose a SQL connection; extending this reference for
atomic writes to additional business tables requires an adapter-owned API.

## Persistence And Concurrency

- Separate main, subtask, and command tables; unique `(main_id,key)` and
  `(main_id,cursor)` constraints.
- Main state and the five substatus counts are read in one SQL statement.
- Write callbacks run in `BEGIN IMMEDIATE` transactions. On error, cancellation,
  or panic all candidate writes roll back. Escaped Tx handles are rejected.
- Independent Repository instances and processes coordinate through SQLite
  locks. No process-local mutex protects correctness.
- WAL permits readers during writes. SQLite still has one writer per database;
  increasing workers does not remove this write serialization.
- Every connection enables foreign keys and a five-second busy timeout. Lock
  failures propagate; this adapter does not automatically replay callbacks.
- The adapter owns initial schema version 1. Unsupported database schema
  versions fail initialization rather than silently changing interpretation.

Lease claim, Ack, Release, due scans, and retry timestamps use SQLite time.
The clock is shared by connections on the database host; it is not a clock
service for multiple independent machines. Use a local database file. This
reference is intended for a single-host deployment, not a network filesystem.
Lease precision is milliseconds. Releases round positive delays up.

Delivery generation and the current stored expiry fence Ack/Release. They do
not prevent a stalled consumer from making an external side effect. Executors
still need stable execution/event idempotency and cancellation race handling.

## Worker And Operations

`Repository.DueMains(ctx, limit)` finds mains with currently claimable commands,
including terminal mains with pending cancellations or notifications. Bound
engines can use `DispatchBatch` to process them with a claim budget.
The index does not discover lost callbacks after an Executor has accepted work;
the reliable external runtime owns those callbacks and its recovery.

`Store.FindCommands` filters by kind and pages by ID. `SubQuery.AfterCursor`
provides stable subtask pagination.

The adapter exposes optional operator controls:

- `QuarantineCommand(ctx, delivery, reason)` fences the currently owned lease
  and retains its payload. Quarantined work is excluded from claims/due scans.
- `DeadLetters(ctx, query)` lists quarantined work.
- `RequeueCommand(ctx, id)` explicitly reactivates it and advances generation.
- `CompactAcknowledged(ctx, before)` removes old acknowledged payloads while
  retaining protocol identity tombstones. Duplicate PutCommand stays a no-op.

Default framework retries do not automatically quarantine. A worker that wants
an escalation threshold can claim/Handle/Ack or Release commands directly and
use generation as its claim count. Cancellation and termination notification
failures must stay visible until handled or deliberately quarantined.

## Verification

```sh
go test ./tflow/...
go test -race ./tflow/...
go vet ./tflow/...
go test -bench=. -benchmem -run='^$' ./tflow/sqlite
```

The test suite includes shared Store contracts, isolated decoded objects,
checkpoint and policy recovery after reopening, independent-connection reports
and claims, lease expiration and retry delay, concurrent reruns, versioned
business wakes, event recovery, historical routing, terminal-command rollback,
codec failure, canceled/panicking transactions, pagination, quarantine/requeue,
payload compaction without resurrection, and an abruptly exiting child process
after execution-state commit but before command Ack.

Benchmarks measure snapshots and state transactions at 100/1000 subtasks and
concurrent duplicate reports. They are reference measurements, not a production
capacity guarantee. Measure your checkpoint/codec cost and actual report rate
before changing the strong Main.Version consistency model.

One local 100 ms run on Apple M4 Pro / Go 1.27.1 measured:

| Operation | 100 Subtasks | 1000 Subtasks |
| --- | ---: | ---: |
| Main snapshot | 44.8 us | 91.4 us |
| State transaction | 230 us | 554 us |

The state transaction benchmark updates only Main.State; it is not a
Report/Cancel throughput benchmark. A production adapter can replace the five
indexed COUNT queries with transactionally maintained summary columns if
measurements justify the added complexity.
