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

func NewResponseError(response *http.Response, detail error) error {
	return ResponseError{
		Response: response,
		Detail:   detail,
	}
}
