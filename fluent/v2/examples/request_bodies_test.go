package examples

import (
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	fluent "github.com/k0923/go/fluent/v2"
)

type userDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ExampleRequest_Path_Query_Queries demonstrates safe path templating and query encoding.
// Path values are escaped with url.PathEscape, while slice query values are encoded as repeated keys.
func Example_requestPathQueryQueries() {
	type listQuery struct {
		Page  int       `query:"page"`
		Tags  []string  `query:"tag"`
		Since time.Time `query:"since,omitempty"`
		Skip  string    `query:"-"`
	}

	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		fmt.Println(r.URL.EscapedPath())
		fmt.Println(r.URL.RawQuery)
		writeJSON(w, http.StatusOK, `{}`)
	})
	defer closeServer()

	client := fluent.MustNew(fluent.WithBaseURL(srv.URL + "/api"))
	err := client.Get("/users/{id}/events").
		Path("id", "u/42").
		Query("sort", "desc").
		Queries(listQuery{
			Page: 1,
			Tags: []string{"vip", "new"},
		}).
		Exec(nilContext())
	if err != nil {
		panic(err)
	}

	// Output:
	// /api/users/u%2F42/events
	// page=1&sort=desc&tag=vip&tag=new
}

// Example_requestQueryStruct shows the common list-query shape from service clients.
// Empty omitempty fields are skipped, while slices become repeated query keys.
func Example_requestQueryStruct() {
	type listQuery struct {
		Page  int       `query:"page"`
		Size  int       `query:"size,omitempty"`
		Tags  []string  `query:"tag"`
		Since time.Time `query:"since,omitempty"`
	}

	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		fmt.Println(r.URL.RawQuery)
		writeJSON(w, http.StatusOK, `[{"id":"u1","name":"Ada"}]`)
	})
	defer closeServer()

	client := fluent.MustNew(fluent.WithBaseURL(srv.URL + "/api/v1"))
	users, err := client.Get("/users").
		Queries(listQuery{Page: 1, Tags: []string{"vip", "new"}}).
		Decode[[]userDTO](nilContext())
	if err != nil {
		panic(err)
	}
	fmt.Println(users[0].Name)

	// Output:
	// page=1&tag=vip&tag=new
	// Ada
}

// ExampleRequest_Header_Headers_Bearer_BasicAuth_Cookie shows request-level
// headers and cookies. Request-level values override or extend Client defaults.
func Example_requestHeadersAuthCookie() {
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		fmt.Println(r.Header.Get("X-Trace"))
		fmt.Println(r.Header.Get("Authorization"))
		fmt.Println(r.Header.Get("X-Extra"))
		cookie, _ := r.Cookie("session")
		fmt.Println(cookie.Value)
		writeJSON(w, http.StatusOK, `{}`)
	})
	defer closeServer()

	client := fluent.MustNew(
		fluent.WithBaseURL(srv.URL),
		fluent.WithHeader("X-Trace", "client-default"),
	)
	err := client.Get("/headers").
		Header("X-Trace", "request-value").
		Headers(http.Header{"X-Extra": {"yes"}}).
		BasicAuth("alice", "secret").
		Bearer("token-wins"). // later calls overwrite Authorization.
		Cookie(&http.Cookie{Name: "session", Value: "s1"}).
		Exec(nilContext())
	if err != nil {
		panic(err)
	}

	// Output:
	// request-value
	// Bearer token-wins
	// yes
	// s1
}

// ExampleRequest_JSON_Decode demonstrates the common JSON request/response path.
func Example_requestJSONDecode() {
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fmt.Println(r.Header.Get("Content-Type"))
		fmt.Println(string(body))
		writeJSON(w, http.StatusOK, `{"id":"u1","name":"Ada"}`)
	})
	defer closeServer()

	client := fluent.MustNew(fluent.WithBaseURL(srv.URL))
	user, err := client.Post("/users").
		JSON(userDTO{Name: "Ada"}).
		Decode[userDTO](nilContext())
	if err != nil {
		panic(err)
	}
	fmt.Printf("%s:%s\n", user.ID, user.Name)

	// Output:
	// application/json
	// {"id":"","name":"Ada"}
	// u1:Ada
}

