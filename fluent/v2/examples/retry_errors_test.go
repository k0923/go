package examples

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	fluent "github.com/k0923/go/fluent/v2"
)

type serviceError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *serviceError) Error() string {
	return e.Code + ": " + e.Message
}

// ExampleRetryPolicy_get demonstrates retrying transient failures for idempotent requests.
func Example_retryPolicyGet() {
	var calls atomic.Int32
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, http.StatusOK, `{"id":"u1","name":"Ada"}`)
	})
	defer closeServer()

	client := fluent.MustNew(
		fluent.WithBaseURL(srv.URL),
		fluent.WithRetry(fluent.RetryPolicy{
			MaxAttempts: 2,
			Backoff:     fluent.ConstantBackoff(0),
		}),
	)
	user, err := client.Get("/users/u1").Decode[userDTO](nilContext())
	if err != nil {
		panic(err)
	}
	fmt.Println(user.Name)
	fmt.Println(calls.Load())

	// Output:
	// Ada
	// 2
}

// ExampleRequest_IdempotencyKey shows the safe way to retry POST requests.
// Without Idempotency-Key, POST is not retried by default.
func Example_retryPostWithIdempotencyKey() {
	var calls atomic.Int32
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		fmt.Println(r.Header.Get("Idempotency-Key"))
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writeJSON(w, http.StatusCreated, `{"id":"order_1"}`)
	})
	defer closeServer()

	client := fluent.MustNew(
		fluent.WithBaseURL(srv.URL),
		fluent.WithRetry(fluent.RetryPolicy{
			MaxAttempts: 2,
			Backoff:     fluent.ConstantBackoff(0),
		}),
	)
	err := client.Post("/orders").
		IdempotencyKey("req-123").
		JSON(map[string]string{"sku": "A1"}).
		Exec(nilContext())
	if err != nil {
		panic(err)
	}
	fmt.Println(calls.Load())

	// Output:
	// req-123
	// req-123
	// 2
}

// ExampleRequest_NoRetry disables retry for a single call even when the Client has a policy.
func Example_retryNoRetry() {
	var calls atomic.Int32
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	defer closeServer()

	client := fluent.MustNew(
		fluent.WithBaseURL(srv.URL),
		fluent.WithRetry(fluent.RetryPolicy{MaxAttempts: 3, Backoff: fluent.ConstantBackoff(0)}),
	)
	err := client.Get("/unstable").NoRetry().Exec(nilContext())
	fmt.Println(errors.Is(err, fluent.ErrStatus))
	fmt.Println(calls.Load())

	// Output:
	// true
	// 1
}

// ExampleRetryPolicy_retryAfter shows that Retry-After takes precedence over Backoff.
// The header value can be seconds or an HTTP date.
func Example_retryAfter() {
	var calls atomic.Int32
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writeJSON(w, http.StatusOK, `{}`)
	})
	defer closeServer()

	client := fluent.MustNew(
		fluent.WithBaseURL(srv.URL),
		fluent.WithRetry(fluent.RetryPolicy{
			MaxAttempts:   2,
			Backoff:       fluent.ConstantBackoff(time.Hour),
			MaxRetryAfter: time.Second,
		}),
	)
	err := client.Get("/limited").Exec(nilContext())
	fmt.Println(err == nil)
	fmt.Println(calls.Load())

	// Output:
	// true
	// 2
}

// ExampleExponentialBackoff demonstrates configuring jittered exponential backoff.
// The value is intentionally not printed exactly because it includes jitter.
func Example_retryExponentialBackoff() {
	backoff := fluent.ExponentialBackoff(100*time.Millisecond, time.Second)
	d := backoff(3)
	fmt.Println(d >= 200*time.Millisecond && d < 400*time.Millisecond)

	// Output:
	// true
}

// ExampleDefaultRetryIf documents the built-in transient failure predicate.
func Example_retryDefaultPredicate() {
	fmt.Println(fluent.DefaultRetryIf(&http.Response{StatusCode: http.StatusServiceUnavailable}, nil))
	fmt.Println(fluent.DefaultRetryIf(&http.Response{StatusCode: http.StatusBadRequest}, nil))

	// Output:
	// true
	// false
}

// ExampleRequest_AcceptStatus treats selected non-2xx statuses as successful responses.
func Example_errorAcceptStatus() {
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not found but expected"))
	})
	defer closeServer()

	client := fluent.MustNew(fluent.WithBaseURL(srv.URL))
	body, err := client.Get("/optional").
		AcceptStatus(http.StatusOK, http.StatusNotFound).
		Bytes(nilContext())
	if err != nil {
		panic(err)
	}
	fmt.Println(string(body))

	// Output:
	// not found but expected
}

