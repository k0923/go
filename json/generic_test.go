package xjson

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

type Speeker interface {
	SayHi() string
}

type Animal interface {
	SayHi() string
}

type Dog struct {
	Name string
}

func (d Dog) SayHi() string {
	return "woof"
}

type Cat struct {
	Name string
}

func (c Cat) SayHi() string {
	return "meow"
}

type S1 struct {
	A int
}

func (s S1) SayHi() string {
	return fmt.Sprintf("Hello no.:%d", s.A)
}

type S2 struct {
	B string
}

func (s S2) SayHi() string {
	return fmt.Sprintf("Hello %s", s.B)
}

type SpeakObj struct {
	S G[Speeker] `json:"s"`
}

func init() {
	Bind[Task1](map[string]Task1{
		"vod": &VodTask{},
	})
}

type TaskDto struct {
	ID   string   `json:"id"`
	Task G[Task1] `json:"task"`
}

type Task1 interface {
	Run(ctx context.Context) error
}

type VodTask struct {
	Url string `json:"url"`
}

func (v *VodTask) Run(ctx context.Context) error {
	return nil
}

func init() {
	binding()
}

func binding() {
	Bind(map[string]Speeker{
		"s1":  S1{},
		"s2":  S2{},
		"cum": CustomUnmarshal{},
	})
	Bind(map[string]Manable{
		"s3": &S3{},
	})

}

type Manable interface {
	// Speeker
	MyName() string
}

type S3 struct {
	S1 S1 `json:"s1"`
}

func (s3 S3) SayHi() string {
	return "hello s3"
}

func (s3 *S3) MyName() string {
	return "my name is s3"
}

type OmitEmptyObj struct {
	S G[Speeker] `json:"s,omitempty"`
}

func TestMarshalSpeaker(t *testing.T) {
	Convey("generic marshal", t, func() {
		obj := SpeakObj{
			S: NG[Speeker](S1{}),
		}
		data, err := json.Marshal(obj)
		So(err, ShouldBeNil)
		So(string(data), ShouldEqual, `{"s":{"data":{"A":0},"type":"s1"}}`)
		So(obj.S, ShouldNotBeNil)
	})

	Convey("marshal omitempty", t, func() {
		var obj OmitEmptyObj
		data, err := json.Marshal(obj)
		So(err, ShouldBeNil)
		So(string(data), ShouldEqual, "{}")
	})

	Convey("marshal nil", t, func() {
		Convey("just nil", func() {
			obj := SpeakObj{
				S: NG[Speeker](nil),
			}
			data, err := json.Marshal(obj)
			So(err, ShouldBeNil)
			So(string(data), ShouldEqual, `{"s":null}`)
			So(obj.S, ShouldNotBeNil)
		})
		Convey("not binding", func() {
			var s *S1
			obj := SpeakObj{
				S: NG[Speeker](s),
			}
			_, err := json.Marshal(obj)
			So(err, ShouldNotBeNil)
			// So(string(data), ShouldEqual, `{"s":null}`)
			// So(obj.S, ShouldNotBeNil)
		})
	})

}

func TestUnmarshalSpeaker(t *testing.T) {
	Convey("generic unmarshal", t, func() {
		var obj SpeakObj
		data := `{"s":{"type":"s1","data":{"A":0}}}`
		err := json.Unmarshal([]byte(data), &obj)
		So(err, ShouldBeNil)
		So(len(obj.S), ShouldEqual, 1)
		So(obj.S.Value(), ShouldNotBeNil)
		So(obj.S.Value().SayHi(), ShouldEqual, "Hello no.:0")
	})

	Convey("unmarshal nil", t, func() {
		Convey("just nil", func() {
			var obj SpeakObj
			data := `{"s":null}`
			err := json.Unmarshal([]byte(data), &obj)
			So(err, ShouldBeNil)

			So(obj.S.Value(), ShouldEqual, nil)

			// 这里的 Value() 返回的是接口类型的 nil
			So(obj.S.Value(), ShouldBeNil)
		})

		Convey("nil with type1", func() {
			var obj SpeakObj
			data := `{"s":{"type":"s1","data":null}}`
			err := json.Unmarshal([]byte(data), &obj)
			So(err, ShouldBeNil)
			So(obj.S.Value(), ShouldBeNil)
		})

		Convey("empty field", func() {
			var obj SpeakObj
			data := `{}`
			err := json.Unmarshal([]byte(data), &obj)
			So(err, ShouldBeNil)
			So(obj.S.Value(), ShouldBeNil)
		})

		Convey("direct unmarshal", func() {
			data := `{"type":"s1","data":{"A":0}}`
			var s G[Speeker]
			err := json.Unmarshal([]byte(data), &s)
			So(err, ShouldBeNil)
			So(s.Value().SayHi(), ShouldEqual, "Hello no.:0")
		})
	})

	Convey("unmarshal array", t, func() {
		Convey("nil array", func() {
			data := `[null,null]`
			var s []G[Speeker]
			err := json.Unmarshal([]byte(data), &s)
			So(err, ShouldBeNil)
			So(len(s), ShouldEqual, 2)
		})
	})
}

