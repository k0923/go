// Package examples contains runnable examples for github.com/k0923/go/fluent/v2.
//
// These examples intentionally live in a separate package and import fluent/v2
// the same way application code does. Run them with:
//
//	go test ./fluent/v2/examples
//
// The files are grouped by feature:
//
//   - client_options_test.go: client construction, options, derived clients, and package-level shortcuts.
//   - request_bodies_test.go: path/query/header helpers, JSON, form, raw body, multipart, Do, Stream.
//   - retry_errors_test.go: retry policy, timeouts, accepted statuses, and structured error handling.
//   - middleware_codec_test.go: middleware, custom RoundTripper, JSON codec options, and custom codecs.
//   - events_test.go: Server-Sent Events for LLM-style streaming APIs.
//
// The examples are written with local httptest servers so they are deterministic
// and do not require network access.
package examples
