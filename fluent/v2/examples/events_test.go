package examples

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	fluent "github.com/k0923/go/fluent/v2"
)

type chatChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
}

func writeSSE(w http.ResponseWriter, frames ...string) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.(http.Flusher).Flush()
	for _, frame := range frames {
		_, _ = fmt.Fprint(w, frame)
		w.(http.Flusher).Flush()
	}
}

// ExampleEventReader parses an arbitrary Server-Sent Events stream.
// It is useful for recorded fixtures or custom transports.
func ExampleEventReader() {
	stream := strings.NewReader(
		": heartbeat\n\n" +
			"id: evt-1\n" +
			"event: delta\n" +
			"data: hello\n" +
			"data: world\n\n",
	)
	reader := fluent.NewEventReader(stream, 0)

	ev, err := reader.Next()
	if err != nil {
		panic(err)
	}
	fmt.Println(ev.ID)
	fmt.Println(ev.Type)
	fmt.Println(string(ev.Data))

	// Output:
	// evt-1
	// delta
	// hello
	// world
}

// ExampleRequest_Events reads raw SSE events. This is useful for providers
// such as Anthropic where event names carry business meaning.
func Example_eventsRaw() {
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w,
			"event: message_start\ndata: {}\n\n",
			"event: ping\ndata: {}\n\n",
			"event: content_block_delta\ndata: {\"text\":\"hi\"}\n\n",
			"event: message_stop\ndata: {}\n\n",
		)
	})
	defer closeServer()

	client := fluent.MustNew(fluent.WithBaseURL(srv.URL))
	for ev, err := range client.Post("/messages").JSON(map[string]bool{"stream": true}).Events(nilContext()) {
		if err != nil {
			panic(err)
		}
		if fluent.SkipType("ping")(ev) {
			continue
		}
		fmt.Println(ev.Type)
	}

	// Output:
	// message_start
	// content_block_delta
	// message_stop
}

// Example_eventsAnthropicDispatch uses event names for provider-specific routing.
func Example_eventsAnthropicDispatch() {
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w,
			"event: content_block_delta\ndata: {\"text\":\"hi\"}\n\n",
			"event: message_stop\ndata: {}\n\n",
		)
	})
	defer closeServer()

	client := fluent.MustNew(fluent.WithBaseURL(srv.URL))
	for ev, err := range client.Post("/messages").JSON(map[string]bool{"stream": true}).Events(nilContext()) {
		if err != nil {
			panic(err)
		}
		switch ev.Type {
		case "content_block_delta":
			fmt.Println(string(ev.Data))
		case "message_stop":
			fmt.Println("done")
			return
		}
	}

	// Output:
	// {"text":"hi"}
	// done
}

// ExampleRequest_EventsOf decodes each event's data field as JSON and skips sentinel events.
// This mirrors the OpenAI-compatible streaming API shape.
func Example_eventsOfJSON() {
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		fmt.Println(r.Header.Get("Accept"))
		writeSSE(w,
			`data: {"choices":[{"delta":{"content":"Hel"}}]}`+"\n\n",
			`data: {"choices":[{"delta":{"content":"lo"}}]}`+"\n\n",
			"data: [DONE]\n\n",
		)
	})
	defer closeServer()

	client := fluent.MustNew(fluent.WithBaseURL(srv.URL))
	var text strings.Builder
	for chunk, err := range client.Post("/chat/completions").
		JSON(map[string]any{"model": "demo", "stream": true}).
		EventsOf[chatChunk](nilContext(), fluent.SkipData("[DONE]")) {
		if err != nil {
			panic(err)
		}
		text.WriteString(chunk.Choices[0].Delta.Content)
	}
	fmt.Println(text.String())

	// Output:
	// text/event-stream
	// Hello
}

// Example_eventsRetryBeforeConnected shows that Events reuses Do semantics before
// the stream is established: retries, Retry-After, status errors, and ErrorDecoder apply.
func Example_eventsRetryBeforeConnected() {
	var calls atomic.Int32
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			writeJSON(w, http.StatusTooManyRequests, `{"code":"RATE_LIMIT","message":"slow down"}`)
			return
		}
		writeJSON(w, http.StatusBadRequest, `{"code":"BAD_REQUEST","message":"bad stream request"}`)
	})
	defer closeServer()

	client := fluent.MustNew(
		fluent.WithBaseURL(srv.URL),
		fluent.WithRetry(fluent.RetryPolicy{MaxAttempts: 2, Backoff: fluent.ConstantBackoff(0)}),
		fluent.WithErrorDecoder(func(resp *http.Response, body []byte) error {
			var se serviceError
			if err := fluent.DefaultCodec.Decode(bytesReader(body), &se); err != nil || se.Code == "" {
				return nil
			}
			return &se
		}),
	)

	var last error
	for _, err := range client.Post("/chat/completions").
		IdempotencyKey("req-1").
		JSON(map[string]bool{"stream": true}).
		Events(nilContext()) {
		last = err
	}
	fmt.Println(errors.Is(last, fluent.ErrStatus))
	fmt.Println(fluent.StatusCode(last))
	if se, ok := errors.AsType[*serviceError](last); ok {
		fmt.Println(se.Code)
	}
	fmt.Println(calls.Load())

	// Output:
	// true
	// 400
	// BAD_REQUEST
	// 2
}

// ExampleWithIdleTimeout configures the maximum silence allowed after a stream
// has been established. Total stream lifetime should still be controlled by ctx.
func ExampleWithIdleTimeout() {
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, "data: first\n\n")
		<-r.Context().Done()
	})
	defer closeServer()

	client := fluent.MustNew(
		fluent.WithBaseURL(srv.URL),
		fluent.WithIdleTimeout(time.Millisecond),
	)
	var last error
	for _, err := range client.Get("/stream").Events(nilContext()) {
		if err != nil {
			last = err
			break
		}
	}
	fmt.Println(errors.Is(last, fluent.ErrTimeout))

	// Output:
	// true
}

// ExampleRequest_IdleTimeout overrides the stream idle timeout for one request.
func Example_requestIdleTimeout() {
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, "data: first\n\n")
		<-r.Context().Done()
	})
	defer closeServer()

	client := fluent.MustNew(fluent.WithBaseURL(srv.URL), fluent.WithIdleTimeout(0))
	var last error
	for _, err := range client.Get("/stream").IdleTimeout(time.Millisecond).Events(nilContext()) {
		if err != nil {
			last = err
			break
		}
	}
	fmt.Println(errors.Is(last, fluent.ErrTimeout))

	// Output:
	// true
}

// ExampleWithMaxEventSize limits the memory a single event can consume.
func ExampleWithMaxEventSize() {
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, "data: "+strings.Repeat("x", 64)+"\n\n")
	})
	defer closeServer()

	client := fluent.MustNew(
		fluent.WithBaseURL(srv.URL),
		fluent.WithMaxEventSize(16),
	)
	var last error
	for _, err := range client.Get("/stream").Events(nilContext()) {
		last = err
	}
	fmt.Println(errors.Is(last, fluent.ErrEventTooLarge))

	// Output:
	// true
}

// ExampleEventsOf decodes a custom event iterator. Use this when events are
// transformed or filtered before decoding.
func ExampleEventsOf() {
	events := func(yield func(fluent.Event, error) bool) {
		yield(fluent.Event{Type: "message", Data: []byte(`{"id":"u1","name":"Ada"}`)}, nil)
	}

	for user, err := range fluent.EventsOf[userDTO](events, fluent.DefaultCodec, nil) {
		if err != nil {
			panic(err)
		}
		fmt.Println(user.Name)
	}

	// Output:
	// Ada
}