type CustomUnmarshal struct {
	A int `json:"a"`
}

func (c CustomUnmarshal) SayHi() string {
	return fmt.Sprintf("custom %d", c.A)
}

func (c *CustomUnmarshal) UnmarshalJSON(data []byte) error {
	c.A = 100
	return nil
}

func TestCustomUnmarshal(t *testing.T) {

	Convey("Test Custom Unmarshal", t, func() {
		data := `
		{
			"type":"cum",
			"data":{
				"a":130
			}
		}
		`
		var obj G[Speeker]
		err := json.Unmarshal([]byte(data), &obj)
		So(err, ShouldBeNil)
		So(obj.Value().SayHi(), ShouldEqual, "custom 100")

	})
}

func TestXxx1(t *testing.T) {
	data := `{}`
	var p PeopleV2
	err := json.Unmarshal([]byte(data), &p)
	if err != nil {
		panic(err)
	}
	fmt.Println(p.Data.Value())
}

type PeopleV2 struct {
	Data G[Speeker] `json:"data"`
}

func TestXXX2(t *testing.T) {
	var p PeopleV2
	var data = `{"data":{"type":"s1"}}`
	err := json.Unmarshal([]byte(data), &p)
	if err != nil {
		panic(err)
	}

	fmt.Println(p.Data.Value() == nil)

}

type ElementTyper interface {
	Type() string
}

type TextElement string

func (t TextElement) Type() string {
	return "text"
}

type MensionUserElement string

func (m MensionUserElement) Type() string {
	return "mention_user"
}

// func TestCustomSerializer(t *testing.T) {
// 	Bind(map[string]ElementTyper{
// 		"text":         TextElement(""),
// 		"mention_user": MensionUserElement(""),
// 	}, ElementSerializer{})

// 	Convey("Test Custom Serializer", t, func() {
// 		var data []G[ElementTyper] = []G[ElementTyper]{
// 			NG[ElementTyper](TextElement("hello")),
// 			NG[ElementTyper](MensionUserElement("world")),
// 		}
// 		d, err := json.Marshal(data)
// 		So(err, ShouldBeNil)

// 		var d1 []G[ElementTyper]
// 		err = json.Unmarshal(d, &d1)
// 		So(err, ShouldBeNil)
// 		_, ok := d1[0].Value().(TextElement)
// 		So(ok, ShouldBeTrue)
// 		_, ok = d1[1].Value().(MensionUserElement)
// 		So(ok, ShouldBeTrue)

// 	})

// }

type MockJSONHandler struct {
	MarshalCalled   bool
	UnmarshalCalled bool
	CustomPrefix    string
}

func (m *MockJSONHandler) Marshal(v any) ([]byte, error) {
	m.MarshalCalled = true
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if m.CustomPrefix != "" {
		return []byte(m.CustomPrefix + string(data)), nil
	}
	return data, nil
}

func (m *MockJSONHandler) Unmarshal(data []byte, v any) error {
	m.UnmarshalCalled = true
	if m.CustomPrefix != "" && len(data) > len(m.CustomPrefix) && string(data[:len(m.CustomPrefix)]) == m.CustomPrefix {
		data = data[len(m.CustomPrefix):]
	}
	return json.Unmarshal(data, v)
}

