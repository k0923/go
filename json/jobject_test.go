package xjson

import (
	"encoding/json"
	"fmt"
	"sort"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

const testJson = `{
  "firstName": "Charles",
  "lastName": "Doe",
  "age": 41,
  "location": {
    "city": "San Fracisco",
    "postalCode": "94103"
  },
  "people": {
    "name": "young",
    "hobbies": "pc game"
  },
  "classes": {
    "1": "test",
    "2": {
      "hobbies": [
        "football"
      ]
    },
    "city": null
  },
  "hobbies": [
    "chess",
    "netflix"
  ]
}`

func TestJsonQueryPath(t *testing.T) {
	obj, err := NewJSONObjectByString(testJson)
	if err != nil {
		panic(err)
	}

	getValue := func(path string) interface{} {
		return obj.Get(path).Value()
	}

	Convey("SimpleValue", t, func() {
		v := getValue("$.firstName")
		So(v, ShouldEqual, "Charles")
		v = getValue("$.age")
		So(v, ShouldEqual, 41)
		v = getValue("$.people.name")
		So(v, ShouldEqual, "young")
		v = getValue("$.classes.1")
		So(v, ShouldEqual, "test")
		v = getValue("$.classes.2.hobbies")
		So(v, ShouldEqualArrayIgnorePos, `["football"]`)
	})

	Convey("IndexValue", t, func() {
		v := getValue("$.hobbies[0]")
		So(v, ShouldEqual, "chess")
		v = getValue("$.hobbies[1]")
		So(v, ShouldEqual, "netflix")
		v = getValue("$.hobbies[2]")
		So(v, ShouldBeNil)
		v = getValue("$.hobbies[:]")
		printPath("$.hobbies[:]")
		So(v, ShouldEqualArrayIgnorePos, `["chess","netflix"]`)
	})

	Convey("recursiveValue", t, func() {
		v := getValue("$..hobbies")
		So(v, ShouldEqualArrayIgnorePos, `[["chess","netflix"],["football"],"pc game"]`)

	})

	Convey("wildcard value", t, func() {
		v := getValue("$.hobbies.*")
		So(v, ShouldEqualArrayIgnorePos, `["chess","netflix"]`)
		v = getValue("$.*.city")
		So(v, ShouldEqualArrayIgnorePos, `["San Fracisco",null]`)
	})

}

func printPath(path string) {
	p, err := buildJsonPath(path)
	if err != nil {
		panic(err)
	}
	for _, p1 := range p {
		fmt.Printf("%v %T\n", p1, p1)
	}

}

func ShouldEqualArrayIgnorePos(actual interface{}, expected ...interface{}) string {
	var expectedData []interface{}
	err := json.Unmarshal([]byte(expected[0].(string)), &expectedData)
	if err != nil {
		return "expected is not array"
	}
	actualData, ok := actual.([]interface{})
	if !ok {
		return "actual is not array"
	}

	if len(expectedData) != len(actualData) {
		fmt.Println(expectedData, actualData)
		return "length not equal"
	}

	sort.Slice(expectedData, func(i, j int) bool {
		dataI, _ := json.Marshal(expectedData[i])
		dataJ, _ := json.Marshal(expectedData[j])
		return string(dataI) < string(dataJ)
	})

	sort.Slice(actualData, func(i, j int) bool {
		dataI, _ := json.Marshal(actualData[i])
		dataJ, _ := json.Marshal(actualData[j])
		return string(dataI) < string(dataJ)
	})

	actionResult, _ := json.Marshal(actualData)
	expectedResult, _ := json.Marshal(expectedData)
	return ShouldEqual(string(actionResult), string(expectedResult))

}

const testJsonData = `
{
	"customers": [
      {
        "id": 3534,
        "name": "西瓜",
        "project_id": 321,
        "created": "2023-11-10T15:16:39+08:00",
        "app_id": 165622,
        "app_cn_name": "西瓜",
        "app_name": "xigua",
        "internal": true,
        "disabled": false
      },
	  {
        "id": 3535,
        "name": "西瓜1",
        "project_id": 321,
        "created": "2023-11-10T15:16:39+08:00",
        "234": 165623,
        "app_cn_name": "西瓜1",
        "app_name": "xigua1",
        "internal": true,
        "disabled": false
      }
    ],
	"users":[
		{
			"id":1
		},
		{
			"id":2
		}
	]
}
`

func TestGetCustomerByPath(t *testing.T) {
	obj, err := NewJSONObjectByString(testJsonData)
	if err != nil {
		panic(err)
	}

	fmt.Println(obj.Get("$.customers.*.234").Value())
}

// type userCase struct {
// 	Input  string
// 	Path   string
// 	Result interface{}
// }

// func TestGenericPathQuery(t *testing.T) {
// 	caselist := []userCase{
// 		{
// 			Input:  `["first", "second", "third", "forth", "fifth"]`,
// 			Path:   "$[1:3]",
// 			Result: []interface{}{"second", "third"},
// 		},
// 		// 2. Array slice on exact match
// 		{
// 			Input:  `["first", "second", "third", "forth", "fifth"]`,
// 			Path:   "$[0:5]",
// 			Result: []interface{}{"first", "second", "third", "forth", "fifth"},
// 		},
// 		// 3. Array slice on partially overlapping array
// 		{
// 			Input:  `["first", "second", "third"]`,
// 			Path:   "$[1:10]",
// 			Result: []interface{}{"second", "third"},
// 		},
// 		// 4. Array slice with negative start and end and range of 1
// 		{
// 			Input:  `[2, "a", 4, 5, 100, "nice"]`,
// 			Path:   "$[-4:-3]",
// 			Result: []interface{}{4},
// 		},
// 		// 5. Array slice with negative start and positive end and range of 1
// 		{
// 			Input:  `[2, "a", 4, 5, 100, "nice"]`,
// 			Path:   "$[-4:3]",
// 			Result: []interface{}{4},
// 		},
// 		// 6. Array slice with negative step
// 		{
// 			Input:  `["first", "second", "third", "forth", "fifth"]`,
// 			Path:   "$[3:0:-2]",
// 			Result: []interface{}{"forth", "second"},
// 		},
// 		// 7. Array slice with open end
// 		{
// 			Input:  `["first", "second", "third", "forth", "fifth"]`,
// 			Path:   "$[1:]",
// 			Result: []interface{}{"second", "third", "forth", "fifth"},
// 		},
// 		// 8. Array slice with open start
// 		{
// 			Input:  `["first", "second", "third", "forth", "fifth"]`,
// 			Path:   "$[:2]",
// 			Result: []interface{}{"first", "second"},
// 		},
// 		// 9. Array slice with open start and end
// 		{
// 			Input:  `["first", "second"]`,
// 			Path:   "$[:]",
// 			Result: []interface{}{"first", "second"},
// 		},
// 		// 10. Array slice with positive start and negative end and range of 1
// 		{
// 			Input:  `[2, "a", 4, 5, 100, "nice"]`,
// 			Path:   "$[3:-2]",
// 			Result: []interface{}{5},
// 		},
// 		// 11. Array slice with start -1 and open end
// 		{
// 			Input:  `["first", "second", "third"]`,
// 			Path:   "$[-1:]",
// 			Result: []interface{}{"third"},
// 		},
// 		// 12. Bracket notation
// 		{
// 			Input:  `{"key": "value"}`,
// 			Path:   "$['key']",
// 			Result: "value",
// 		},
// 		// 13. Bracket notation on object without key
// 		{
// 			Input:  `{"key": "value"}`,
// 			Path:   "$['missing']",
// 			Result: nil,
// 		},
// 		// 14. Bracket notation after recursive descent
// 		{
// 			Input:  `[ "first", { "key": [ "first nested", { "more": [ { "nested": ["deepest", "second"] }, ["more", "values"] ] } ] }]`,
// 			Path:   "$..[0]",
// 			Result: []interface{}{"first", "first nested", map[string][]interface{}{"nested": {"deepest", "second"}}, "deepest", "more"},
// 		},
// 		// 15. Bracket notation with dot
// 		{
// 			Input:  `{ "one": {"key": "value"}, "two": {"some": "more", "key": "other value"}, "two.some": "42"}`,
// 			Path:   "$['two.some']",
// 			Result: "42",
// 		},
// 		// 16. Bracket notation with empty string
// 		{
// 			Input:  `{"": 42, "''": 123, "\"\"": 222}`,
// 			Path:   "$['']",
// 			Result: 42,
// 		},
// 		// 17. Bracket notation with number
// 		{
// 			Input:  `["first", "second", "third", "forth", "fifth"]`,
// 			Path:   "$[2]",
// 			Result: "third",
// 		},
// 		// 18. Bracket notation with number on object
// 		{
// 			Input:  `{"0": "value"}`,
// 			Path:   "$[0]",
// 			Result: []interface{}{},
// 		},
// 		// 19. Bracket notation with number after dot notation with wildcard on nested arrays with different length
// 		{
// 			Input:  `[[1], [2,3]]`,
// 			Path:   "$.*[1]",
// 			Result: []interface{}{3},
// 		},
// 		// 20. Bracket notation with number -1
// 		{
// 			Input:  `["first", "second", "third"]`,
// 			Path:   "$[-1]",
// 			Result: "third",
// 		},
// 		// 21. Bracket notation with quoted number on object
// 		{
// 			Input:  `{"0": "value"}`,
// 			Path:   "$['0']",
// 			Result: "value",
// 		},
// 		// 22. Bracket notation with wildcard on array
// 		{
// 			Input:  `[ "string", 42, { "key": "value" }, [0, 1]]`,
// 			Path:   "$[*]",
// 			Result: []interface{}{"string", 42, map[interface{}]interface{}{"key": "value"}, []interface{}{0, 1}},
// 		},
// 		// 23. Bracket notation with wildcard after array slice
// 		{
// 			Input:  `[[1, 2], ["a", "b"], [0, 0]]`,
// 			Path:   "$[0:2][*]",
// 			Result: []interface{}{1, 2, "a", "b"},
// 		},
// 		// 24. Bracket notation with wildcard after dot notation after bracket notation with wildcard
// 		{
// 			Input:  `[{"bar": [42]}]`,
// 			Path:   "$[*].bar[*]",
// 			Result: []interface{}{42},
// 		},
// 		// 25: Bracket notation with wildcard after recursive descent
// 		{
// 			Input:  `{"key": "value","another key": {"complex": "string","primitives": [0, 1]}}`,
// 			Path:   "$..[*]",
// 			Result: []interface{}{"value", map[interface{}]interface{}{"complex": "string", "primitives": []interface{}{0, 1}}, "string", []interface{}{0, 1}, 0, 1},
// 		},
// 		// 26. Dot notation
// 		{
// 			Input:  `{"key": "value"}`,
// 			Path:   "$.key",
// 			Result: "value",
// 		},
// 		// 27. Dot notation on array value
// 		{
// 			Input:  `{"key": ["first", "second"]}`,
// 			Path:   "$.key",
// 			Result: []interface{}{"first", "second"},
// 		},
// 		// 28. Dot notation on empty object value
// 		{
// 			Input:  `{"key": {}}`,
// 			Path:   "$.key",
// 			Result: map[interface{}]interface{}{},
// 		},
// 		// 29. Dot notation on object without key
// 		{
// 			Input:  `{"key": "value"}`,
// 			Path:   "$.missing",
// 			Result: []interface{}{},
// 		},
// 		// 30. Dot notation after array slice
// 		{
// 			Input:  `[{"key": "ey"}, {"key": "bee"}, {"key": "see"}]`,
// 			Path:   "$[0:2].key",
// 			Result: []interface{}{"ey", "bee"},
// 		},
// 		// 31. Dot notation after bracket notation with wildcard
// 		{
// 			Input:  `[{"a": 1},{"a": 1}]`,
// 			Path:   "$[*].a",
// 			Result: []interface{}{1, 1},
// 		},
// 		// 32. Dot notation after filter expression
// 		// {
// 		// 	Input:  `[{"id": 42, "name": "forty-two"}, {"id": 1, "name": "one"}]`,
// 		// 	Path:   "$[?(@id==42)]",
// 		// 	Result: []interface{}{"forty-two"},
// 		// },
// 		// 33. Dot notation after recursive descent
// 		{
// 			Input:  `{ "object": { "key": "value", "array": [ {"key": "something"}, {"key": {"key": "russian dolls"}} ] }, "key": "top"}`,
// 			Path:   "$..key",
// 			Result: []interface{}{"value", "something", map[interface{}]interface{}{"key": "russian dolls"}, "russian dolls", "top"},
// 		},
// 		// 34. Dot notation after recursive descent after dot notation
// 		// {
// 		// 	Input:  StoreExampleEvent,
// 		// 	Path:   "$.store..price",
// 		// 	Result: []interface{}{12.99, 19.95, 22.99, 8.95, 8.99},
// 		// },
// 		// 35. Dot notation after union
// 		{
// 			Input:  `[{"key": "ey"}, {"key": "bee"}, {"key": "see"}]`,
// 			Path:   "$[0,2].key",
// 			Result: []interface{}{"ey", "see"},
// 		},
// 		// 36. Dot notation after union with keys
// 		{
// 			Input:  `{ "one": {"key": "value"}, "two": {"k": "v"}, "three": {"some": "more", "key": "other value"}}`,
// 			Path:   "$['one','three'].key",
// 			Result: []interface{}{"value", "other value"},
// 		},
// 		// 37. Dot notation with non ASCII key
// 		{
// 			Input:  `{"属性": "value"}`,
// 			Path:   "$.属性",
// 			Result: "value",
// 		},
// 		// 38. Dot notation with number on object
// 		{
// 			Input:  `{"属性": "value"}`,
// 			Path:   "$.属性",
// 			Result: "value",
// 		},
// 		// 39. Dot notation with wildcard after dot notation after dot notation with wildcard
// 		{
// 			Input:  `[{"bar": [42]}]`,
// 			Path:   "$.*.bar.*",
// 			Result: []interface{}{42},
// 		},
// 		// 40. Dot notation with wildcard after recursive descent
// 		{
// 			Input:  `{ "key": "value", "another key": { "complex": "string", "primitives": [0, 1]}}`,
// 			Path:   "$..*",
// 			Result: []interface{}{"string", "value", 0, 1, []interface{}{0, 1}, map[interface{}]interface{}{"complex": "string", "primitives": []interface{}{0, 1}}},
// 		},
// 		// 41. Filter expression after dot notation with wildcard after recursive descent
// 		// {
// 		// 	Input: `[ { "complexity": { "one": [ { "name": "first", "id": 1 }, { "name": "next", "id": 2 }, { "name": "another", "id": 3 }, { "name": "more", "id": 4 } ], "more": { "name": "next to last", "id": 5 } } }, { "name": "last", "id": 6 }]`,
// 		// 	Path:  "$..*[?(@.id>2)]",
// 		// 	Result: []interface{}{map[interface{}]interface{}{"name": "next to last", "id": 5},
// 		// 		map[interface{}]interface{}{"name": "another", "id": 3},
// 		// 		map[interface{}]interface{}{"name": "more", "id": 4}},
// 		// },
// 		// 42. Filter expression with addition
// 		// {
// 		// 	Input:  `[{"key": 60}, {"key": 50}, {"key": 10}, {"key": -50}, {"key+50": 100}]`,
// 		// 	Path:   "$[?(@.key+50==100)]",
// 		// 	Result: []interface{}{map[interface{}]interface{}{"key": 50}},
// 		// },
// 		// 43. Filter expression with boolean and operator
// 		// {
// 		// 	Input:  `[ {"key": 42}, {"key": 43}, {"key": 44}]`,
// 		// 	Path:   "$[?(@.key>42 && @.key<44)]",
// 		// 	Result: []interface{}{map[interface{}]interface{}{"key": 43}},
// 		// },
// 		// 44. Filter expression with boolean or operator
// 		// {
// 		// 	Input:  `[ {"key": 42}, {"key": 43}, {"key": 44}]`,
// 		// 	Path:   "$[?(@.key>43 || @.key<43)]",
// 		// 	Result: []interface{}{map[interface{}]interface{}{"key": 42}, map[interface{}]interface{}{"key": 44}},
// 		// },
// 		// 45. Filter expression with bracket notation
// 		// {
// 		// 	Input:  `[ {"key": 0}, {"key": 42}, {"key": -1}, {"key": 41}, {"key": 43}, {"key": 42.0001}, {"key": 41.9999}, {"key": 100}, {"some": "value"}]`,
// 		// 	Path:   "$[?(@['key']==42)]",
// 		// 	Result: []interface{}{map[interface{}]interface{}{"key": 42}},
// 		// },
// 		// 46. Filter expression with bracket notation with number
// 		// {
// 		// 	Input:  `[["a", "b"], ["x", "y"]]`,
// 		// 	Path:   "$[?(@[1]=='b')]",
// 		// 	Result: []interface{}{[]interface{}{"a", "b"}},
// 		// },
// 		// 47. Filter expression with bracket notation with number on object
// 		// {
// 		// 	Input:  `{"1": ["a", "b"], "2": ["x", "y"]}`,
// 		// 	Path:   "$[?(@[1]=='b')]",
// 		// 	Result: []interface{}{[]interface{}{"a", "b"}},
// 		// },
// 		// 48. Filter expression with different grouped operators
// 		// {
// 		// 	Input:  `[{"a":true},{"a":true,"b":true},{"a":true,"b":true,"c":true},{"b":true,"c":true},{"a":true,"c":true},{"c":true},{"b":true}]`,
// 		// 	Path:   "$[?(@.a && (@.b || @.c))]",
// 		// 	Result: []interface{}{map[interface{}]interface{}{"a": true, "b": true}, map[interface{}]interface{}{"a": true, "b": true, "c": true}, map[interface{}]interface{}{"a": true, "c": true}},
// 		// },
// 		// 49. Filter expression with different grouped operators
// 		// {
// 		// 	Input:  `[{"a":true,"b":true},{"a":true,"b":true,"c":true},{"b":true,"c":true},{"a":true,"c":true},{"a":true},{"b":true},{"c":true},{"d":true},{}]`,
// 		// 	Path:   "$[?(@.a && @.b || @.c)]",
// 		// 	Result: []interface{}{map[interface{}]interface{}{"a": true, "b": true}, map[interface{}]interface{}{"a": true, "b": true, "c": true}, map[interface{}]interface{}{"a": true, "c": true}, map[interface{}]interface{}{"b": true, "c": true}, map[interface{}]interface{}{"c": true}},
// 		// },
// 		// 50. Filter expression with equals
// 		// {
// 		// 	Input:  `[{"a":true,"b":true},{"a":true,"b":true,"c":true},{"b":true,"c":true},{"a":true,"c":true},{"a":true},{"b":true},{"c":true},{"d":true},{}]`,
// 		// 	Path:   "$[?(@.a && @.b || @.c)]",
// 		// 	Result: []interface{}{map[interface{}]interface{}{"a": true, "b": true}, map[interface{}]interface{}{"a": true, "b": true, "c": true}, map[interface{}]interface{}{"a": true, "c": true}, map[interface{}]interface{}{"b": true, "c": true}, map[interface{}]interface{}{"c": true}},
// 		// },
// 		// 51. Filter expression with equals
// 		// {
// 		// 	Input:  `[{"key":42},{"key":41.9999},{"key":"42"},{"key":[42]},{"key":{"key":42}}]`,
// 		// 	Path:   "$[?(@.key==42)]",
// 		// 	Result: []interface{}{map[interface{}]interface{}{"key": 42}},
// 		// },
// 		// 52. Filter expression with equals string
// 		// {
// 		// 	Input:  `[{"key":"some"},{"key":"value"},{"key":"valuemore"},{"key":["value"]},{"key":{"some":"value"}},{"key":{"key":"value"}},{"some":"value"}]`,
// 		// 	Path:   `$[?(@.key=="value")]`,
// 		// 	Result: []interface{}{map[interface{}]interface{}{"key": "value"}},
// 		// },
// 		// 53. Filter expression with equals true
// 		// {
// 		// 	Input:  `[{"key":true},{"key":false},{"key":1}]`,
// 		// 	Path:   "$[?(@.key==true)]",
// 		// 	Result: []interface{}{map[interface{}]interface{}{"key": true}},
// 		// },
// 		// 54. Union
// 		{
// 			Input:  `["first", "second", "third"]`,
// 			Path:   "$[0,1]",
// 			Result: []interface{}{"first", "second"},
// 		},
// 		// 55. Union with keys
// 		{
// 			Input:  `{"key": "value","another": "entry"}`,
// 			Path:   "$['key','another']",
// 			Result: []interface{}{"value", "entry"},
// 		},
// 		// 56. Union with keys on object without key
// 		{
// 			Input:  `{"key": "value","another": "entry"}`,
// 			Path:   "$['missing','key']",
// 			Result: []interface{}{"value"},
// 		},
// 		// 57. Union with keys after recursive descent
// 		{
// 			Input:  `[{"c":"cc1","d":"dd1","e":"ee1"}, {"c": "cc2", "child": {"d": "dd2"}}, {"c": "cc3"}, {"d": "dd4"}, {"child": {"c": "cc5"}}]`,
// 			Path:   "$..['c','d']",
// 			Result: []interface{}{"cc1", "cc2", "cc3", "cc5", "dd1", "dd2", "dd4"},
// 		},
// 	}
// 	// 58. Filter expression in object
// 	// {
// 	// 	Input:  StoreExampleEvent,
// 	// 	Path:   `$.store.book[?(@.category=='reference')].title`,
// 	// 	Result: []interface{}{"Sayings of the Century"},
// 	// },
// 	// 59. Filter expression in object
// 	// {
// 	// 	Input:  StoreExampleEvent,
// 	// 	Path:   `$.store.book[?(@.price > 10)].title`,
// 	// 	Result: []interface{}{"Sword of Honour", "The Lord of the Rings"},
// 	// },
// 	for _, c := range caselist {
// 		obj, err := NewJSONObjectByString(c.Input)
// 		if err != nil {
// 			fmt.Println("error path:", c.Path)
// 			panic(err)
// 		}
// 		fmt.Println(obj.Get(c.Path).Value(), "-------", c.Result)
// 	}

// 	printPath("$['key']")

// }
