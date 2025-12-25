package json

import (
	"context"
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
			So(err.Error(), ShouldContainSubstring, "type:unknown_type is not binding")
			So(result, ShouldBeNil)
		})

		Convey("interface type not binding", func() {
			typeName := "s1"
			data := []byte(`{"A":1}`)

			result, err := ParseFromJSON[int](typeName, data)
			So(err, ShouldNotBeNil)
			So(err.Error(), ShouldContainSubstring, "is not binding")
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