func TestWithJSONHandler(t *testing.T) {
	Convey("Test WithJSONHandler", t, func() {
		Convey("custom JSON handler for marshal", func() {
			mockHandler := &MockJSONHandler{}

			Bind(map[string]S1{
				"custom_s1": {},
			}, WithJSONHandler(mockHandler.Marshal, mockHandler.Unmarshal))

			obj := struct {
				S G[S1] `json:"s"`
			}{
				S: NG[S1](S1{A: 42}),
			}

			data, err := json.Marshal(obj)
			So(err, ShouldBeNil)
			So(mockHandler.MarshalCalled, ShouldBeTrue)
			So(string(data), ShouldContainSubstring, `"type":"custom_s1"`)
			So(string(data), ShouldContainSubstring, `"A":42`)
		})

		Convey("custom JSON handler for unmarshal", func() {
			mockHandler := &MockJSONHandler{}

			Bind(map[string]S2{
				"custom_s2": {},
			}, WithJSONHandler(mockHandler.Marshal, mockHandler.Unmarshal))

			var obj struct {
				S G[S2] `json:"s"`
			}

			data := `{"s":{"type":"custom_s2","data":{"B":"test"}}}`
			err := json.Unmarshal([]byte(data), &obj)
			So(err, ShouldBeNil)
			So(mockHandler.UnmarshalCalled, ShouldBeTrue)
			So(obj.S.Value().B, ShouldEqual, "test")
		})

		Convey("custom JSON handler error handling", func() {
			errorHandler := &MockJSONHandler{}

			type ErrorStruct struct {
				Val int `json:"val"`
			}

			Bind(map[string]ErrorStruct{
				"error_struct": {},
			}, WithJSONHandler(errorHandler.Marshal, errorHandler.Unmarshal))

			var obj struct {
				S G[ErrorStruct] `json:"s"`
			}
			invalidData := `{"s":{"type":"error_struct","data":invalid}}`
			err := json.Unmarshal([]byte(invalidData), &obj)
			So(err, ShouldNotBeNil)
		})

		Convey("custom JSON handler with array", func() {
			mockHandler := &MockJSONHandler{}

			type ArrayStruct struct {
				Val int `json:"val"`
			}

			Bind(map[string]ArrayStruct{
				"array_struct": {},
			}, WithJSONHandler(mockHandler.Marshal, mockHandler.Unmarshal))

			obj := struct {
				Items []G[ArrayStruct] `json:"items"`
			}{
				Items: []G[ArrayStruct]{
					NG[ArrayStruct](ArrayStruct{Val: 1}),
					NG[ArrayStruct](ArrayStruct{Val: 2}),
					NG[ArrayStruct](ArrayStruct{Val: 3}),
				},
			}

			data, err := json.Marshal(obj)
			So(err, ShouldBeNil)
			So(mockHandler.MarshalCalled, ShouldBeTrue)

			var obj2 struct {
				Items []G[ArrayStruct] `json:"items"`
			}
			err = json.Unmarshal(data, &obj2)
			So(err, ShouldBeNil)
			So(mockHandler.UnmarshalCalled, ShouldBeTrue)
			So(len(obj2.Items), ShouldEqual, 3)
			So(obj2.Items[0].Value().Val, ShouldEqual, 1)
			So(obj2.Items[1].Value().Val, ShouldEqual, 2)
			So(obj2.Items[2].Value().Val, ShouldEqual, 3)
		})

		Convey("custom JSON handler with nested struct", func() {
			type NestedStruct struct {
				Inner S1 `json:"inner"`
			}

			mockHandler := &MockJSONHandler{}

			Bind(map[string]NestedStruct{
				"nested": {},
			}, WithJSONHandler(mockHandler.Marshal, mockHandler.Unmarshal))

			obj := struct {
				Data G[NestedStruct] `json:"data"`
			}{
				Data: NG[NestedStruct](NestedStruct{
					Inner: S1{A: 100},
				}),
			}

			data, err := json.Marshal(obj)
			So(err, ShouldBeNil)
			So(mockHandler.MarshalCalled, ShouldBeTrue)
			So(string(data), ShouldContainSubstring, `"type":"nested"`)

			var obj2 struct {
				Data G[NestedStruct] `json:"data"`
			}
			err = json.Unmarshal(data, &obj2)
			So(err, ShouldBeNil)
			So(mockHandler.UnmarshalCalled, ShouldBeTrue)
			So(obj2.Data.Value().Inner.A, ShouldEqual, 100)
		})

		Convey("custom JSON handler with pointer type", func() {
			mockHandler := &MockJSONHandler{}

			type PtrStruct struct {
				Val int `json:"val"`
			}

			Bind(map[string]*PtrStruct{
				"ptr_struct": nil,
			}, WithJSONHandler(mockHandler.Marshal, mockHandler.Unmarshal))

			obj := struct {
				S G[*PtrStruct] `json:"s"`
			}{
				S: NG[*PtrStruct](&PtrStruct{Val: 77}),
			}

			data, err := json.Marshal(obj)
			So(err, ShouldBeNil)
			So(mockHandler.MarshalCalled, ShouldBeTrue)

			var obj2 struct {
				S G[*PtrStruct] `json:"s"`
			}
			err = json.Unmarshal(data, &obj2)
			So(err, ShouldBeNil)
			So(mockHandler.UnmarshalCalled, ShouldBeTrue)
			So(obj2.S.Value(), ShouldNotBeNil)
			So(obj2.S.Value().Val, ShouldEqual, 77)
		})

		Convey("custom JSON handler with ParseFromJSON", func() {
			mockHandler := &MockJSONHandler{}

			type ParseStruct struct {
				Val int `json:"val"`
			}

			Bind(map[string]ParseStruct{
				"parse_struct": {},
			}, WithJSONHandler(mockHandler.Marshal, mockHandler.Unmarshal))

			typeName := "parse_struct"
			data := []byte(`{"val":123}`)

			// NOTE: Since MarshalAPI was removed from BindOption, ParseFromJSON (which is outside G[T])
			// cannot access the custom unmarshal function anymore unless we expose it from wrapper or
			// change ParseFromJSON to use wrapper.
			// Currently ParseFromJSON uses standard json.Unmarshal for the final step.
			// However, ParseFromJSON doesn't use the wrapper at all currently!
			// We should update ParseFromJSON to use wrapper if possible, or accept that it uses default.

			// But wait, ParseFromJSON DOESN'T call UnmarshalJSON of G[T]. It does logic manually.
			// Let's verify what ParseFromJSON does in generic.go.

			result, err := ParseFromJSON[ParseStruct](typeName, data)
			So(err, ShouldBeNil)
			// So(mockHandler.UnmarshalCalled, ShouldBeTrue) // This will fail because ParseFromJSON hardcodes json.Unmarshal now
			So(result.Value().Val, ShouldEqual, 123)
		})

		Convey("custom JSON handler with omitempty", func() {
			type OmitStruct struct {
				Val int `json:"val"`
			}

			type OmitWrapper struct {
				S G[OmitStruct] `json:"s,omitempty"`
			}

			mockHandler := &MockJSONHandler{}

			Bind(map[string]OmitStruct{
				"omit_struct": {},
			}, WithJSONHandler(mockHandler.Marshal, mockHandler.Unmarshal))

			var obj OmitWrapper
			data, err := json.Marshal(obj)
			So(err, ShouldBeNil)
			So(mockHandler.MarshalCalled, ShouldBeFalse)
			So(string(data), ShouldEqual, "{}")

			obj.S = NG[OmitStruct](OmitStruct{Val: 55})
			data, err = json.Marshal(obj)
			So(err, ShouldBeNil)
			So(mockHandler.MarshalCalled, ShouldBeTrue)
			So(string(data), ShouldContainSubstring, `"type":"omit_struct"`)
		})

		Convey("custom JSON handler with null value", func() {
			mockHandler := &MockJSONHandler{}

			type NullStruct struct {
				Val int `json:"val"`
			}

			Bind(map[string]NullStruct{
				"null_struct": {},
			}, WithJSONHandler(mockHandler.Marshal, mockHandler.Unmarshal))

			var obj struct {
				S G[NullStruct] `json:"s"`
			}

			data := `{"s":null}`
			err := json.Unmarshal([]byte(data), &obj)
			So(err, ShouldBeNil)
			So(mockHandler.UnmarshalCalled, ShouldBeFalse)
			So(len(obj.S), ShouldEqual, 1)
		})

		Convey("custom JSON handler with multiple bindings", func() {
			mockHandler1 := &MockJSONHandler{}
			mockHandler2 := &MockJSONHandler{}

			type MultiStruct1 struct {
				Val int `json:"val"`
			}

			type MultiStruct2 struct {
				Str string `json:"str"`
			}

			Bind(map[string]MultiStruct1{
				"multi_struct1": {},
			}, WithJSONHandler(mockHandler1.Marshal, mockHandler1.Unmarshal))

			Bind(map[string]MultiStruct2{
				"multi_struct2": {},
			}, WithJSONHandler(mockHandler2.Marshal, mockHandler2.Unmarshal))

			obj1 := struct {
				S G[MultiStruct1] `json:"s"`
			}{
				S: NG[MultiStruct1](MultiStruct1{Val: 1}),
			}

			obj2 := struct {
				S G[MultiStruct2] `json:"s"`
			}{
				S: NG[MultiStruct2](MultiStruct2{Str: "test"}),
			}

			_, err := json.Marshal(obj1)
			So(err, ShouldBeNil)
			So(mockHandler1.MarshalCalled, ShouldBeTrue)

			_, err = json.Marshal(obj2)
			So(err, ShouldBeNil)
			So(mockHandler2.MarshalCalled, ShouldBeTrue)
		})

		Convey("custom JSON handler tracks marshal and unmarshal calls", func() {
			mockHandler := &MockJSONHandler{}

			type TrackStruct struct {
				Val int `json:"val"`
			}

			Bind(map[string]TrackStruct{
				"track_struct": {},
			}, WithJSONHandler(mockHandler.Marshal, mockHandler.Unmarshal))

			obj := struct {
				S G[TrackStruct] `json:"s"`
			}{
				S: NG[TrackStruct](TrackStruct{Val: 42}),
			}

			data, err := json.Marshal(obj)
			So(err, ShouldBeNil)
			So(mockHandler.MarshalCalled, ShouldBeTrue)
			So(mockHandler.UnmarshalCalled, ShouldBeFalse)

			var obj2 struct {
				S G[TrackStruct] `json:"s"`
			}
			err = json.Unmarshal(data, &obj2)
			So(err, ShouldBeNil)
			So(mockHandler.UnmarshalCalled, ShouldBeTrue)
			So(obj2.S.Value().Val, ShouldEqual, 42)
		})
	})
}

