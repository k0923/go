package fluent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

type user struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *apiError) Error() string { return e.Code + ": " + e.Message }

func newTestClient(t *testing.T, h http.HandlerFunc, opts ...Option) *Client {
	t.Helper()
	srv := httptest.NewTestServer(t, h)
	t.Cleanup(srv.Close)
	hc := srv.Client() // 内存网络下 URL 在首次调用 Client() 后才设置
	c, err := New(append([]Option{WithBaseURL(srv.URL + "/v1"), WithHTTPClient(hc)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDecodeWithPathQueryAndHeaders(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/v1/users/a%2Fb" {
			t.Errorf("path = %q", r.URL.EscapedPath())
		}
		if got := r.URL.Query()["tag"]; len(got) != 2 {
			t.Errorf("tag = %v", got)
		}
		if r.Header.Get("X-App") != "demo" || r.Header.Get("Authorization") != "Bearer tk" {
			t.Errorf("header = %v", r.Header)
		}
		fmt.Fprint(w, `{"id":"a/b","name":"bob"}`)
	}, WithHeader("X-App", "demo"))

	u, err := c.Get("/users/{id}").
		Path("id", "a/b").
		Query("tag", "x", "y").
		Bearer("tk").
		Decode[user](t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if u.Name != "bob" {
		t.Fatalf("user = %+v", u)
	}
}

func TestPathTemplateErrors(t *testing.T) {
	c := MustNew(WithBaseURL("http://example.com"))
	for _, req := range []*Request{
		c.Get("/users/{id}"),
		c.Get("/users").Path("id", "1"),
		c.Get("/users/{id").Path("id", "1"),
		MustNew().Get("/relative"),
	} {
		err := req.Exec(t.Context())
		if !errors.Is(err, ErrBuild) {
			t.Errorf("err = %v, want ErrBuild", err)
		}
	}
}

func TestStatusErrorWithBusinessDecoder(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"code":"USER_NOT_FOUND","message":"no such user"}`)
	}, WithErrorDecoder(func(resp *http.Response, body []byte) error {
		var e apiError
		if err := DefaultCodec.Decode(strings.NewReader(string(body)), &e); err != nil || e.Code == "" {
			return nil
		}
		return &e
	}))

	_, err := c.Get("/users/{id}").Path("id", "1").Decode[user](t.Context())
	if !errors.Is(err, ErrStatus) {
		t.Fatalf("err = %v", err)
	}
	if StatusCode(err) != http.StatusNotFound {
		t.Fatalf("status = %d", StatusCode(err))
	}
	biz, ok := errors.AsType[*apiError](err)
	if !ok || biz.Code != "USER_NOT_FOUND" {
		t.Fatalf("business error = %v", biz)
	}
	fe, _ := errors.AsType[*Error](err)
	if !strings.Contains(string(fe.Body), "USER_NOT_FOUND") || !strings.HasSuffix(fe.URL, "/v1/users/1") {
		t.Fatalf("Error = %+v", fe)
	}
}

func TestAcceptStatus(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	resp, err := c.Get("/x").AcceptStatus(http.StatusOK, http.StatusNotFound).Do(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestDecodeError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `not json`)
	})
	_, err := c.Get("/x").Decode[user](t.Context())
	if !errors.Is(err, ErrDecode) {
		t.Fatalf("err = %v", err)
	}
	fe, _ := errors.AsType[*Error](err)
	if string(fe.Body) != "not json" || fe.StatusCode != 200 {
		t.Fatalf("Error = %+v", fe)
	}
}

func TestNoContent(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	if _, err := c.Delete("/users/{id}").Path("id", "1").Decode[user](t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultCodecIsV1Compatible(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"ID":"1","NAME":"bob"}`)
	})
	u, err := c.Get("/x").Decode[user](t.Context())
	if err != nil || u.Name != "bob" {
		t.Fatalf("u=%+v err=%v, default codec must match case-insensitively like v1", u, err)
	}
	strict, _ := c.With(WithCodec(JSON()))
	u, err = strict.Get("/x").Decode[user](t.Context())
	if err != nil || u.Name != "" {
		t.Fatalf("u=%+v err=%v, JSON() must be case-sensitive", u, err)
	}
}

type flaky struct {
	codes  []int
	header http.Header
	calls  atomic.Int32
	bodies []string
	mu     sync.Mutex
}

func (f *flaky) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.bodies = append(f.bodies, string(b))
		f.mu.Unlock()
		n := int(f.calls.Add(1)) - 1
		code := f.codes[min(n, len(f.codes)-1)]
		maps.Copy(w.Header(), f.header)
		w.WriteHeader(code)
		fmt.Fprint(w, `{}`)
	}
}

func TestRetryReplaysJSONBody(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &flaky{codes: []int{503, 502, 200}}
		c := newTestClient(t, f.handler(), WithRetry(RetryPolicy{MaxAttempts: 3, Backoff: ConstantBackoff(time.Second)}))
		start := time.Now()
		err := c.Put("/users/1").JSON(user{ID: "1"}).Exec(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if f.calls.Load() != 3 || time.Since(start) != 2*time.Second {
			t.Fatalf("calls=%d elapsed=%v", f.calls.Load(), time.Since(start))
		}
		for _, b := range f.bodies {
			if b != `{"id":"1","name":""}` {
				t.Fatalf("replayed body = %q", b)
			}
		}
	})
}

