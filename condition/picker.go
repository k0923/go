package condition

import "context"

type Picker interface {
	Value(ctx context.Context) any
}