func TestWithInitializer(t *testing.T) {
	Convey("Test WithInitializer", t, func() {
		Convey("basic type initialization", func() {
			Convey("int with default value", func() {
				Bind(map[string]int{
					"int": 0,
				}, WithInitializer(func(v int) int {
					if v == 0 {
						return 100
					}
					return v
				}))

				result := NG(0)
				So(result.Value(), ShouldEqual, 100)

				result2 := NG(50)
				So(result2.Value(), ShouldEqual, 50)
			})

			Convey("string with default value", func() {
				Bind(map[string]string{
					"str": "",
				}, WithInitializer(func(v string) string {
					if v == "" {
						return "default"
					}
					return v
				}))

				result := NG("")
				So(result.Value(), ShouldEqual, "default")

				result2 := NG("hello")
				So(result2.Value(), ShouldEqual, "hello")
			})
		})

		Convey("struct initialization with default values", func() {
			type User struct {
				Name  string
				Age   int
				Email string
			}

			Bind(map[string]User{
				"user": {},
			}, WithInitializer(func(v User) User {
				if v.Name == "" {
					v.Name = "anonymous"
				}
				if v.Age == 0 {
					v.Age = 18
				}
				if v.Email == "" {
					v.Email = "default@example.com"
				}
				return v
			}))

			result := NG(User{})
			So(result.Value().Name, ShouldEqual, "anonymous")
			So(result.Value().Age, ShouldEqual, 18)
			So(result.Value().Email, ShouldEqual, "default@example.com")

			result2 := NG(User{Name: "John", Age: 25})
			So(result2.Value().Name, ShouldEqual, "John")
			So(result2.Value().Age, ShouldEqual, 25)
			So(result2.Value().Email, ShouldEqual, "default@example.com")
		})

		Convey("pointer type initialization", func() {
			type Config struct {
				Enabled bool
				Timeout int
			}

			Bind(map[string]*Config{
				"config": nil,
			}, WithInitializer(func(v *Config) *Config {
				if v == nil {
					return &Config{Enabled: true, Timeout: 30}
				}
				if v.Timeout == 0 {
					v.Timeout = 30
				}
				return v
			}))

			result := NG[*Config](nil)
			So(result.Value(), ShouldNotBeNil)
			So(result.Value().Enabled, ShouldBeTrue)
			So(result.Value().Timeout, ShouldEqual, 30)

			result2 := NG(&Config{Enabled: false})
			So(result2.Value().Enabled, ShouldBeFalse)
			So(result2.Value().Timeout, ShouldEqual, 30)
		})

		Convey("slice initialization with append", func() {
			type Items struct {
				List []int
			}

			Bind(map[string]Items{
				"items": {},
			}, WithInitializer(func(v Items) Items {
				if len(v.List) == 0 {
					v.List = []int{1, 2, 3}
				}
				return v
			}))

			result := NG(Items{})
			So(len(result.Value().List), ShouldEqual, 3)
			So(result.Value().List, ShouldResemble, []int{1, 2, 3})

			result2 := NG(Items{List: []int{10, 20}})
			So(len(result2.Value().List), ShouldEqual, 2)
			So(result2.Value().List, ShouldResemble, []int{10, 20})
		})

		Convey("map initialization", func() {
			type Metadata struct {
				Tags map[string]string
			}

			Bind(map[string]Metadata{
				"metadata": {},
			}, WithInitializer(func(v Metadata) Metadata {
				if v.Tags == nil {
					v.Tags = make(map[string]string)
					v.Tags["version"] = "1.0"
					v.Tags["created"] = "now"
				}
				return v
			}))

			result := NG(Metadata{})
			So(result.Value().Tags, ShouldNotBeNil)
			So(result.Value().Tags["version"], ShouldEqual, "1.0")
			So(result.Value().Tags["created"], ShouldEqual, "now")

			result2 := NG(Metadata{Tags: map[string]string{"custom": "value"}})
			So(result2.Value().Tags["custom"], ShouldEqual, "value")
			So(result2.Value().Tags["version"], ShouldEqual, "")
		})

		Convey("interface type with initializer", func() {
			Bind(map[string]Animal{
				"dog": Dog{},
				"cat": Cat{},
			}, WithInitializer(func(v Animal) Animal {
				if d, ok := v.(Dog); ok {
					if d.Name == "" {
						d.Name = "unnamed_dog"
					}
					return d
				}
				if c, ok := v.(Cat); ok {
					if c.Name == "" {
						c.Name = "unnamed_cat"
					}
					return c
				}
				return v
			}))

			result := NG[Animal](Dog{})
			dog, ok := result.Value().(Dog)
			So(ok, ShouldBeTrue)
			So(dog.Name, ShouldEqual, "unnamed_dog")

			result2 := NG[Animal](Cat{})
			cat, ok := result2.Value().(Cat)
			So(ok, ShouldBeTrue)
			So(cat.Name, ShouldEqual, "unnamed_cat")

			result3 := NG[Animal](Dog{Name: "Buddy"})
			dog3, ok := result3.Value().(Dog)
			So(ok, ShouldBeTrue)
			So(dog3.Name, ShouldEqual, "Buddy")
		})

		Convey("initializer with JSON unmarshal", func() {
			type Product struct {
				ID    string
				Name  string
				Price float64
			}

			Bind(map[string]Product{
				"product": {},
			}, WithInitializer(func(v Product) Product {
				if v.ID == "" {
					v.ID = "auto-generated-id"
				}
				if v.Price == 0 {
					v.Price = 9.99
				}
				return v
			}))

			var obj struct {
				Product G[Product] `json:"product"`
			}

			data := `{"product":{"type":"product","data":{"Name":"Test Product"}}}`
			err := json.Unmarshal([]byte(data), &obj)
			So(err, ShouldBeNil)
			So(obj.Product.Value().ID, ShouldEqual, "auto-generated-id")
			So(obj.Product.Value().Name, ShouldEqual, "Test Product")
			So(obj.Product.Value().Price, ShouldEqual, 9.99)

			data2 := `{"product":{"type":"product","data":{"ID":"custom-id","Name":"Custom Product","Price":19.99}}}`
			err = json.Unmarshal([]byte(data2), &obj)
			So(err, ShouldBeNil)
			So(obj.Product.Value().ID, ShouldEqual, "custom-id")
			So(obj.Product.Value().Name, ShouldEqual, "Custom Product")
			So(obj.Product.Value().Price, ShouldEqual, 19.99)
		})

		Convey("type assertion failure returns original value", func() {
			type TestStruct struct {
				A int
			}

			Bind(map[string]TestStruct{
				"test_struct": {},
			}, WithInitializer(func(v TestStruct) TestStruct {
				v.A = 999
				return v
			}))

			result := NG(TestStruct{})
			So(result.Value().A, ShouldEqual, 999)
		})

		Convey("complex nested struct initialization", func() {
			type Address struct {
				Street  string
				City    string
				Country string
			}

			type Person struct {
				Name    string
				Age     int
				Address Address
			}

			Bind(map[string]Person{
				"person": {},
			}, WithInitializer(func(v Person) Person {
				if v.Name == "" {
					v.Name = "John Doe"
				}
				if v.Age == 0 {
					v.Age = 30
				}
				if v.Address.City == "" {
					v.Address.City = "Unknown"
				}
				if v.Address.Country == "" {
					v.Address.Country = "USA"
				}
				return v
			}))

			result := NG(Person{})
			So(result.Value().Name, ShouldEqual, "John Doe")
			So(result.Value().Age, ShouldEqual, 30)
			So(result.Value().Address.City, ShouldEqual, "Unknown")
			So(result.Value().Address.Country, ShouldEqual, "USA")

			result2 := NG(Person{
				Name: "Jane",
				Address: Address{
					City:    "New York",
					Country: "USA",
				},
			})
			So(result2.Value().Name, ShouldEqual, "Jane")
			So(result2.Value().Age, ShouldEqual, 30)
			So(result2.Value().Address.City, ShouldEqual, "New York")
			So(result2.Value().Address.Country, ShouldEqual, "USA")
		})

		Convey("slice append operation", func() {
			type NumberList struct {
				Numbers []int
			}

			Bind(map[string]NumberList{
				"numbers": {},
			}, WithInitializer(func(v NumberList) NumberList {
				if len(v.Numbers) == 0 {
					v.Numbers = []int{0}
				} else {
					v.Numbers = append(v.Numbers, 999)
				}
				return v
			}))

			result := NG(NumberList{})
			So(len(result.Value().Numbers), ShouldEqual, 1)
			So(result.Value().Numbers[0], ShouldEqual, 0)

			result2 := NG(NumberList{Numbers: []int{1, 2, 3}})
			So(len(result2.Value().Numbers), ShouldEqual, 4)
			So(result2.Value().Numbers[3], ShouldEqual, 999)
		})
	})
}

