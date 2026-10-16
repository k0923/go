package fluent

import (
	"net/http"
)

type ResponseError struct {
	Response *http.Response
	Detail   error
}

func (err ResponseError) Error() string {
	if err.Detail != nil {
		return err.Detail.Error()
	}
	return ""
}

// Unwrap 使 errors.Is / errors.As 能识别底层错误（如 *json.SyntaxError）。
func (err ResponseError) Unwrap() error {
	return err.Detail
}

func NewResponseError(response *http.Response, detail error) error {
	return ResponseError{
		Response: response,
		Detail:   detail,
	}
}
