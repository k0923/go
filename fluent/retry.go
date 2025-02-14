package fluent

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

type Validator func(ctx context.Context, resp *http.Response) error

func RetryByDuration(duration ...time.Duration) func() bool {
	return func() bool {
		if len(duration) > 0 {
			time.Sleep(duration[0])
			duration = duration[1:]
			return true
		}
		return false
	}
}

func RetryByCount(count int) func() bool {
	return func() bool {
		if count > 0 {
			count--
			return true
		}
		return false
	}
}

func ValidCode(code int) Validator {
	return func(ctx context.Context, resp *http.Response) error {
		if resp.StatusCode != code {
			return fmt.Errorf("resp code is %d and not equal to expected code %d", resp.StatusCode, code)
		}
		return nil
	}
}

func UseStrategy(retryMethod func() bool, validators ...Validator) RetryStrategy {
	if retryMethod == nil {
		retryMethod = RetryByCount(1)
	}
	return func(ctx context.Context, resp *http.Response, err error) (error, bool) {
		if err != nil {
			return err, retryMethod()
		}

		if resp == nil {
			return fmt.Errorf("resp is nil"), retryMethod()
		}

		if len(validators) > 0 {
			for _, validator := range validators {
				if err := validator(ctx, resp); err != nil {
					return err, retryMethod()
				}
			}
		}

		return err, false

	}
}
