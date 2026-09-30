package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
)

type result struct {
	Operation string `json:"operation"`
	Left      int64  `json:"left"`
	Right     int64  `json:"right"`
	Value     int64  `json:"value"`
}

func main() {
	if len(os.Args) != 4 {
		exitf("usage: guest.wasm <add|multiply> <left> <right>")
	}

	left, err := strconv.ParseInt(os.Args[2], 10, 64)
	if err != nil {
		exitf("invalid left operand: %v", err)
	}

	right, err := strconv.ParseInt(os.Args[3], 10, 64)
	if err != nil {
		exitf("invalid right operand: %v", err)
	}

	var value int64
	switch os.Args[1] {
	case "add":
		value = left + right
	case "multiply":
		value = left * right
	default:
		exitf("unsupported operation %q", os.Args[1])
	}

	if err := json.NewEncoder(os.Stdout).Encode(result{
		Operation: os.Args[1],
		Left:      left,
		Right:     right,
		Value:     value,
	}); err != nil {
		exitf("encode result: %v", err)
	}
}

func exitf(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
