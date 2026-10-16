package examples

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	fluent "github.com/k0923/go/fluent/v2"
)

// ExampleNew demonstrates the recommended way to create a reusable Client.
// A Client is immutable after construction and can be shared by many goroutines.
func Example_clientNew() {
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		fmt.Println(r.URL.Path)
		fmt.Println(r.Header.Get("User-Agent"))
		fmt.Println(r.Header.Get("X-App"))
		writeJSON(w, http.StatusOK, `{"ok":true}`)
	})
	defer closeServer()

	client, err := fluent.New(
		fluent.WithBaseURL(srv.URL+"/api/v1"),
		fluent.WithUserAgent("orders/1.0"),
		fluent.WithHeader("X-App", "checkout"),
		fluent.WithTimeout(10*time.Second),
	)
	if err != nil {
		panic(err)
	}

	if err := client.Get("/health").Exec(nilContext()); err != nil {
		panic(err)
	}

	// Output:
	// /api/v1/health
	// orders/1.0
	// checkout
}

// Example_typicalClientCall combines the common production setup: BaseURL,
// User-Agent, retry, middleware, business-error decoding, path parameters, and Decode.
func Example_typicalClientCall() {
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		fmt.Println(r.URL.EscapedPath())
		fmt.Println(r.Header.Get("User-Agent"))
		writeJSON(w, http.StatusOK, `{"id":"u/42","name":"Bob"}`)
	})
	defer closeServer()

	api := fluent.MustNew(
		fluent.WithBaseURL(srv.URL+"/api/v1"),
		fluent.WithUserAgent("order-svc/1.4"),
		fluent.WithRetry(fluent.RetryPolicy{MaxAttempts: 3, Backoff: fluent.ConstantBackoff(0)}),
		fluent.WithMiddleware(fluent.Logging(slog.New(slog.DiscardHandler))),
		fluent.WithErrorDecoder(func(resp *http.Response, body []byte) error {
			var se serviceError
			if err := fluent.DefaultCodec.Decode(bytesReader(body), &se); err != nil || se.Code == "" {
				return nil
			}
			return &se
		}),
	)

	user, err := api.Get("/users/{id}").Path("id", "u/42").Decode[userDTO](nilContext())
	if err != nil {
		panic(err)
	}
	fmt.Printf("%s:%s\n", user.ID, user.Name)

	// Output:
	// /api/v1/users/u%2F42
	// order-svc/1.4
	// u/42:Bob
}

// ExampleMustNew is useful for package-level clients in applications where
// configuration errors should fail fast during initialization.
func Example_clientMustNew() {
	client := fluent.MustNew(
		fluent.WithBaseURL("https://api.example.com/v1"),
		fluent.WithHeaders(http.Header{
			"X-App": {"billing"},
		}),
	)
	req := client.Get("/invoices/{id}").Path("id", "inv_123")

	fmt.Printf("%T\n", req)

	// Output:
	// *fluent.Request
}

// ExampleClient_With derives a child Client without mutating the parent.
// This is the preferred pattern for per-upstream or per-feature overrides.
func Example_clientWith() {
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		fmt.Println(r.Header.Get("X-Tenant"))
		fmt.Println(r.Header.Get("X-Mode"))
		writeJSON(w, http.StatusOK, `{}`)
	})
	defer closeServer()

	parent := fluent.MustNew(
		fluent.WithBaseURL(srv.URL),
		fluent.WithHeader("X-Tenant", "global"),
	)
	child, err := parent.With(fluent.WithHeader("X-Mode", "admin"))
	if err != nil {
		panic(err)
	}

	if err := child.Get("/users").Exec(nilContext()); err != nil {
		panic(err)
	}

	// Output:
	// global
	// admin
}

// ExampleWithHTTPClient shows how to pass a tuned http.Client.
// Use this for custom transports, proxies, TLS settings, or connection-pool limits.
func Example_clientHTTPClient() {
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"ok":true}`)
	})
	defer closeServer()

	httpClient := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			MaxIdleConnsPerHost: 32,
		},
	}
	client := fluent.MustNew(
		fluent.WithBaseURL(srv.URL),
		fluent.WithHTTPClient(httpClient),
	)

	err := client.Get("/ready").Exec(nilContext())
	fmt.Println(err == nil)

	// Output:
	// true
}

// ExampleDefault demonstrates package-level shortcuts for scripts and small tools.
// Services should prefer their own Client so configuration is explicit.
func Example_defaultClient() {
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"status":"ok"}`)
	})
	defer closeServer()

	body, err := fluent.Get(srv.URL + "/status").Bytes(nilContext())
	if err != nil {
		panic(err)
	}
	fmt.Println(string(body))

	// Output:
	// {"status":"ok"}
}

// Example_requestMethods covers the method helpers and the generic NewRequest entry.
// Use NewRequest for less common methods; use typed helpers for ordinary REST calls.
func Example_requestMethods() {
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		fmt.Println(r.Method)
		writeJSON(w, http.StatusOK, `{}`)
	})
	defer closeServer()

	client := fluent.MustNew(fluent.WithBaseURL(srv.URL))
	_ = client.NewRequest(http.MethodPatch, "/items/1").Exec(nilContext())
	_ = client.Patch("/items/1").Exec(nilContext())
	_ = client.Delete("/items/1").Exec(nilContext())

	// Package-level helpers are convenient for scripts when the request URL is absolute.
	_ = fluent.Post(srv.URL + "/tasks").JSON(map[string]string{"name": "cleanup"}).Exec(nilContext())
	_ = fluent.Put(srv.URL + "/tasks/1").JSON(map[string]string{"name": "cleanup"}).Exec(nilContext())
	_ = fluent.Patch(srv.URL + "/tasks/1").JSON(map[string]string{"name": "done"}).Exec(nilContext())
	_ = fluent.Delete(srv.URL + "/tasks/1").Exec(nilContext())
	_ = fluent.Head(srv.URL + "/tasks/1").Exec(nilContext())

	// Output:
	// PATCH
	// PATCH
	// DELETE
	// POST
	// PUT
	// PATCH
	// DELETE
	// HEAD
}
