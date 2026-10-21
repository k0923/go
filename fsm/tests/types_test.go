package fsm_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These programs must fail compilation: runtime checks cannot substitute for
// the public API's promised state-domain and event-payload type isolation.
func TestCompileTimeTypeIsolation(t *testing.T) {
	const prelude = `package typecheck
import (
    "context"
    "github.com/k0923/go/fsm"
)
type OrderState string
func (OrderState) FSMState() {}
type RefundState string
func (RefundState) FSMState() {}
type Payment struct { Amount int }
var pay = fsm.NewEvent[OrderState, Payment]("pay")
var builder = fsm.NewBuilder[OrderState, struct{}]()
var _ = context.Background
`
	tests := []struct {
		name string
		code string
		want []string
	}{
		{
			"foreign source",
			`func invalid() { builder.On(pay).From(RefundState("pending")) }`,
			[]string{"RefundState", "OrderState"},
		},
		{
			"foreign initial state",
			`func invalid(d *fsm.Definition[OrderState, struct{}]) {
    d.NewMachine(RefundState("pending"), struct{}{})
}`,
			[]string{"RefundState", "OrderState"},
		},
		{
			"wrong payload",
			`func invalid(m *fsm.Machine[OrderState, struct{}]) {
    m.Fire(context.Background(), pay, "wrong")
}`,
			[]string{"Payment", "string"},
		},
		{
			"missing state marker",
			`type PlainState string
var invalid = fsm.NewBuilder[PlainState, struct{}]()`,
			[]string{"PlainState", "FSMState"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "invalid.go")
			if err := os.WriteFile(path, []byte(prelude+tt.code), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			output, err := exec.CommandContext(ctx, "go", "test", path).CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("compiler timed out: %v", ctx.Err())
			}
			if err == nil {
				t.Fatalf("invalid program compiled:\n%s", tt.code)
			}
			for _, text := range tt.want {
				if !strings.Contains(string(output), text) {
					t.Fatalf("missing type diagnostic %q:\n%s", text, output)
				}
			}
		})
	}
}
