package condition

import (
	"context"
	. "github.com/k0923/go/json"
)

const (
	And GroupOpt = "and"
	Or  GroupOpt = "or"
)

type GroupCondition struct {
	Opt        GroupOpt       `json:"opt"`
	Conditions []G[Condition] `json:"conditions"`
}

func (c GroupCondition) IsMatch(ctx context.Context) (bool, error) {
	if len(c.Conditions) == 0 {
		return true, nil
	}
	for _, condition := range c.Conditions {
		result, err := condition.Value().IsMatch(ctx)
		if err != nil {
			return false, err
		}
		if c.Opt == And {
			if !result {
				return false, nil
			}
		} else {
			if result {
				return true, nil
			}
		}
	}
	if c.Opt == And {
		return true, nil
	}
	return false, nil
}
