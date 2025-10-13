package json

import (
	"encoding/json"
	"fmt"
	"testing"
)

type TodoSourceWeb struct {
}

func (source TodoSourceWeb) GetSource() string {
	return "web"
}

type TodoSourceLark struct {
	AppID  string `json:"app_id"`
	ChatID string `json:"chat_id"`
}

func (source TodoSourceLark) GetSource() string {
	return "lark"
}

func TestSort(t *testing.T) {
	data := `
	{"data": null, "type": "web"}
	`
	source := TodoSource([]byte(data))

	iface, err := source.Interface()
	fmt.Println(iface, err)
}

type TodoSource []byte

func (source *TodoSource) UnmarshalJSON(data []byte) error {
	*source = data
	return nil
}

func (source TodoSource) Interface() (interface{}, error) {
	type temp struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	var t temp
	err := json.Unmarshal(source, &t)
	if err != nil {
		return nil, err
	}
	if t.Type == "lark" {
		var lark TodoSourceLark
		err = json.Unmarshal(t.Data, &lark)
		if err != nil {
			return nil, err
		}
		return lark, nil
	}
	return TodoSourceWeb{}, nil

}

type Student struct {
	Name string `json:"name"`
	Age  int    `json:"age"`
}

func BenchmarkJSONSort(b *testing.B) {
	data := []byte(`
	{
		"type": "nexus.event.todo.update",
		"b": [
			{},
			{},
			[]
		],
		"a": {},
		"e": {
			"1": [
				{
					"6": 3,
					"1": "{} [] {/asw}",
					"2": 3
				}
			],
			"0": {
				"1": null,
				
				"3": true,
				"2": false
			}
		}
	}
	`)

	for i := 0; i < b.N; i++ {
		_, err := SortJSON(data)
		if err != nil {
			panic(err)
		}
	}

}

func BenchmarkUnmarshal(b *testing.B) {
	data := []byte(`
	{
		"type": "nexus.event.todo.update",
		"b": [
			{},
			{},
			[]
		],
		"a": {},
		"e": {
			"1": [
				{
					"6": 3,
					"1": "{} [] {/asw}",
					"2": 3
				}
			],
			"0": {
				"1": null,
				
				"3": true,
				"2": false
			}
		}
	}
	`)
	var result interface{}
	for i := 0; i < b.N; i++ {
		err := json.Unmarshal(data, &result)
		if err != nil {
			panic(err)
		}
	}
}

func TestTT(t *testing.T) {
	data := []byte(`
	{
		"type": "nexus.event.todo.update",
		"b": [
			{},                                                                                                                                                                      
			{},
			[]
		],
		"a": {},
		"e": {
			"1": [
				{
					"6": 3,
					"1": "{} [] {/asw}",
					"2": 3
				}
			],
			"0": {
				"1": null,
				
				"3": true,
				"2": false
			}
		}
	}
	`)

	data, err := SortJSON(data)
	fmt.Println(string(data), err)
	if err != nil {
		panic(err)
	}

}

func BenchmarkSer(b *testing.B) {
	data := `
	{
		"type": "nexus.event.todo.update",
		"b": [
			{},
			{},
			[]
		],
		"a": {},
		"e": {
			"1": [
				{
					"6": 3,
					"1": "{} [] {/asw}",
					"2": 3
				}
			],
			"0": {
				"1": null,
				
				"3": true,
				"2": false
			}
		}
	}
	`
	for i := 0; i < b.N; i++ {
		var m interface{}
		json.Unmarshal([]byte(data), &m)
		json.Marshal(m)
	}

}