// ExampleRequest_Into decodes into an existing variable when a caller wants to
// reuse storage or control the zero value.
func Example_requestInto() {
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"id":"u2","name":"Grace"}`)
	})
	defer closeServer()

	var dst userDTO
	client := fluent.MustNew(fluent.WithBaseURL(srv.URL))
	err := client.Get("/users/u2").Into(nilContext(), &dst)
	if err != nil {
		panic(err)
	}
	fmt.Println(dst.Name)

	// Output:
	// Grace
}

// ExampleRequest_Form sends application/x-www-form-urlencoded data.
// Struct tags use `form`, while zero values with omitempty are skipped.
func Example_requestForm() {
	type formData struct {
		Name string `form:"name"`
		Age  int    `form:"age,omitempty"`
	}

	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		fmt.Println(r.Header.Get("Content-Type"))
		fmt.Println(r.PostForm.Encode())
		writeJSON(w, http.StatusOK, `{}`)
	})
	defer closeServer()

	client := fluent.MustNew(fluent.WithBaseURL(srv.URL))
	if err := client.Post("/profiles").Form(formData{Name: "Bob"}).Exec(nilContext()); err != nil {
		panic(err)
	}

	// Output:
	// application/x-www-form-urlencoded
	// name=Bob
}

// ExampleRequest_BodyBytes sends a replayable raw byte payload.
func Example_requestBodyBytes() {
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fmt.Println(r.Header.Get("Content-Type"))
		fmt.Println(string(body))
		writeJSON(w, http.StatusOK, `{}`)
	})
	defer closeServer()

	client := fluent.MustNew(fluent.WithBaseURL(srv.URL))
	err := client.Put("/documents/1").
		BodyBytes([]byte("plain text"), "text/plain").
		Exec(nilContext())
	if err != nil {
		panic(err)
	}

	// Output:
	// text/plain
	// plain text
}

// ExampleRequest_BodyFunc uses a fresh reader for every attempt and is therefore retryable.
// Prefer BodyFunc over BodyReader when the request may be retried.
func Example_requestBodyFunc() {
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fmt.Println(string(body))
		writeJSON(w, http.StatusOK, `{}`)
	})
	defer closeServer()

	client := fluent.MustNew(fluent.WithBaseURL(srv.URL))
	err := client.Post("/upload").
		BodyFunc(func() (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("reopenable")), nil
		}, "text/plain").
		Exec(nilContext())
	if err != nil {
		panic(err)
	}

	// Output:
	// reopenable
}

// ExampleRequest_BodyReader sends a one-shot stream. It is suitable for a single attempt;
// use BodyFunc when retry is required.
func Example_requestBodyReader() {
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fmt.Println(string(body))
		writeJSON(w, http.StatusOK, `{}`)
	})
	defer closeServer()

	client := fluent.MustNew(fluent.WithBaseURL(srv.URL))
	err := client.Post("/once").
		BodyReader(strings.NewReader("one-shot"), "text/plain").
		NoRetry().
		Exec(nilContext())
	if err != nil {
		panic(err)
	}

	// Output:
	// one-shot
}

// ExampleRequest_Multipart builds multipart/form-data lazily for each attempt.
// Returning an error from the writer stops the request before sending a partial body.
func Example_requestMultipart() {
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		file, header, _ := r.FormFile("file")
		data, _ := io.ReadAll(file)
		fmt.Println(r.FormValue("folder"))
		fmt.Println(header.Filename)
		fmt.Println(string(data))
		writeJSON(w, http.StatusOK, `{}`)
	})
	defer closeServer()

	client := fluent.MustNew(fluent.WithBaseURL(srv.URL))
	err := client.Post("/files").Multipart(func(w *multipart.Writer) error {
		if err := w.WriteField("folder", "docs"); err != nil {
			return err
		}
		part, err := w.CreateFormFile("file", "readme.txt")
		if err != nil {
			return err
		}
		_, err = io.WriteString(part, "hello")
		return err
	}).Exec(nilContext())
	if err != nil {
		panic(err)
	}

	// Output:
	// docs
	// readme.txt
	// hello
}

// Example_requestMultipartRetry uploads multipart data with a retryable writer.
// The multipart callback is invoked again for each attempt, so file handles can be reopened there.
func Example_requestMultipartRetry() {
	var calls atomic.Int32
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = r.ParseMultipartForm(1 << 20)
		file, _, _ := r.FormFile("file")
		data, _ := io.ReadAll(file)
		fmt.Println(r.FormValue("folder"))
		fmt.Println(string(data))
		writeJSON(w, http.StatusCreated, `{}`)
	})
	defer closeServer()

	client := fluent.MustNew(
		fluent.WithBaseURL(srv.URL),
		fluent.WithRetry(fluent.RetryPolicy{
			MaxAttempts:        2,
			Backoff:            fluent.ConstantBackoff(0),
			RetryNonIdempotent: true,
		}),
	)
	err := client.Post("/files").Multipart(func(w *multipart.Writer) error {
		if err := w.WriteField("folder", "docs"); err != nil {
			return err
		}
		part, err := w.CreateFormFile("file", "a.txt")
		if err != nil {
			return err
		}
		_, err = io.Copy(part, strings.NewReader("hello"))
		return err
	}).Exec(nilContext())
	if err != nil {
		panic(err)
	}
	fmt.Println(calls.Load())

	// Output:
	// docs
	// hello
	// 2
}

// ExampleRequest_Do shows the low-level API. When Do succeeds, callers must close resp.Body.
func Example_requestDo() {
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-ID", "req-1")
		_, _ = io.WriteString(w, "ok")
	})
	defer closeServer()

	client := fluent.MustNew(fluent.WithBaseURL(srv.URL))
	resp, err := client.Get("/raw").Do(nilContext())
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	fmt.Println(resp.Header.Get("X-Request-ID"))
	fmt.Println(string(body))

	// Output:
	// req-1
	// ok
}

// ExampleRequest_Bytes reads the whole response body into memory.
// Use Stream for large downloads.
func Example_requestBytes() {
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "small response")
	})
	defer closeServer()

	client := fluent.MustNew(fluent.WithBaseURL(srv.URL))
	body, err := client.Get("/small").Bytes(nilContext())
	if err != nil {
		panic(err)
	}
	fmt.Println(string(body))

	// Output:
	// small response
}

// ExampleRequest_Stream copies a large response directly to an io.Writer.
func Example_requestStream() {
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "download")
	})
	defer closeServer()

	var dst strings.Builder
	client := fluent.MustNew(fluent.WithBaseURL(srv.URL))
	n, err := client.Get("/file").Stream(nilContext(), &dst)
	if err != nil {
		panic(err)
	}
	fmt.Printf("%d:%s\n", n, dst.String())

	// Output:
	// 8:download
}

// Example_requestStreamWithTimeout shows the recommended shape for long downloads.
// Timeout controls a single attempt; increase or disable it when copying large responses.
func Example_requestStreamWithTimeout() {
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "large download")
	})
	defer closeServer()

	var dst strings.Builder
	client := fluent.MustNew(fluent.WithBaseURL(srv.URL))
	n, err := client.Get("/files/{id}").
		Path("id", "file/42").
		Timeout(10*time.Minute).
		Stream(nilContext(), &dst)
	if err != nil {
		panic(err)
	}
	fmt.Printf("%d:%s\n", n, dst.String())

	// Output:
	// 14:large download
}

// ExampleRequest_Head demonstrates HEAD requests. Into/Decode intentionally skip decoding.
func Example_requestHead() {
	srv, closeServer := exampleServer(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Exists", "yes")
		w.WriteHeader(http.StatusOK)
	})
	defer closeServer()

	client := fluent.MustNew(fluent.WithBaseURL(srv.URL))
	resp, err := client.Head("/object").Do(nilContext())
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	fmt.Println(resp.Header.Get("X-Exists"))

	// Output:
	// yes
}
