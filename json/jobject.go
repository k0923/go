package xjson

import (
	"encoding/json"
)

type NonExist struct{}

type JSONObject interface {
	Value() interface{}
	Get(path string) JSONObject
	Parent() JSONObject
	Root() JSONObject
}

type jsonObject struct {
	data   interface{}
	parent *jsonObject
	root   *jsonObject
}

func (j *jsonObject) Parent() JSONObject {
	return j.parent
}

func (j *jsonObject) Root() JSONObject {
	return j.root
}

func (j *jsonObject) Value() interface{} {
	return j.data
}

func getData(target interface{}, paths []jsonPath) interface{} {
	needSpilt := false
	result := target
	for _, path := range paths {
		if needSpilt {
			if data, ok := result.([]interface{}); ok {
				newResult := make([]interface{}, 0)
				for _, v := range data {
					subResult := path.Get(v)
					if _, ok := subResult.(NonExist); !ok {
						newResult = append(newResult, subResult)
					}
				}
				result = newResult
			}
		} else {
			result = path.Get(result)
		}
		if needSpilt == false && isSplitPath(path) {
			needSpilt = true
		}
	}
	if _, ok := result.(NonExist); ok {
		return nil
	}

	return result
}

func (j *jsonObject) Get(path string) JSONObject {
	jsonPath, err := buildJsonPath(path)
	if err != nil {
		return &jsonObject{
			data:   nil,
			root:   j.root,
			parent: j,
		}
	}
	result := getData(j.data, jsonPath)
	return &jsonObject{
		data:   result,
		root:   j.root,
		parent: j,
	}
}

func NewJSONObjectByString(jsonStr string) (JSONObject, error) {
	var data interface{}
	if err := json.Unmarshal([]byte(jsonStr), &data); err != nil {
		return nil, err
	}

	obj := &jsonObject{
		data: data,
	}
	obj.root = obj

	return obj, nil
}

func NewJSONObject(data interface{}) JSONObject {
	obj := &jsonObject{data: data}
	obj.root = obj
	return obj
}

type jsonPath interface {
	Get(target interface{}) interface{}
}

type propertyPath string

func (p propertyPath) Get(target interface{}) interface{} {
	if d, ok := target.(map[string]interface{}); ok {
		if result, exist := d[string(p)]; exist {
			return result
		} else {
			return NonExist{}
		}
	}
	return NonExist{}

}

type indexPath int

func (p indexPath) Get(target interface{}) interface{} {
	if d, ok := target.([]interface{}); ok {
		if int(p) >= len(d) {
			return NonExist{}
		}
		if int(p) < 0 {
			if int(p) < -len(d) {
				return NonExist{}
			}
			return d[len(d)+int(p)]
		}
		return d[int(p)]
	}
	return NonExist{}
}

type slicePath struct {
	from     *int
	to       *int
	step     *int
	hasColon bool
}

func (p *slicePath) AddNum(data int) {
	if p.from == nil {
		p.from = &data
	} else if p.to == nil {
		p.to = &data
	} else if p.step == nil {
		p.step = &data
	}
}

func (p *slicePath) AddColon() {
	p.hasColon = true
	if p.from == nil {
		from := 0
		p.from = &from
	}
}

func (p *slicePath) build() jsonPath {
	// [:], []
	if p.hasColon || p.from == nil {
		return *p
	}
	return indexPath(*p.from)

}

func (p *slicePath) getFrom(size int) int {
	if p.from == nil || size == 0 || *p.from == 0 {
		return 0
	}
	if *p.from > 0 {
		return *p.from
	} else {
		if *p.from < -size {
			return 0
		}
		return size + *p.from
	}
}

func (p *slicePath) getTo(size int) int {
	if p.to == nil || size == 0 || *p.to == 0 || *p.to > size {
		return size
	}
	if *p.to < 0 {
		if *p.to < -size {
			return 0
		}
		return size + *p.to

	}
	return *p.to

}

func (p *slicePath) getStep() int {
	if p.step == nil {
		return 1
	}
	return *p.step
}

func (p slicePath) Get(target interface{}) interface{} {
	if d, ok := target.([]interface{}); ok {
		dataLen := len(d)

		if dataLen == 0 || p.hasColon == false {
			return []interface{}{}
		}
		from := p.getFrom(dataLen)
		to := p.getTo(dataLen)
		step := p.getStep()
		if from >= to || to == 0 || step < 0 {
			return []interface{}{}
		}
		if step < 2 {
			return d[from:to]
		}

		result := make([]interface{}, 0)
		for i := from; i < to; i += step {
			result = append(result, d[i])
		}
		return result
	}
	return NonExist{}
}

type wildcardPath string

func (p wildcardPath) Get(target interface{}) interface{} {
	if p == "*" {
		switch result := target.(type) {
		case map[string]interface{}:
			newResult := make([]interface{}, 0, len(result))
			for _, v := range result {
				if v != nil {
					newResult = append(newResult, v)
				}
			}
			return newResult
		default:
			return target
		}
	}
	return nil
}

type recursivePath string

func (p recursivePath) Get(target interface{}) interface{} {
	result := make([]interface{}, 0)
	switch data := target.(type) {
	case map[string]interface{}:
		if item, exist := data[string(p)]; exist {
			result = append(result, item)
		}
		for _, v := range data {
			subResult := p.Get(v)
			switch subResultData := subResult.(type) {
			case []interface{}:
				result = append(result, subResultData...)
			case NonExist:
				continue
			default:
				result = append(result, subResultData)
			}
		}
	case []interface{}:
		for _, v := range data {
			subResult := p.Get(v)
			switch subResultData := subResult.(type) {
			case []interface{}:
				result = append(result, subResultData...)
			case NonExist:
				continue
			default:
				result = append(result, subResultData)
			}
		}
	}
	if len(result) == 0 {
		return NonExist{}
	}

	return result
}

func isSplitPath(path jsonPath) bool {
	switch path.(type) {
	case wildcardPath:
		return true
	case recursivePath:
		return true
	case slicePath:
		return true
	default:
		return false
	}
}