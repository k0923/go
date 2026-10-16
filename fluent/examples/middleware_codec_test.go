package examples

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	fluent "github.com/k0923/go/fluent"
)

// Example_middlewareAttempt shows that middleware runs for every attempt.
// AttemptFromContext is useful for structured logs and metrics labels.
func Example_middlewareAttempt() {
	attemptLogger := func(next http.RoundTripper) http.RoundTripper {
		return fluent.RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			fmt.Printf("attempt=%d\n", fluent.AttemptFromContext(req.Context()))
			return next.RoundTrip(req)
		})
	}
	var calls int
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, http.StatusOK, `{}`)
	})
	defer closeServer()

	client := fluent.MustNew(
		fluent.WithBaseURL(srv.URL),
		fluent.WithMiddleware(attemptLogger),
		fluent.WithRetry(fluent.RetryPolicy{
			MaxAttempts: 2,
			Backoff:     fluent.ConstantBackoff(0),
		}),
	)
	if err := client.Get("/work").Exec(nilContext()); err != nil {
		panic(err)
	}

	// Output:
	// attempt=1
	// attempt=2
}

// Example_middlewareHeader computes a request header at send time.
// Use this for rotating tokens or per-request signatures.
func Example_middlewareHeader() {
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		fmt.Println(r.Header.Get("X-Signature"))
		writeJSON(w, http.StatusOK, `{}`)
	})
	defer closeServer()

	client := fluent.MustNew(
		fluent.WithBaseURL(srv.URL),
		fluent.WithMiddleware(fluent.Header("X-Signature", func(req *http.Request) (string, error) {
			return "signed:" + req.URL.Path, nil
		})),
	)
	if err := client.Get("/signed").Exec(nilContext()); err != nil {
		panic(err)
	}

	// Output:
	// signed:/signed
}

// Example_middlewareLogging shows the built-in slog middleware.
// This example uses slog.DiscardHandler to keep test output stable; applications should pass a real logger.
func Example_middlewareLogging() {
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{}`)
	})
	defer closeServer()

	client := fluent.MustNew(
		fluent.WithBaseURL(srv.URL),
		fluent.WithMiddleware(fluent.Logging(slog.New(slog.DiscardHandler))),
	)
	err := client.Get("/logged").Exec(nilContext())
	fmt.Println(err == nil)

	// Output:
	// true
}

// ExampleRoundTripperFunc builds a fully in-memory client without httptest.
// This is the lightest way to test code that depends on fluent.
func ExampleRoundTripperFunc() {
	httpClient := &http.Client{
		Transport: fluent.RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"id":"u1","name":"Mock"}`)),
				Request:    req,
			}, nil
		}),
	}
	client := fluent.MustNew(fluent.WithHTTPClient(httpClient))

	user, err := client.Get("https://example.test/users/u1").Decode[userDTO](nilContext())
	if err != nil {
		panic(err)
	}
	fmt.Println(user.Name)

	// Output:
	// Mock
}

// ExampleJSON_strict shows that the default JSON codec is case-sensitive.
// This avoids accidentally binding fields with misspelled or differently cased names.
func ExampleJSON_strict() {
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"ID":"u1","NAME":"Upper"}`)
	})
	defer closeServer()

	client := fluent.MustNew(fluent.WithBaseURL(srv.URL))

	value, err := client.Get("/user").Decode[userDTO](nilContext())
	if err != nil {
		panic(err)
	}

	fmt.Printf("%q\n", value.Name)

	// Output:
	// ""
}

type plainTextCodec struct{}

func (plainTextCodec) ContentType() string { return "text/plain" }

func (plainTextCodec) Marshal(v any) ([]byte, error) {
	return []byte(fmt.Sprint(v)), nil
}

func (plainTextCodec) Decode(r io.Reader, v any) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	switch out := v.(type) {
	case *string:
		*out = string(data)
		return nil
	default:
		return fmt.Errorf("plainTextCodec can decode only *string, got %T", v)
	}
}

// ExampleWithCodec_custom demonstrates a custom Codec for non-JSON APIs.
func ExampleWithCodec_custom() {
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fmt.Println(r.Header.Get("Content-Type"))
		_, _ = fmt.Fprintf(w, "echo:%s", body)
	})
	defer closeServer()

	client := fluent.MustNew(
		fluent.WithBaseURL(srv.URL),
		fluent.WithCodec(plainTextCodec{}),
	)
	text, err := client.Post("/echo").JSON("hello").Decode[string](nilContext())
	if err != nil {
		panic(err)
	}
	fmt.Println(text)

	// Output:
	// text/plain
	// echo:hello
}
