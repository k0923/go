package xfield

import (
	"encoding/json"
	"math"
	"math/big"
	"reflect"
	"strconv"
)

type NumberField[P any] struct {
	Meta
	Min  *float64 `json:"min,omitempty"`
	Max  *float64 `json:"max,omitempty"`
	Enum []any    `json:"enum,omitempty"`
}

func (*NumberField[P]) Type() string { return "number" }

func (f *NumberField[P]) ValidateSchema() error {
	for _, bound := range []*float64{f.Min, f.Max} {
		if bound != nil && (math.IsNaN(*bound) || math.IsInf(*bound, 0)) {
			return invalidSchema("number bounds must be finite")
		}
	}
	if f.Min != nil && f.Max != nil && *f.Min > *f.Max {
		return invalidSchema("min exceeds max")
	}
	for i, value := range f.Enum {
		if _, ok := asNumber(value); !ok {
			return wrapPathError(joinIndexPath("enum", i), invalidSchema("expect finite number"))
		}
	}
	return nil
}

func (f *NumberField[P]) Parse(data any, _ P) (any, error) {
	if err := f.ValidateSchema(); err != nil {
		return nil, err
	}
	if data == nil {
		return nil, nil
	}
	num, ok := asNumber(data)
	if !ok {
		return nil, invalidValue("expect finite number")
	}
	if f.Min != nil {
		min, _ := asNumber(*f.Min)
		if num.Cmp(min) < 0 {
			return nil, invalidValue("value < min(%v)", *f.Min)
		}
	}
	if f.Max != nil {
		max, _ := asNumber(*f.Max)
		if num.Cmp(max) > 0 {
			return nil, invalidValue("value > max(%v)", *f.Max)
		}
	}
	if len(f.Enum) > 0 {
		found := false
		for _, value := range f.Enum {
			member, _ := asNumber(value)
			found = found || num.Cmp(member) == 0
		}
		if !found {
			return nil, invalidValue("not in enum")
		}
	}
	return data, nil
}

// Compare decimal representations exactly instead of rounding large integers
// through float64. Named Go numeric types and json.Number are supported.
func asNumber(value any) (*big.Rat, bool) {
	var text string
	if number, ok := value.(json.Number); ok {
		text = string(number)
		if !json.Valid([]byte(text)) || text == "" || (text[0] != '-' && (text[0] < '0' || text[0] > '9')) {
			return nil, false
		}
	} else if value != nil {
		v := reflect.ValueOf(value)
		switch v.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			text = strconv.FormatInt(v.Int(), 10)
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			text = strconv.FormatUint(v.Uint(), 10)
		case reflect.Float32, reflect.Float64:
			num := v.Float()
			if math.IsNaN(num) || math.IsInf(num, 0) {
				return nil, false
			}
			text = strconv.FormatFloat(num, 'g', -1, v.Type().Bits())
		default:
			return nil, false
		}
	}
	// Avoid unbounded allocation from malicious exponent strings.
	if len(text) > 1024 {
		return nil, false
	}
	for i, c := range text {
		if c == 'e' || c == 'E' {
			exponent, err := strconv.Atoi(text[i+1:])
			if err != nil || exponent < -1024 || exponent > 1024 {
				return nil, false
			}
			break
		}
	}
	num, ok := new(big.Rat).SetString(text)
	return num, ok
}
