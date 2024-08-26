package condition

import (
	"context"

	. "github.com/k0923/go/json"
)

type Opt string
type GroupOpt string

const (
	NE          Opt = "neq" // 不等于
	Equals      Opt = "eq"  // 相等
	In          Opt = "in"  // 包含于
	All         Opt = "all"
	Exist       Opt = "exist"       // 存在
	GreaterThan Opt = "greaterThan" // 大于
	LessThan    Opt = "lessThan"    // 小于
)



type Condition interface {
	IsMatch(ctx context.Context) (bool, error)
}

type Fetcher interface {
	GetValue(parent interface{}, root interface{}) (interface{}, error)
}

type SimpleCondition struct {
	X   G[Picker] `json:"x"`
	Opt Opt       `json:"opt"`
	Y   G[Picker] `json:"y"`
}

type Calculator interface {
	CanHandle()
}
