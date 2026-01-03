package xjson

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"
)

type StructPeople struct {
	Name Optional[string] `json:"name,omitempty"`
	Age  Optional[int]    `json:"age,omitempty"`
}

var Marshal = json.Marshal
var Unmarshal = json.Unmarshal

func TestOptionalWithOutSet(t *testing.T) {

	Convey("没有设置过字段值序列化", t, func() {
		people := StructPeople{}
		data, err := Marshal(people)
		So(err, ShouldBeNil)
		So(string(data), ShouldEqual, "{}")
	})

	Convey("没有设置过字段值序列化", t, func() {
		people := &StructPeople{}
		data, err := Marshal(people)
		So(err, ShouldBeNil)
		So(string(data), ShouldEqual, "{}")
	})
}

func TestOptionalWithSet(t *testing.T) {
	Convey("初始化属性值序列化测试", t, func() {
		people := StructPeople{
			Name: NO("Young"),
			Age:  NO(12),
		}
		data, err := Marshal(people)
		So(err, ShouldBeNil)

		var dataMap map[string]interface{}
		err = Unmarshal(data, &dataMap)
		So(err, ShouldBeNil)
		So(len(dataMap), ShouldEqual, 2)
		So(dataMap["name"], ShouldEqual, "Young")
		So(dataMap["age"], ShouldEqual, 12)
	})

	Convey("初始化属性值序列化测试", t, func() {
		people := &StructPeople{
			Name: NO("Young"),
			Age:  NO(12),
		}
		data, err := Marshal(people)
		So(err, ShouldBeNil)

		var dataMap map[string]interface{}
		err = Unmarshal(data, &dataMap)
		So(err, ShouldBeNil)
		So(len(dataMap), ShouldEqual, 2)
		So(dataMap["name"], ShouldEqual, "Young")
		So(dataMap["age"], ShouldEqual, 12)
	})
}

func TestUnmarshalWithNull(t *testing.T) {

	Convey("反序列测试之null", t, func() {
		data := []byte(`
		{
			"name":null,
			"age":null
		}
		`)
		var people StructPeople
		err := Unmarshal(data, &people)
		So(err, ShouldBeNil)

		So(people.Age.Value(), ShouldEqual, 0)
		So(people.Name.Value(), ShouldEqual, "")
		So(people.Age.IsNull(), ShouldEqual, true)
		So(people.Name.IsNull(), ShouldEqual, true)
		So(people.Age.IsUndefined(), ShouldBeFalse)
		// So(people.Age.Mode(), ShouldEqual, meta.Null)
		// So(people.Name.Mode(), ShouldEqual, meta.Null)
	})

	Convey("反序列测试之null", t, func() {
		data := []byte(`
		{
			"name":null,
			"age":null
		}
		`)
		var people *StructPeople
		err := Unmarshal(data, &people)
		So(err, ShouldBeNil)

		So(people.Age.Value(), ShouldEqual, 0)
		So(people.Name.Value(), ShouldEqual, "")
		So(people.Age.IsNull(), ShouldEqual, true)
		So(people.Name.IsNull(), ShouldEqual, true)

		// So(people.Age.Mode(), ShouldEqual, meta.Null)
		// So(people.Name.Mode(), ShouldEqual, meta.Null)
	})

	Convey("反序列化之没有设置过KEY", t, func() {
		data := []byte(`
		{
		}
		`)

		var people StructPeople
		err := Unmarshal(data, &people)
		So(err, ShouldBeNil)

		So(people.Age.Value(), ShouldEqual, 0)
		So(people.Name.Value(), ShouldEqual, "")

		So(people.Age.IsUndefined(), ShouldBeTrue)
		So(people.Name.IsUndefined(), ShouldBeTrue)

		// So(people.Age.Mode(), ShouldEqual, meta.Undefined)
		// So(people.Name.Mode(), ShouldEqual, meta.Undefined)
	})

	Convey("反序列化之没有设置过KEY", t, func() {
		data := []byte(`
		{
		}
		`)

		var people *StructPeople
		err := Unmarshal(data, &people)
		So(err, ShouldBeNil)

		So(people.Age.Value(), ShouldEqual, 0)
		So(people.Name.Value(), ShouldEqual, "")

		So(people.Age.IsUndefined(), ShouldBeTrue)
		So(people.Name.IsUndefined(), ShouldBeTrue)
		// So(people.Age.Mode(), ShouldEqual, meta.Undefined)
		// So(people.Name.Mode(), ShouldEqual, meta.Undefined)
	})

	Convey("正常反序列化测试", t, func() {
		data := []byte(`
		{
			"name":"young",
			"age":22
		}
		`)

		var people StructPeople
		err := Unmarshal(data, &people)
		So(err, ShouldBeNil)

		So(people.Age.Value(), ShouldEqual, 22)
		So(people.Name.Value(), ShouldEqual, "young")

		So(people.Age.IsUndefined(), ShouldBeFalse)
		So(people.Name.IsUndefined(), ShouldBeFalse)

		// So(people.Age.Mode(), ShouldEqual, meta.Default)
		// So(people.Name.Mode(), ShouldEqual, meta.Default)
	})

	Convey("正常反序列化测试", t, func() {
		data := []byte(`
		{
			"name":"young",
			"age":22
		}
		`)

		var people *StructPeople
		err := Unmarshal(data, &people)
		So(err, ShouldBeNil)

		So(people.Age.Value(), ShouldEqual, 22)
		So(people.Name.Value(), ShouldEqual, "young")

		So(people.Age.IsUndefined(), ShouldBeFalse)
		So(people.Name.IsUndefined(), ShouldBeFalse)
		// So(people.Age.Mode(), ShouldEqual, meta.Default)
		// So(people.Name.Mode(), ShouldEqual, meta.Default)
	})

}