// ExampleWithAcceptStatus sets a Client-wide success predicate.
// Request.AcceptStatus can still override this for a single call.
func ExampleWithAcceptStatus() {
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("queued"))
	})
	defer closeServer()

	client := fluent.MustNew(
		fluent.WithBaseURL(srv.URL),
		fluent.WithAcceptStatus(func(code int) bool {
			return code == http.StatusOK || code == http.StatusAccepted
		}),
	)
	body, err := client.Post("/jobs").JSON(map[string]string{"kind": "report"}).Bytes(nilContext())
	if err != nil {
		panic(err)
	}
	fmt.Println(string(body))

	// Output:
	// queued
}

// ExampleWithErrorDecoder converts a non-2xx JSON response into a typed business error.
func Example_errorDecoder() {
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusBadRequest, `{"code":"BAD_MODEL","message":"unknown model"}`)
	})
	defer closeServer()

	client := fluent.MustNew(
		fluent.WithBaseURL(srv.URL),
		fluent.WithErrorDecoder(func(resp *http.Response, body []byte) error {
			var se serviceError
			if err := fluent.DefaultCodec.Decode(bytesReader(body), &se); err != nil || se.Code == "" {
				return nil
			}
			return &se
		}),
	)
	err := client.Post("/chat").JSON(map[string]string{"model": "missing"}).Exec(nilContext())

	fmt.Println(errors.Is(err, fluent.ErrStatus))
	fmt.Println(fluent.StatusCode(err))
	if se, ok := errors.AsType[*serviceError](err); ok {
		fmt.Println(se.Code)
	}

	// Output:
	// true
	// 400
	// BAD_MODEL
}

// Example_errorClassify shows how application code can branch on fluent error categories.
func Example_errorClassify() {
	classify := func(err error) string {
		switch {
		case err == nil:
			return "ok"
		case errors.Is(err, fluent.ErrStatus) && fluent.StatusCode(err) == http.StatusNotFound:
			return "not-found"
		case errors.Is(err, fluent.ErrTimeout), errors.Is(err, fluent.ErrTransport):
			return "upstream-unavailable"
		default:
			return "other"
		}
	}

	notFoundSrv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	defer closeServer()

	statusClient := fluent.MustNew(fluent.WithBaseURL(notFoundSrv.URL))
	err := statusClient.Get("/users/missing").Exec(nilContext())
	fmt.Println(classify(err))

	transportClient := fluent.MustNew(fluent.WithHTTPClient(&http.Client{
		Transport: fluent.RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return nil, errors.New("connection reset")
		}),
	}))
	err = transportClient.Get("https://api.example.com/users/1").Exec(nilContext())
	fmt.Println(classify(err))

	// Output:
	// not-found
	// upstream-unavailable
}

// ExampleError_build demonstrates build-time validation errors.
func Example_errorBuild() {
	client := fluent.MustNew(fluent.WithBaseURL("https://api.example.com"))
	_, err := client.Get("/users/{id}").Decode[userDTO](nilContext())

	fmt.Println(errors.Is(err, fluent.ErrBuild))
	if fe, ok := errors.AsType[*fluent.Error](err); ok {
		fmt.Println(fe.Kind)
	}

	// Output:
	// true
	// fluent: build error
}

// ExampleError_decode demonstrates response decoding failures.
func Example_errorDecode() {
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `not-json`)
	})
	defer closeServer()

	client := fluent.MustNew(fluent.WithBaseURL(srv.URL))
	_, err := client.Get("/broken").Decode[userDTO](nilContext())

	fmt.Println(errors.Is(err, fluent.ErrDecode))
	if fe, ok := errors.AsType[*fluent.Error](err); ok {
		fmt.Println(string(fe.Body))
	}

	// Output:
	// true
	// not-json
}

// ExampleError_timeout demonstrates classifying context-driven timeout and cancellation.
func Example_errorTimeout() {
	ctx, cancel := context.WithCancel(nilContext())
	cancel()

	client := fluent.MustNew(fluent.WithBaseURL("https://api.example.com"))
	err := client.Get("/users").Exec(ctx)

	fmt.Println(errors.Is(err, fluent.ErrCanceled))
	fmt.Println(errors.Is(err, context.Canceled))

	// Output:
	// true
	// true
}