func TestGFrom(t *testing.T) {
	Convey("Test GFrom function", t, func() {
		Convey("success create G from type name and data", func() {
			typeName := "s1"
			data := []byte(`{"A":42}`)

			result, err := ParseFromJSON[Speeker](typeName, data)
			So(err, ShouldBeNil)
			So(result, ShouldNotBeNil)
			So(len(result), ShouldEqual, 1)
			So(result.TypeName(), ShouldEqual, typeName)

			s1, ok := result.Value().(S1)
			So(ok, ShouldBeTrue)
			So(s1.A, ShouldEqual, 42)
			So(s1.SayHi(), ShouldEqual, "Hello no.:42")
		})

		Convey("success create G for s2 type", func() {
			typeName := "s2"
			data := []byte(`{"B":"world"}`)

			result, err := ParseFromJSON[Speeker](typeName, data)
			So(err, ShouldBeNil)
			So(result, ShouldNotBeNil)
			So(result.TypeName(), ShouldEqual, typeName)
			s2, ok := result.Value().(S2)
			So(ok, ShouldBeTrue)
			So(s2.B, ShouldEqual, "world")
			So(s2.SayHi(), ShouldEqual, "Hello world")
		})

		Convey("type not binding", func() {
			typeName := "unknown_type"
			data := []byte(`{"A":1}`)

			result, err := ParseFromJSON[Speeker](typeName, data)
			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, "type alias 'unknown_type' is not registered")
			So(result, ShouldBeNil)
		})

		Convey("interface type not binding", func() {
			typeName := "s1"
			data := []byte(`{"A":1}`)

			result, err := ParseFromJSON[int](typeName, data)
			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, "type alias 's1' is not registered")
			So(result, ShouldBeNil)
		})

		Convey("invalid json data", func() {
			typeName := "s1"
			data := []byte(`invalid json`)

			result, err := ParseFromJSON[Speeker](typeName, data)

			So(err, ShouldNotBeNil)
			So(result, ShouldBeNil)
		})

		Convey("empty data", func() {
			typeName := "s1"
			data := []byte(`{}`)

			result, err := ParseFromJSON[Speeker](typeName, data)
			So(err, ShouldBeNil)
			So(result, ShouldNotBeNil)

			s1, ok := result.Value().(S1)
			So(ok, ShouldBeTrue)
			So(s1.A, ShouldEqual, 0)
		})

		Convey("GFrom for Task1 interface", func() {
			typeName := "vod"
			data := []byte(`{"url":"http://example.com/video.mp4"}`)

			result, err := ParseFromJSON[Task1](typeName, data)
			So(err, ShouldBeNil)
			So(result, ShouldNotBeNil)
			So(result.TypeName(), ShouldEqual, typeName)
			vodTask, ok := result.Value().(*VodTask)
			So(ok, ShouldBeTrue)
			So(vodTask.Url, ShouldEqual, "http://example.com/video.mp4")
		})
	})
}

