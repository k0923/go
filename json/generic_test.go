package json

import (
	"encoding/json"
	"fmt"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

type Speeker interface {
	SayHi() string
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
		So(string(data), ShouldEqual, `{"s":{"type":"s1","data":{"A":0}}}`)
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
		Convey("nil with type", func() {
			var s *S1
			obj := SpeakObj{
				S: NG[Speeker](s),
			}
			data, err := json.Marshal(obj)
			So(err, ShouldBeNil)
			So(string(data), ShouldEqual, `{"s":null}`)
			So(obj.S, ShouldNotBeNil)
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
			So(obj.S, ShouldBeNil)
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
		fmt.Println(hub)
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
	Data G[S1] `json:"data"`
}