type StructPeopleNoOmit struct {
	Name Optional[string] `json:"name"`
	Age  Optional[int]    `json:"age"`
}

func TestMarshalStructWithOutOmit(t *testing.T) {
	Convey("如果未设置omitempty,则字段值会被设置成null", t, func() {
		p := StructPeopleNoOmit{}
		data, err := Marshal(p)
		So(err, ShouldBeNil)
		So(string(data), ShouldEqual, string([]byte(`{"name":null,"age":null}`)))
	})

	Convey("如果未设置omitempty,则字段值会被设置成null", t, func() {
		p := &StructPeopleNoOmit{}
		data, err := Marshal(p)
		So(err, ShouldBeNil)
		So(string(data), ShouldEqual, string([]byte(`{"name":null,"age":null}`)))
	})

	Convey("正常序列化不受影响测试", t, func() {
		p := StructPeopleNoOmit{
			Name: NO("TEST"),
			Age:  NO(88),
		}
		data, err := Marshal(p)
		So(err, ShouldBeNil)
		So(string(data), ShouldEqual, string([]byte(`{"name":"TEST","age":88}`)))
	})

	Convey("正常序列化不受影响测试", t, func() {
		p := &StructPeopleNoOmit{
			Name: NO("TEST"),
			Age:  NO(88),
		}
		data, err := Marshal(p)
		So(err, ShouldBeNil)
		So(string(data), ShouldEqual, string([]byte(`{"name":"TEST","age":88}`)))
	})
}

