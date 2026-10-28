package main

import (
	"path/filepath"
	"testing"

	"github.com/k0923/go/tflow"
)

func TestReferenceExampleCanReopenCompletedDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reference.db")
	if err := run(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	if err := run(t.Context(), path); err != nil {
		t.Fatal(err)
	}
}

func TestBusinessCodecRoundTrip(t *testing.T) {
	c := BusinessCodec{}
	for _, value := range []any{&Batch{Next: 2, Total: 5}, Unit{Row: 4}, tflow.FillPolicy{}, tflow.WavePolicy{Size: 2}, tflow.BufferPolicy{TargetPending: 3}, tflow.DefaultMainDecider{}} {
		data, err := c.Encode(value)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := c.Decode(data)
		if err != nil {
			t.Fatal(err)
		}
		again, err := c.Encode(decoded)
		if err != nil || string(data) != string(again) {
			t.Fatalf("%T roundtrip %s %s %v", value, data, again, err)
		}
	}
	for _, data := range [][]byte{[]byte("{"), []byte(`{"Type":"batch","Version":9}`), []byte(`{"Type":"unknown","Version":1}`)} {
		if _, err := c.Decode(data); err == nil {
			t.Fatal("invalid envelope accepted")
		}
	}
	if _, err := c.Encode(func() {}); err == nil {
		t.Fatal("closure serialized")
	}
}
