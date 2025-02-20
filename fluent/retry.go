package fluent

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

type Validator func(ctx context.Context, resp *http.Response) error

type controller func() bool

type controllerBuilder func() controller

func RetryByDuration(duration ...time.Duration) controllerBuilder {
	return func() controller {
		count := 0
		return func() bool {
			if count >= len(duration) {
				return false
			}
			time.Sleep(duration[count])
			count++
			return true
		}
	}
}

func RetryByCount(count int) controllerBuilder {
	return func() controller {
		newCount := count
		return func() bool {
			if newCount > 0 {
				newCount--
				return true
			}
			return false
		}
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

func UseStrategy(builder controllerBuilder, validators ...Validator) RetryStrategy {
	return func() RetryHandler {
		if builder == nil {
			builder = RetryByCount(1)
		}
		controller := builder()
		return func(ctx context.Context, resp *http.Response, err error) (error, bool) {
			if err != nil {
				return err, controller()
			}

			if resp == nil {
				return fmt.Errorf("resp is nil"), controller()
			}

			if len(validators) > 0 {
				for _, validator := range validators {
					if err := validator(ctx, resp); err != nil {
						return err, controller()
					}
				}
			}

			return err, false

		}
	}

}