func TestUnmarshalWithOutOmit(t *testing.T) {
	Convey("反序列化测试之null1", t, func() {
		data := []byte(`
		{
			"name":null,
			"age":null
		}
		`)
		var people StructPeopleNoOmit
		err := Unmarshal(data, &people)
		So(err, ShouldBeNil)

		So(people.Age.Value(), ShouldEqual, 0)
		So(people.Name.Value(), ShouldEqual, "")

		So(people.Age.IsNull(), ShouldEqual, true)
		So(people.Name.IsNull(), ShouldEqual, true)

		// So(people.Age.Mode(), ShouldEqual, meta.Null)
		// So(people.Name.Mode(), ShouldEqual, meta.Null)
	})

	Convey("反序列化测试之null2", t, func() {
		data := []byte(`
		{
			"name":null,
			"age":null
		}
		`)
		var people *StructPeopleNoOmit
		err := Unmarshal(data, &people)
		So(err, ShouldBeNil)

		So(people.Age.Value(), ShouldEqual, 0)
		So(people.Name.Value(), ShouldEqual, "")

		So(people.Age.IsNull(), ShouldEqual, true)
		So(people.Name.IsNull(), ShouldEqual, true)
		// So(people.Age.Mode(), ShouldEqual, meta.Null)
		// So(people.Name.Mode(), ShouldEqual, meta.Null)
	})

	Convey("反序列化之没有设置过KEY", t, func() {
		data := []byte(`
		{
		}
		`)

		var people StructPeopleNoOmit
		err := Unmarshal(data, &people)
		So(err, ShouldBeNil)

		So(people.Age.Value(), ShouldEqual, 0)
		So(people.Name.Value(), ShouldEqual, "")

		So(people.Age.IsUndefined(), ShouldBeTrue)
		So(people.Name.IsUndefined(), ShouldBeTrue)

		// So(people.Age.Mode(), ShouldEqual, meta.Undefined)
		// So(people.Name.Mode(), ShouldEqual, meta.Undefined)
	})

	Convey("反序列化之没有设置过KEY", t, func() {
		data := []byte(`
		{
		}
		`)

		var people *StructPeopleNoOmit
		err := Unmarshal(data, &people)
		So(err, ShouldBeNil)

		So(people.Age.Value(), ShouldEqual, 0)
		So(people.Name.Value(), ShouldEqual, "")

		So(people.Age.IsUndefined(), ShouldBeTrue)
		So(people.Name.IsUndefined(), ShouldBeTrue)

		// So(people.Age.Mode(), ShouldEqual, meta.Undefined)
		// So(people.Name.Mode(), ShouldEqual, meta.Undefined)
	})

	Convey("正常反序列化测试", t, func() {
		data := []byte(`
		{
			"name":"young",
			"age":22
		}
		`)

		var people StructPeopleNoOmit
		err := Unmarshal(data, &people)
		So(err, ShouldBeNil)

		So(people.Age.Value(), ShouldEqual, 22)
		So(people.Name.Value(), ShouldEqual, "young")

		So(people.Age.IsUndefined(), ShouldBeFalse)
		So(people.Name.IsUndefined(), ShouldBeFalse)

		// So(people.Age.Mode(), ShouldEqual, meta.Default)
		// So(people.Name.Mode(), ShouldEqual, meta.Default)
	})

	Convey("正常反序列化测试", t, func() {
		data := []byte(`
		{
			"name":"young",
			"age":22
		}
		`)

		var people *StructPeopleNoOmit
		err := Unmarshal(data, &people)
		So(err, ShouldBeNil)

		So(people.Age.Value(), ShouldEqual, 22)
		So(people.Name.Value(), ShouldEqual, "young")

		So(people.Age.IsUndefined(), ShouldBeFalse)
		So(people.Name.IsUndefined(), ShouldBeFalse)

		// So(people.Age.Mode(), ShouldEqual, meta.Default)
		// So(people.Name.Mode(), ShouldEqual, meta.Default)
	})

}

type PointerPeople struct {
	Name *Optional[string] `json:"name,omitempty"`
	Age  *Optional[int]    `json:"age,omitempty"`
}

func Ref[T any](data T) *T {
	return &data
}

func TestNormalMarshalPointer(t *testing.T) {

	Convey("正常序列化测试", t, func() {
		people := PointerPeople{
			Name: Ref(NO("Hello")),
			Age:  Ref(NO(12)),
		}

		data, err := Marshal(people)
		So(err, ShouldBeNil)
		So(string(data), ShouldEqual, string([]byte(`{"name":"Hello","age":12}`)))

	})

	Convey("初始值序列化测试", t, func() {
		people := PointerPeople{}

		data, err := Marshal(people)
		So(err, ShouldBeNil)
		So(string(data), ShouldEqual, string([]byte(`{}`)))
	})
}

func TestUnMarshalPointer(t *testing.T) {
	Convey("正常反序列化测试", t, func() {
		data := []byte(`
		{
			"name":"young",
			"age":22
		}
		`)

		var people PointerPeople
		err := Unmarshal(data, &people)
		So(err, ShouldBeNil)

		So(people.Age.Value(), ShouldEqual, 22)
		So(people.Name.Value(), ShouldEqual, "young")

		So(people.Age.IsUndefined(), ShouldBeFalse)
		So(people.Name.IsUndefined(), ShouldBeFalse)
		// So(people.Age.Mode(), ShouldEqual, meta.Default)
		// So(people.Name.Mode(), ShouldEqual, meta.Default)
	})

	Convey("正常反序列化测试null", t, func() {
		data := []byte(`
		{
			"name":null,
			"age":null
		}
		`)

		var people PointerPeople
		err := Unmarshal(data, &people)
		So(err, ShouldBeNil)

		So(people.Age, ShouldBeNil)
		So(people.Name, ShouldBeNil)
	})
	Convey("正常反序列化测试-未设置", t, func() {
		data := []byte(`
		{
		}
		`)

		var people PointerPeople
		err := Unmarshal(data, &people)
		So(err, ShouldBeNil)

		So(people.Age, ShouldBeNil)
		So(people.Name, ShouldBeNil)
	})

	Convey("Clear测试", t, func() {
		p := StructPeople{
			Name: NO("TEST"),
			Age:  NO(88),
		}
		// p.Name.Clear() // Clear not implemented in Optional
		p.Name = Undefined[string]()

		d, err := json.Marshal(p)
		So(err, ShouldEqual, nil)
		var m map[string]interface{}
		err = json.Unmarshal(d, &m)
		So(err, ShouldEqual, nil)
		So(m["age"], ShouldEqual, 88)
		_, exist := m["name"]
		So(exist, ShouldEqual, false)
	})

}

