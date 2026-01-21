package xjson

import (
	"encoding/json"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

type ComboStruct struct {
	ID   string `json:"id"`
	Data string `json:"data"`
}

type InitStruct struct {
	Val int `json:"val"`
}

type InitStruct2 struct {
	Val int `json:"val"`
}

type ComboStruct2 struct {
	ID   string `json:"id"`
	Data string `json:"data"`
}

type ComboStruct3 struct {
	ID   string `json:"id"`
	Data string `json:"data"`
}

func TestOptionCombinations(t *testing.T) {
	Convey("Test BindOption Combinations", t, func() {

		Convey("WithFlatLayout + WithTypeKey", func() {
			// WithFlatLayout uses the typeKey passed to it.
			// The typeKey is configured in BindOption.
			// We need to ensure that WithTypeKey sets the key, and WithFlatLayout's wrapper uses it.
			
			Bind(map[string]ComboStruct{
				"combo_flat_type": {},
			}, WithFlatLayout(), WithTypeKey("kind"))

			// Marshal
			obj := struct {
				S G[ComboStruct] `json:"s"`
			}{
				S: NG(ComboStruct{ID: "1", Data: "test"}),
			}
			data, err := json.Marshal(obj)
			So(err, ShouldBeNil)
			// Expect: {"s":{"kind":"combo_flat_type","id":"1","data":"test"}}
			So(string(data), ShouldContainSubstring, `"kind":"combo_flat_type"`)
			So(string(data), ShouldContainSubstring, `"id":"1"`)

			// Unmarshal
			jsonStr := `{"s":{"kind":"combo_flat_type","id":"2","data":"check"}}`
			var res struct {
				S G[ComboStruct] `json:"s"`
			}
			err = json.Unmarshal([]byte(jsonStr), &res)
			So(err, ShouldBeNil)
			So(res.S.Value().ID, ShouldEqual, "2")
			So(res.S.Value().Data, ShouldEqual, "check")
		})

		Convey("WithFlatLayout + WithInitializer", func() {
			Bind(map[string]InitStruct{
				"combo_flat_init": {},
			}, WithFlatLayout(), WithInitializer(func(v InitStruct) InitStruct {
				if v.Val == 0 {
					v.Val = 100
				}
				return v
			}))

			// Unmarshal with zero value -> should trigger initializer
			// jsonStr := `{"type":"combo_flat_init"}` // val missing, so 0
			// Note: for G[T], we usually unmarshal into a field
			wrapperStr := `{"s":{"type":"combo_flat_init"}}`
			
			var res struct {
				S G[InitStruct] `json:"s"`
			}
			err := json.Unmarshal([]byte(wrapperStr), &res)
			So(err, ShouldBeNil)
			So(res.S.Value().Val, ShouldEqual, 100)

			// Unmarshal with value -> should keep value
			wrapperStr2 := `{"s":{"type":"combo_flat_init","val":50}}`
			err = json.Unmarshal([]byte(wrapperStr2), &res)
			So(err, ShouldBeNil)
			So(res.S.Value().Val, ShouldEqual, 50)
		})

		Convey("WithJSONHandler + WithInitializer", func() {
			mockHandler := &MockJSONHandler{}
			
			Bind(map[string]InitStruct2{
				"combo_handler_init": {},
			}, WithJSONHandler(mockHandler.Marshal, mockHandler.Unmarshal), WithInitializer(func(v InitStruct2) InitStruct2 {
				if v.Val < 0 {
					v.Val = 0
				}
				return v
			}))

			// Unmarshal
			jsonStr := `{"s":{"type":"combo_handler_init","data":{"val":-5}}}`
			var res struct {
				S G[InitStruct2] `json:"s"`
			}
			err := json.Unmarshal([]byte(jsonStr), &res)
			So(err, ShouldBeNil)
			So(mockHandler.UnmarshalCalled, ShouldBeTrue)
			So(res.S.Value().Val, ShouldEqual, 0)
		})

		Convey("WithFlatLayout Override (Last Wins)", func() {
			// If we call WithJSONHandler THEN WithFlatLayout, FlatLayout should win (overwrite wrappers)
			Bind(map[string]ComboStruct2{
				"combo_override": {},
			}, WithJSONHandler(json.Marshal, json.Unmarshal), WithFlatLayout())

			obj := struct {
				S G[ComboStruct2] `json:"s"`
			}{
				S: NG(ComboStruct2{ID: "99"}),
			}
			data, err := json.Marshal(obj)
			So(err, ShouldBeNil)
			// Should be flat
			So(string(data), ShouldContainSubstring, `"type":"combo_override"`)
			So(string(data), ShouldContainSubstring, `"id":"99"`)
			So(string(data), ShouldNotContainSubstring, `"data":{`) // Should not have nested data
		})
		
		Convey("WithFlatLayout + WithWrapper (Manual Override)", func() {
		    // If user provides a custom wrapper AFTER FlatLayout, custom wrapper wins
		    customMarshal := func(typeKey, typeName, valueKey string, value any) ([]byte, error) {
		        return []byte(`"custom_override"`), nil
		    }
		    
		    Bind(map[string]ComboStruct3{
		        "combo_manual": {},
		    }, WithFlatLayout(), WithWrapper(customMarshal, nil))
		    
		    obj := struct {
				S G[ComboStruct3] `json:"s"`
			}{
				S: NG(ComboStruct3{ID: "1"}),
			}
			data, err := json.Marshal(obj)
			So(err, ShouldBeNil)
			So(string(data), ShouldContainSubstring, `"custom_override"`)
		})
	})
}