func TestRetryExhaustedReportsAttempts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &flaky{codes: []int{503}}
		c := newTestClient(t, f.handler(), WithRetry(RetryPolicy{MaxAttempts: 3}))
		err := c.Get("/x").Exec(t.Context())
		fe, ok := errors.AsType[*Error](err)
		if !ok || fe.Kind != ErrStatus || fe.Attempts != 3 || fe.StatusCode != 503 {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestPostIsNotRetriedUnlessIdempotent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &flaky{codes: []int{503, 503, 200}}
		c := newTestClient(t, f.handler(), WithRetry(RetryPolicy{MaxAttempts: 3}))
		if err := c.Post("/orders").JSON(map[string]int{"n": 1}).Exec(t.Context()); !errors.Is(err, ErrStatus) {
			t.Fatalf("err = %v", err)
		}
		if f.calls.Load() != 1 {
			t.Fatalf("POST must not be retried, calls=%d", f.calls.Load())
		}
		if err := c.Post("/orders").IdempotencyKey("k1").JSON(map[string]int{"n": 1}).Exec(t.Context()); err != nil {
			t.Fatal(err)
		}
		if f.calls.Load() != 3 {
			t.Fatalf("calls=%d", f.calls.Load())
		}
	})
}

func TestRetryAfter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &flaky{codes: []int{429, 200}, header: http.Header{"Retry-After": {"7"}}}
		c := newTestClient(t, f.handler(), WithRetry(RetryPolicy{MaxAttempts: 2}))
		start := time.Now()
		if err := c.Get("/x").Exec(t.Context()); err != nil {
			t.Fatal(err)
		}
		if time.Since(start) != 7*time.Second {
			t.Fatalf("elapsed = %v, want Retry-After 7s", time.Since(start))
		}

		f2 := &flaky{codes: []int{429, 200}, header: http.Header{"Retry-After": {"3600"}}}
		c2 := newTestClient(t, f2.handler(), WithRetry(RetryPolicy{MaxAttempts: 2}))
		if err := c2.Get("/x").Exec(t.Context()); !errors.Is(err, ErrStatus) || f2.calls.Load() != 1 {
			t.Fatalf("Retry-After above limit must stop retrying: err=%v calls=%d", err, f2.calls.Load())
		}
	})
}

func TestCancelDuringBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &flaky{codes: []int{503}}
		c := newTestClient(t, f.handler(), WithRetry(RetryPolicy{MaxAttempts: 5, Backoff: ConstantBackoff(time.Hour)}))
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		err := c.Get("/x").Exec(ctx)
		if !errors.Is(err, ErrTimeout) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v", err)
		}
		ctx2, cancel2 := context.WithCancel(t.Context())
		cancel2()
		if err := c.Get("/x").Exec(ctx2); !errors.Is(err, ErrCanceled) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestPerAttemptTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				<-r.Context().Done()
				return
			}
			fmt.Fprint(w, `{}`)
		}, WithTimeout(time.Second), WithRetry(RetryPolicy{MaxAttempts: 2, Backoff: ConstantBackoff(0)}))
		if err := c.Get("/x").Exec(t.Context()); err != nil {
			t.Fatalf("second attempt must succeed after first times out: %v", err)
		}
		_, err := c.Get("/x").Timeout(time.Second).NoRetry().Do(t.Context())
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestTimeoutKind(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}, WithTimeout(time.Second))
		if err := c.Get("/x").Exec(t.Context()); !errors.Is(err, ErrTimeout) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestMultipartRetryAndError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if calls.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			f, _, _ := r.FormFile("file")
			io.Copy(w, f)
		}, WithRetry(RetryPolicy{MaxAttempts: 2, RetryNonIdempotent: true}))

		data, err := c.Post("/upload").Multipart(func(w *multipart.Writer) error {
			_ = w.WriteField("k", "v")
			part, err := w.CreateFormFile("file", "a.txt")
			if err != nil {
				return err
			}
			_, err = io.WriteString(part, "hello")
			return err
		}).Bytes(t.Context())
		if err != nil || string(data) != "hello" || calls.Load() != 2 {
			t.Fatalf("data=%q err=%v calls=%d", data, err, calls.Load())
		}

		want := errors.New("disk read failed")
		err = c.Post("/upload").Multipart(func(*multipart.Writer) error { return want }).Exec(t.Context())
		if !errors.Is(err, ErrBuild) || !errors.Is(err, want) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestBodyReaderIsNotRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &flaky{codes: []int{503, 200}}
		c := newTestClient(t, f.handler(), WithRetry(RetryPolicy{MaxAttempts: 3}))
		err := c.Put("/x").BodyReader(strings.NewReader("once"), "text/plain").Exec(t.Context())
		if !errors.Is(err, ErrStatus) || f.calls.Load() != 1 {
			t.Fatalf("err=%v calls=%d", err, f.calls.Load())
		}
	})
}