type Student2 struct {
	Name string
}

func (stu Student2) GetName() string {
	return stu.Name
}

func Update[T any](target *T) {
	v := &target
	*v = nil
}

type Range[T any] [2]T

func (rg *Range[T]) From() T {
	return rg[0]
}

func (rg *Range[T]) To() T {
	return rg[1]
}

func update(source *[]string, value string) {
	*source = append(*source, value)
}

type TimeTT struct {
	Start time.Time `json:"start"`
}

type A[T any] []T

func (a A[T]) Empty() bool {
	return true
}

type Stu struct {
	Test A[string] `json:"test,omitempty"`
}

type B struct {
	Name string
}

func (b *B) Test() {
	fmt.Println("bca")
}

func TT(b *B) {
	// fmt.Println(b.Name)
	b.Test()
}

func TestTemp(t *testing.T) {
	TT(nil)

}

type indexData[T any] struct {
	index uint
	data  T
}

func DistinctBy[T any, C comparable](items []T, fn func(T) C, selector func(pre T, cur T) T) []T {
	itemMap := make(map[C]indexData[T])
	result := make([]T, 0)
	for _, item := range items {
		key := fn(item)
		if data, exist := itemMap[key]; exist {
			if selector != nil {
				result[data.index] = selector(data.data, item)
			}
			continue
		}
		itemMap[key] = indexData[T]{uint(len(result)), item}
		result = append(result, item)
	}
	return result
}

func TestHello(t *testing.T) {

	type MyStu struct {
		Name Optional[string] `json:"name,omitempty"`
	}

	s := MyStu{
		Name: NO("Hello"),
	}
	fmt.Println("name:", s.Name.Value())
	// s.Name.Clear()
	s.Name = Undefined[string]()

	fmt.Println("name:", s.Name.Value())

	data, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	fmt.Println(string(data))

	fmt.Println('a' - 'A')
	fmt.Println(('a' - 'A') | 'E')
}

type Runable interface {
	Run(ctx context.Context) error
}

type Task struct {
	Context Runable `json:"context,omitempty"`
}

type Man struct {
	Name string `json:"name"`
}

func (man Man) Run(ctx context.Context) error {
	return nil
}

// func TestRunable(t *testing.T) {
// 	test := Task{
// 		Context: Man{},
// 	}
// 	data, err := json.Marshal(test)
// 	fmt.Println("data:", string(data))
// 	if err != nil {
// 		panic(err)
// 	}
// 	err = json.Unmarshal(data, &test)
// 	if err != nil {
// 		panic(err)
// 	}
//
// }

func TestOmitEmptyBehavior(t *testing.T) {
	type OmitStruct struct {
		Name Optional[string] `json:"name,omitempty"`
	}

	Convey("omitempty测试：结构体值类型（Undefined）", t, func() {
		// Optional[T] 现改为切片类型，零值为 nil。
		// Go json 库对 nil 切片会应用 omitempty，因此字段会被忽略。
		s := OmitStruct{Name: Undefined[string]()}
		data, err := json.Marshal(s)
		So(err, ShouldBeNil)
		So(string(data), ShouldEqual, `{}`)
	})

	Convey("omitempty测试：有值但值为零值（空字符串）", t, func() {
		s := OmitStruct{Name: NO("")}
		data, err := json.Marshal(s)
		So(err, ShouldBeNil)
		// status=StatusValue, val="" -> json.Marshal("") -> ""
		So(string(data), ShouldEqual, `{"name":""}`)
	})

	Convey("omitempty测试：显式 Null", t, func() {
		s := OmitStruct{Name: Null[string]()}
		data, err := json.Marshal(s)
		So(err, ShouldBeNil)
		So(string(data), ShouldEqual, `{"name":null}`)
	})

	type OmitPointerStruct struct {
		Name *Optional[string] `json:"name,omitempty"`
	}

	Convey("omitempty测试：nil 指针", t, func() {
		s := OmitPointerStruct{Name: nil}
		data, err := json.Marshal(s)
		So(err, ShouldBeNil)
		So(string(data), ShouldEqual, `{}`)
	})

	Convey("omitempty测试：指向 Undefined 的指针", t, func() {
		// 指针不为 nil，指向一个 Undefined 的 Optional
		// MarshalJSON 会被调用，返回 "null"
		opt := Undefined[string]()
		s := OmitPointerStruct{Name: &opt}
		data, err := json.Marshal(s)
		So(err, ShouldBeNil)
		So(string(data), ShouldEqual, `{"name":null}`)
	})
}
