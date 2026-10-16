# fluent Examples

This package contains runnable Go examples for `github.com/k0923/go/fluent`.

Run all examples:

```bash
go test ./fluent/examples
```

## Navigation

- `client_options_test.go`: `New`, `MustNew`, `With`, `WithBaseURL`, `WithHTTPClient`, `WithHeader`, `WithHeaders`, `WithUserAgent`, production-style Client setup, package-level shortcuts.
- `request_bodies_test.go`: `Path`, `Query`, `Queries`, query structs, `Header`, `Headers`, `Bearer`, `BasicAuth`, `Cookie`, `JSON`, `Form`, `BodyBytes`, `BodyReader`, `BodyFunc`, retryable `Multipart`, `Do`, `Exec`, `Into`, `Decode`, `Bytes`, `Stream`, `Head`.
- `retry_errors_test.go`: `RetryPolicy`, `ConstantBackoff`, `ExponentialBackoff`, `DefaultRetryIf`, `AttemptFromContext`, `Timeout`, `NoRetry`, `AcceptStatus`, `Error`, `Kind`, `StatusCode`, `ErrorDecoder`, category-based error handling.
- `middleware_codec_test.go`: `Middleware`, `RoundTripperFunc`, `Logging`, dynamic header middleware, `DefaultCodec`, `JSON`, custom `Codec`.
- `events_test.go`: `Events`, `EventsOf`, `EventReader`, OpenAI-compatible streams, Anthropic-style event dispatch, pre-stream retry/status handling, `SkipData`, `SkipType`, `WithIdleTimeout`, `IdleTimeout`, `WithMaxEventSize`, `ErrEventTooLarge`.

Each example uses a local `httptest.Server`; no external API key or network endpoint is required.
