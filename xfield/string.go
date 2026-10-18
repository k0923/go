package xfield

import (
	"regexp"
	"unicode/utf8"
)

type EnumOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// StringFieldConfig retains the source schema's enum option representation.
type StringFieldConfig struct {
	EnumOptions []EnumOption `json:"enumOptions,omitempty"`
}

type StringField[P any] struct {
	Meta
	MinLength *int              `json:"minLength,omitempty"`
	MaxLength *int              `json:"maxLength,omitempty"`
	Pattern   *string           `json:"pattern,omitempty"`
	Enum      []string          `json:"enum,omitempty"`
	Config    StringFieldConfig `json:"config,omitempty"`
}

func (*StringField[P]) Type() string { return "string" }

func (f *StringField[P]) ValidateSchema() error {
	if err := validateIntBounds(f.MinLength, f.MaxLength); err != nil {
		return err
	}
	if f.Pattern != nil {
		if _, err := regexp.Compile(*f.Pattern); err != nil {
			return invalidSchema("invalid pattern: %v", err)
		}
	}
	return nil
}

func (f *StringField[P]) Parse(data any, _ P) (any, error) {
	if err := f.ValidateSchema(); err != nil {
		return nil, err
	}
	if data == nil {
		return nil, nil
	}
	s, ok := data.(string)
	if !ok {
		return nil, invalidValue("expect string")
	}
	length := utf8.RuneCountInString(s)
	if f.MinLength != nil && length < *f.MinLength {
		return nil, invalidValue("length < minLength(%d)", *f.MinLength)
	}
	if f.MaxLength != nil && length > *f.MaxLength {
		return nil, invalidValue("length > maxLength(%d)", *f.MaxLength)
	}
	if f.Pattern != nil {
		re, _ := regexp.Compile(*f.Pattern)
		if !re.MatchString(s) {
			return nil, invalidValue("not match pattern")
		}
	}
	if len(f.Enum) > 0 {
		found := false
		for _, value := range f.Enum {
			found = found || value == s
		}
		if !found {
			return nil, invalidValue("not in enum")
		}
	}
	// Config.EnumOptions is display metadata, matching the source semantics.
	return s, nil
}
