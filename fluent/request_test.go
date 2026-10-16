package fluent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// countingBody 记录响应体是否被关闭，用于检测连接泄漏。
type countingBody struct {
	io.Reader
	closed *atomic.Int32
}

func (b countingBody) Close() error {
	b.closed.Add(1)
	return nil
}

type stubTransport struct {
	codes  []int
	calls  atomic.Int32
	closed atomic.Int32
}

func (s *stubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
	}
	n := int(s.calls.Add(1)) - 1
	code := s.codes[min(n, len(s.codes)-1)]
	return &http.Response{
		StatusCode: code,
		Header:     http.Header{},
		Body:       countingBody{strings.NewReader(`{"ok":true}`), &s.closed},
		Request:    r,
	}, nil
}

func newServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	srv := httptest.NewTestServer(t, h)
	t.Cleanup(srv.Close)
	// 内存网络下 URL 在首次调用 Client() 时才会被设置。
	srv.Client()
	return srv
}

func TestGet(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query()["tag"]; len(got) != 2 || got[0] != "a" || got[1] != "b" {
			t.Errorf("tag query = %v", got)
		}
		if got := r.URL.Query().Get("id"); got != "7" {
			t.Errorf("id query = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tk" {
			t.Errorf("Authorization = %q", got)
		}
		if c, err := r.Cookie("sid"); err != nil || c.Value != "s1" {
			t.Errorf("cookie sid = %v, %v", c, err)
		}
		_, _ = io.WriteString(w, `{"message":"pong"}`)
	})
	var result struct{ Message string }
	err := Get(srv.URL+"/ping").
		Client(srv.Client()).
		Query("id", 7).
		QueryArray("tag", []string{"a", "b"}).
		Bearer("tk").
		Cookie("sid", "s1").
		Retry(UseStrategy(RetryByCount(1), ValidCode(http.StatusOK))).
		JsonResult(&result).
		Send()
	if err != nil {
		t.Fatal(err)
	}
	if result.Message != "pong" {
		t.Fatalf("result = %+v", result)
	}
}

func TestOverrideClient(t *testing.T) {
	var calls atomic.Int32
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if got := r.Header.Get("X-From"); got != "client" {
			t.Errorf("X-From = %q", got)
		}
		_, _ = io.WriteString(w, `{}`)
	})
	client := NewClient(&RequestOptions{
		Headers:       map[string]string{"X-From": "client"},
		Client:        srv.Client(),
		RetryStrategy: UseStrategy(RetryByCount(2), ValidCode(http.StatusOK)),
	})
	var result map[string]any
	if err := client.Get(srv.URL).JsonResult(&result).Send(); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("calls = %d, want 3", got)
	}
}

func TestNewClientNilOptions(t *testing.T) {
	c := NewClient(nil)
	if c.Get("http://example.com") == nil {
		t.Fatal("nil request")
	}
}

func TestDefaultClientDeprecatedSetters(t *testing.T) {
	c := &client{RequestOptions: NewRequestOptions()}
	c.Header("a", "b")
	c.Headers(map[string]int{"n": 1})
	if c.RequestOptions.Headers["a"] != "b" || c.RequestOptions.Headers["n"] != "1" {
		t.Fatalf("headers = %v", c.RequestOptions.Headers)
	}
	if DefaultClient.RequestOptions.Headers == nil {
		t.Fatal("DefaultClient headers must be initialized")
	}
}

func TestRetryClosesDiscardedResponses(t *testing.T) {
	rt := &stubTransport{codes: []int{500}}
	err := Get("http://stub").
		Client(&http.Client{Transport: rt}).
		Retry(UseStrategy(RetryByCount(2), ValidCode(http.StatusOK))).
		JsonResult(&struct{}{}).
		Send()
	if err == nil {
		t.Fatal("want validation error")
	}
	if calls, closed := rt.calls.Load(), rt.closed.Load(); calls != 3 || closed != calls {
		t.Fatalf("calls=%d closed=%d, every response must be closed", calls, closed)
	}
}

func TestSendWithoutHandlerClosesBody(t *testing.T) {
	rt := &stubTransport{codes: []int{200}}
	if err := Get("http://stub").Client(&http.Client{Transport: rt}).Send(); err != nil {
		t.Fatal(err)
	}
	if rt.closed.Load() != 1 {
		t.Fatal("body not closed")
	}
}

func TestMaxRetryTimesBoundsAttempts(t *testing.T) {
	old := MaxRetryTimes
	MaxRetryTimes = 2
	t.Cleanup(func() { MaxRetryTimes = old })
	rt := &stubTransport{codes: []int{500}}
	err := Get("http://stub").
		Client(&http.Client{Transport: rt}).
		Retry(UseStrategy(RetryByCount(10), ValidCode(http.StatusOK))).
		Send()
	if err == nil {
		t.Fatal("validation error must be reported after budget is exhausted")
	}
	if got := rt.calls.Load(); got != 3 {
		t.Fatalf("calls = %d, want 1 + MaxRetryTimes", got)
	}
}