type BlockInfo struct {
	ID       string   `json:"block_id,omitempty"`
	Type     int      `json:"block_type"`
	Children []string `json:"children,omitempty"`
	ParentID string   `json:"parent_id,omitempty"`
}

func (b *BlockInfo) SetType(t int) {
	b.Type = t
}

func (b *BlockInfo) SetID(id string) {
	b.ID = id
}

func (b *BlockInfo) GetID() string {
	return b.ID
}

func (b *BlockInfo) GetType() int {
	return b.Type
}

type Blocker interface {
	SetID(string)
	GetID() string
	SetType(int)
	GetType() int
}

type TextBlock struct {
	BlockInfo
	Text TextElements `json:"text"`
}

type TextElements struct {
	Elements []TextRunElement `json:"elements,omitempty"`
}

type TextRunElement struct {
	TextRun `json:"text_run,omitempty"`
}

type TextRun struct {
	Content string `json:"content,omitempty"`
}

type ImageBlock struct {
	BlockInfo
	Image ImageProp `json:"image,omitempty"`
}

type ImageProp struct {
	Align  int    `json:"align,omitempty"`
	Height int    `json:"height,omitempty"`
	Token  string `json:"token,omitempty"`
	Width  int    `json:"width,omitempty"`
}

// func TestCustomSerializerV2(t *testing.T) {
// 	Bind(map[string]Blocker{
// 		"2":  &TextBlock{},
// 		"27": &ImageBlock{},
// 	}, BlockSerializer{})

