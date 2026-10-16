package condition

import "testing"

func TestOperatorNames(t *testing.T) {
	for _, op := range Operators() {
		name := op.String()
		if name == "invalid" || name == "" {
			t.Fatalf("operator %d has no name", op)
		}
		if got, ok := ParseOperator(name); !ok || got != op {
			t.Fatalf("round trip %s: got %v %v", name, got, ok)
		}
	}
	if _, ok := ParseOperator("xor"); ok {
		t.Fatal("unknown name accepted")
	}
	if Operator(0).String() != "invalid" || opEnd.String() != "invalid" {
		t.Fatal("out of range operator must print invalid")
	}
}

func TestLiteralAccessors(t *testing.T) {
	typed, err := userSchema().Check(Op(OpIn, Ref("city"), LitStrs("bj", "sh")))
	if err != nil {
		t.Fatal(err)
	}
	l := typed.Args[1].Lit
	if l.Kind() != KindStrings || l.Len() != 2 || l.Strings()[1] != "sh" {
		t.Fatalf("unexpected literal %+v", l)
	}
	// Reading a literal as the wrong kind is a backend bug and must not
	// silently yield a zero value.
	for name, read := range map[string]func(){
		"number":  func() { _ = l.Number() },
		"string":  func() { _ = l.String() },
		"bool":    func() { _ = l.Bool() },
		"numbers": func() { _ = l.Numbers() },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			read()
		})
	}
}

func TestLitConstructorsNormalise(t *testing.T) {
	type cents int32
	typed, err := userSchema().Check(Op(OpAnd,
		Op(OpGe, Ref("age"), LitNum(cents(18))),
		Op(OpIn, Ref("age"), LitNums[uint8](1, 2)),
	))
	if err != nil {
		t.Fatal(err)
	}
	if typed.Args[0].Args[1].Lit.Number() != 18 || typed.Args[1].Args[1].Lit.Numbers()[1] != 2 {
		t.Fatal("numeric literals must normalise to float64")
	}
}
