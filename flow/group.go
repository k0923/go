package flow

import "context"

type GroupContext struct {
	context.Context
	branches []string
}