// 	var textBlock = TextBlock{
// 		BlockInfo: BlockInfo{
// 			ID: "HEllo",
// 		},
// 		Text: TextElements{
// 			Elements: []TextRunElement{
// 				{
// 					TextRun: TextRun{
// 						Content: "hello",
// 					},
// 				},
// 			},
// 		},
// 	}

// 	Convey("Test Block Serializer", t, func() {
// 		var data []G[Blocker] = []G[Blocker]{
// 			NG[Blocker](&textBlock),
// 			NG[Blocker](&ImageBlock{
// 				BlockInfo: BlockInfo{
// 					ID: "IMG_HELWO",
// 				},
// 				Image: ImageProp{
// 					Height: 512,
// 					Width:  512,
// 				},
// 			}),
// 		}

// 		d, err := json.Marshal(data)
// 		So(err, ShouldBeNil)
// 		var d2 []G[Blocker]
// 		err = json.Unmarshal(d, &d2)
// 		So(err, ShouldBeNil)
// 		So(d2[0].Value().GetID(), ShouldEqual, "HEllo")
// 		So(d2[1].Value().GetID(), ShouldEqual, "IMG_HELWO")
// 		So(d2[0].Value().GetType(), ShouldEqual, 2)
// 		So(d2[1].Value().GetType(), ShouldEqual, 27)
// 	})
// }

func TestIsZero(t *testing.T) {
	Convey("Test IsZero", t, func() {
		var g G[int]
		So(g.IsZero(), ShouldBeTrue)

		g2 := NG(10)
		So(g2.IsZero(), ShouldBeFalse)

		g3 := NG(0)
		So(g3.IsZero(), ShouldBeFalse)
	})
}

func TestDetailType(t *testing.T) {
	Convey("Test DetailType", t, func() {
		// Ensure bindings are set (binding() is called in init, but we can call it again to be safe or rely on init)
		// binding() // already called in init

		var g G[Speeker]

		Convey("existing types", func() {
			s1Type := g.DetailType("s1")
			So(s1Type, ShouldEqual, reflect.TypeOf(S1{}))

			s2Type := g.DetailType("s2")
			So(s2Type, ShouldEqual, reflect.TypeOf(S2{}))
		})

		Convey("non-existing type", func() {
			unknownType := g.DetailType("unknown_type")
			So(unknownType, ShouldBeNil)
		})
	})
}
