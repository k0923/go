package xjson

import (
	"bytes"
	"encoding/json"
)

// optData 是内部承载数据的结构体，不对外暴露细节
type optData[T any] struct {
	Value  T
	IsNull bool
}

// O 定义为结构体切片
// 1. Undefined: nil (len==0) -> omitempty 生效
// 2. Null:      [{IsNull: true}]
// 3. Value:     [{Value: v, IsNull: false}]
type O[T any] []optData[T]

// -----------------------------------------------------------------------------
// 构造函数
// -----------------------------------------------------------------------------

// NO 创建一个有值的 O
// 优化：相比 []*T，这里少了一次 T 的堆分配，直接存入 slice 底层数组
func NO[T any](v T) O[T] {
	return []optData[T]{
		{Value: v, IsNull: false},
	}
}

// Null 创建一个显式 null 的 O
func Null[T any]() O[T] {
	return []optData[T]{
		{IsNull: true},
	}
}

// Undefined 创建一个未定义的 O (nil)
func Undefined[T any]() O[T] {
	return nil
}

// -----------------------------------------------------------------------------
// 方法定义 (现在接收器是合法的 Slice 类型)
// -----------------------------------------------------------------------------

func (o O[T]) IsUndefined() bool {
	return len(o) == 0
}

func (o O[T]) IsNull() bool {
	return len(o) > 0 && o[0].IsNull
}

func (o O[T]) HasValue() bool {
	return len(o) > 0 && !o[0].IsNull
}

func (o O[T]) Value() T {
	if o.HasValue() {
		return o[0].Value
	}
	var zero T
	return zero
}

// -----------------------------------------------------------------------------
// JSON 接口实现
// -----------------------------------------------------------------------------

func (o O[T]) MarshalJSON() ([]byte, error) {
	// 1. Undefined (nil slice)
	// 如果字段配置了 omitempty，json 库不会调用此方法。
	// 如果没配置 omitempty，通常输出 null。
	if len(o) == 0 {
		return []byte("null"), nil
	}

	// 2. Explicit Null
	if o[0].IsNull {
		return []byte("null"), nil
	}

	// 3. Value
	return json.Marshal(o[0].Value)
}

func (o *O[T]) UnmarshalJSON(data []byte) error {
	// 1. 处理 JSON null
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		*o = []optData[T]{{IsNull: true}}
		return nil
	}

	// 2. 解析值
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}

	// 3. 构造切片
	*o = []optData[T]{{Value: v, IsNull: false}}
	return nil
}