func TestRetryByDurationUsesFakeClock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rt := &stubTransport{codes: []int{500, 500, 200}}
		start := time.Now()
		err := Get("http://stub").
			Client(&http.Client{Transport: rt}).
			Retry(UseStrategy(RetryByDuration(time.Second, 2*time.Second), ValidCode(http.StatusOK))).
			Send()
		if err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed != 3*time.Second {
			t.Fatalf("elapsed = %v, want 3s", elapsed)
		}
	})
}

func TestRetryStopsOnContextCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rt := &stubTransport{codes: []int{500}}
		ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
		defer cancel()
		start := time.Now()
		err := Get("http://stub").
			Context(ctx).
			Client(&http.Client{Transport: rt}).
			Retry(UseStrategy(RetryByDuration(time.Second, time.Hour), ValidCode(http.StatusOK))).
			Send()
		if err == nil {
			t.Fatal("want error")
		}
		if elapsed := time.Since(start); elapsed != 1500*time.Millisecond {
			t.Fatalf("elapsed = %v, retry sleep must stop at ctx deadline", elapsed)
		}
	})
}

func TestRetryByBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rt := &stubTransport{codes: []int{500}}
		start := time.Now()
		_ = Get("http://stub").
			Client(&http.Client{Transport: rt}).
			Retry(UseStrategy(RetryByBackoff(100*time.Millisecond, 300*time.Millisecond, 3), ValidCode(http.StatusOK))).
			Send()
		if got := rt.calls.Load(); got != 4 {
			t.Fatalf("calls = %d, want 4", got)
		}
		// 100ms+150ms 抖动上限、200ms+300ms 截断、300ms 截断。
		if elapsed := time.Since(start); elapsed < 600*time.Millisecond || elapsed > 900*time.Millisecond {
			t.Fatalf("elapsed = %v", elapsed)
		}
	})
}

func TestFileBody(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
			return
		}
		if got := r.FormValue("k"); got != "v" {
			t.Errorf("form k = %q", got)
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			t.Errorf("form file: %v", err)
			return
		}
		data, _ := io.ReadAll(f)
		_, _ = w.Write(data)
	})
	var out strings.Builder
	err := Post(srv.URL).
		Client(srv.Client()).
		Form("k", "v").
		FileBody(FileBody{
			FileName:       "a.txt",
			FileFieldName:  "file",
			ReaderSupplier: func() (io.Reader, error) { return strings.NewReader("hello"), nil },
		}).
		FileResult(&out).
		Send()
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "hello" {
		t.Fatalf("echo = %q", out.String())
	}
}

func TestFileBodySupplierErrorIsReturned(t *testing.T) {
	want := errors.New("open failed")
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
	})
	err := Post(srv.URL).
		Client(srv.Client()).
		FileBody(FileBody{
			FileName:       "a.txt",
			FileFieldName:  "file",
			ReaderSupplier: func() (io.Reader, error) { return nil, want },
		}).
		Send()
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestFormBody(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("Content-Type = %q", ct)
		}
		_ = r.ParseForm()
		if r.PostForm.Get("name") != "bob" || r.PostForm.Get("age") != "3" {
			t.Errorf("form = %v", r.PostForm)
		}
	})
	type form struct {
		Name string `form:"name"`
		Age  int    `form:"age"`
	}
	if err := Post(srv.URL).Client(srv.Client()).Forms(form{"bob", 3}).FormBody().Send(); err != nil {
		t.Fatal(err)
	}
}

func TestJsonResultErrorUnwraps(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "not json")
	})
	var v map[string]any
	err := Get(srv.URL).Client(srv.Client()).JsonResult(&v).Send()
	var respErr ResponseError
	if !errors.As(err, &respErr) {
		t.Fatalf("err = %T", err)
	}
	if _, ok := errors.AsType[*json.SyntaxError](err); !ok {
		t.Fatalf("ResponseError must unwrap to *json.SyntaxError, got %v", err)
	}
	body, _ := io.ReadAll(respErr.Response.Body)
	if string(body) != "not json" {
		t.Fatalf("body = %q, raw body must stay readable", body)
	}
}

func TestQueryArrayCopiesInput(t *testing.T) {
	values := []string{"1", "2"}
	req := Get("http://example.com").QueryArray("a", values)
	values[0] = "mutated"
	built, err := req.Build()
	if err != nil {
		t.Fatal(err)
	}
	if got := built.URL.Query()["a"]; got[0] != "1" {
		t.Fatalf("query = %v", got)
	}
}

func TestRequestIsSafeForConcurrentSend(t *testing.T) {
	rt := &stubTransport{codes: []int{200}}
	strategy := UseStrategy(nil, ValidCode(http.StatusOK))
	req := Post("http://stub").
		Client(&http.Client{Transport: rt}).
		JsonBody(map[string]int{"a": 1}).
		Retry(strategy)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if err := req.Send(); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if rt.calls.Load() != 16 {
		t.Fatalf("calls = %d", rt.calls.Load())
	}
}

func TestBuildDoesNotMutateRequest(t *testing.T) {
	req := Get("http://example.com").(*request)
	req.RequestOptions.Client = nil
	if _, err := req.Build(); err != nil {
		t.Fatal(err)
	}
	if req.ctx != nil || req.RequestOptions.Client != nil {
		t.Fatal("Build must be read-only")
	}
}