type listQuery struct {
	Page    int       `query:"page"`
	Size    int       `query:"size,omitempty"`
	Tags    []string  `query:"tag"`
	Since   time.Time `query:"since,omitempty"`
	Secret  string    `query:"-"`
	private string
	paging
}

type paging struct {
	Cursor string `query:"cursor,omitempty"`
}

func TestQueriesAndForm(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("page") != "0" || q.Has("size") || len(q["tag"]) != 2 || q.Has("Secret") || q.Get("cursor") != "c1" {
			t.Errorf("query = %v", q)
		}
		_ = r.ParseForm()
		if r.PostForm.Get("name") != "bob" || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("form = %v", r.PostForm)
		}
	})
	q := listQuery{Tags: []string{"a", "b"}, Secret: "s", private: "p", paging: paging{Cursor: "c1"}}
	err := c.Post("/search").
		Queries(q).
		Form(map[string]string{"name": "bob"}).
		Exec(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Get("/x").Queries(42).Exec(t.Context()); !errors.Is(err, ErrBuild) {
		t.Fatalf("err = %v", err)
	}
}

func TestMiddlewareOrderAndAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var trace []string
		mw := func(name string) Middleware {
			return func(next http.RoundTripper) http.RoundTripper {
				return RoundTripperFunc(func(r *http.Request) (*http.Response, error) {
					mu.Lock()
					trace = append(trace, fmt.Sprintf("%s#%d", name, AttemptFromContext(r.Context())))
					mu.Unlock()
					return next.RoundTrip(r)
				})
			}
		}
		f := &flaky{codes: []int{503, 200}}
		c := newTestClient(t, f.handler(),
			WithRetry(RetryPolicy{MaxAttempts: 2, Backoff: ConstantBackoff(0)}),
			WithMiddleware(mw("outer"), mw("inner")),
			WithMiddleware(Header("X-Token", func(*http.Request) (string, error) { return "t", nil })),
			WithMiddleware(Logging(slog.New(slog.DiscardHandler))),
		)
		if err := c.Get("/x").Exec(t.Context()); err != nil {
			t.Fatal(err)
		}
		want := "outer#1 inner#1 outer#2 inner#2"
		if got := strings.Join(trace, " "); got != want {
			t.Fatalf("trace = %q, want %q", got, want)
		}
	})
}

func TestWithDoesNotAffectParent(t *testing.T) {
	parent := MustNew(WithBaseURL("http://a.example/v1"), WithHeader("X-A", "1"))
	child, err := parent.With(WithHeader("X-B", "2"), WithBaseURL("http://b.example"))
	if err != nil {
		t.Fatal(err)
	}
	if parent.header.Get("X-B") != "" || parent.base.Host != "a.example" {
		t.Fatal("parent mutated")
	}
	if child.header.Get("X-A") != "1" || child.base.Host != "b.example" {
		t.Fatal("child must inherit and override")
	}
	if _, err := New(WithBaseURL("not-absolute")); !errors.Is(err, ErrBuild) {
		t.Fatalf("err = %v", err)
	}
}

func TestConcurrentDoOnSharedRequest(t *testing.T) {
	var calls atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		w.Write(b)
	})
	req := c.Put("/echo").JSON(user{ID: "1"}).Header("X-K", "v")
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			u, err := req.Decode[user](t.Context())
			if err != nil || u.ID != "1" {
				t.Error(u, err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 32 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestStream(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, strings.Repeat("x", 10000))
	})
	var sb strings.Builder
	n, err := c.Get("/file").Stream(t.Context(), &sb)
	if err != nil || n != 10000 || sb.Len() != 10000 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestErrorMessage(t *testing.T) {
	e := &Error{Kind: ErrStatus, Method: "GET", URL: "http://x/a", StatusCode: 503, Attempts: 3}
	want := "fluent: GET http://x/a: status 503 Service Unavailable (after 3 attempts)"
	if e.Error() != want {
		t.Fatalf("got %q", e.Error())
	}
	if StatusCode(errors.New("x")) != 0 || Kind(99).String() != "kind(99)" {
		t.Fatal("helpers")
	}
}

func TestExponentialBackoffBounds(t *testing.T) {
	b := ExponentialBackoff(100*time.Millisecond, time.Second)
	for attempt, hi := range map[int]time.Duration{1: 100 * time.Millisecond, 2: 200 * time.Millisecond, 10: time.Second} {
		for range 50 {
			if d := b(attempt); d < hi/2 || d >= hi {
				t.Fatalf("attempt %d: %v not in [%v,%v)", attempt, d, hi/2, hi)
			}
		}
	}
}

func BenchmarkDecode(b *testing.B) {
	srv := httptest.NewTestServer(b, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"1","name":"bob"}`)
	}))
	defer srv.Close()
	c := MustNew(WithHTTPClient(srv.Client()), WithBaseURL(srv.URL))
	ctx := context.Background()
	for b.Loop() {
		if _, err := c.Get("/users/{id}").Path("id", "1").Query("v", "1").Decode[user](ctx); err != nil {
			b.Fatal(err)
		}
	}
}
