// Package tflow implements two-level scheduling for a single logical main task.
// A Repository locates it; Store is bound to its stable MainID, and Engine
// lifecycle methods operate on that scope without repeating the identity.
// Business objects implement MainTask, SubTask and Plan; Store adapters own
// persistence and reconstruction. Scheduling state and durable dispatch intents
// are committed atomically, and execution is supplied through ports.
// Options.Executor provides direct in-process execution through the Executor port.
// Resumable tasks can implement TerminationHandler, with persisted routing
// identities and Store-reconstructed dependencies, to notify across instances.
// Stores provide consistent SubSummary values alongside main state. Business
// MainDecider implementations determine lifecycle outcomes; Engine commits their
// decisions with version checks and enforces execution/quota invariants.
package tflow
