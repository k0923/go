package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"github.com/k0923/go/orderedmap"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	var prices orderedmap.Map[int]
	prices.Set("banana", 20)
	prices.Set("apple", 10)
	prices.Set("banana", 25)
	for key, value := range prices.All() {
		fmt.Println(key, value)
	}

	data, err := json.Marshal(prices)
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	if err := json.Unmarshal([]byte(`{"orange":30,"apple":15}`), &prices); err != nil {
		return err
	}
	fmt.Println("replaced:", prices.Keys())

	var groups orderedmap.Map[*orderedmap.Map[int]]
	if err := json.Unmarshal([]byte(`{"fruit":{"z":1,"a":2},"other":{"y":3,"b":4}}`), &groups); err != nil {
		return err
	}
	fruit, _ := groups.Get("fruit")
	fruit.Set("last", 5)
	data, err = json.Marshal(groups)
	if err != nil {
		return err
	}
	fmt.Println("nested:", string(data))

	err = json.Unmarshal([]byte(`{"apple":1,"apple":2}`), &prices)
	if !errors.Is(err, orderedmap.ErrDuplicateKey) {
		return fmt.Errorf("expected duplicate key error, got %v", err)
	}
	var duplicate *orderedmap.DuplicateKeyError
	if errors.As(err, &duplicate) {
		fmt.Println("duplicate:", duplicate.Key)
	}
	fmt.Println("unchanged:", prices.Keys())
	return nil
}
