package fluent

import (
	"fmt"
	"testing"
	"time"
)

func init() {
	fmt.Println("start")
}

func TestGet(t *testing.T) {
	var result interface{}
	var s2 RetryStrategy = UseStrategy(RetryByDuration(time.Second*5, time.Second*1), ValidCode(200))
	err := Get("http://localhost:8080/ping").Retry(s2).JsonResult(&result).Send()
	fmt.Println(result)
	fmt.Println(err)

}

// func TestClient(t *testing.T) {
// 	var result interface{}
// 	client := NewClient(&RequestOptions{
// 		RetryStrategy: UseStrategy(RetryByDuration(time.Second*2, time.Second*1), ValidCode(200)),
// 	})
// 	err := client.Get("http://localhost:8080").JsonResult(&result).Send()
// 	fmt.Println(result)
// 	fmt.Println(err)

// }

func TestOverrideClient(t *testing.T) {
	var result interface{}
	client := NewClient(&RequestOptions{
		RetryStrategy: UseStrategy(RetryByCount(2), ValidCode(200)),
	})
	err := client.Get("http://localhost:8080").JsonResult(&result).Send()
	fmt.Println(result)
	fmt.Println(err)
}
