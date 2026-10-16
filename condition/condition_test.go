package condition

import (
	"errors"
	"math"
	"testing"
)

type item struct {
	Price float64
	Tags  []string
}

type user struct {
	Age    int
	Name   string
	City   string
	VIP    bool
	Scores []float64
	Tags   []string
	Items  []item
}

var (
	age    = Field(func(u user) int { return u.Age })
	name   = Field(func(u user) string { return u.Name })
	city   = Field(func(u user) string { return u.City })
	scores = Field(func(u user) []float64 { return u.Scores })
	items  = Field(func(u user) []item { return u.Items })
	price  = Field(func(i item) float64 { return i.Price })
)

var errBoom = errors.New("boom")

func failing[T, V any]() Expr[T, V] {
	return ExprFunc[T, V](func(T) (V, error) { var z V; return z, errBoom })
}

// counter records how often it is evaluated, to prove short-circuiting.
type counter struct {
	n      int
	result bool
}

func (c *counter) Match(user) (bool, error) { c.n++; return c.result, nil }

func mustMatch(t *testing.T, c Condition[user], u user, want bool) {
	t.Helper()
	got, err := c.Match(u)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestLogic(t *testing.T) {
	u := user{}
	tr, fa := True[user](), False[user]()
	cases := []struct {
		name string
		c    Condition[user]
		want bool
	}{
		{"empty and", And[user](), true},
		{"empty or", Or[user](), false},
		{"or all false", Or(fa, fa), false}, // old GroupCondition returned true
		{"or one true", Or(fa, tr), true},
		{"and one false", And(tr, fa), false},
		{"not true", Not(tr), false},
		{"double not", Not(Not(fa)), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { mustMatch(t, tc.c, u, tc.want) })
	}
}

func TestShortCircuit(t *testing.T) {
	a, b := &counter{result: false}, &counter{result: true}
	mustMatch(t, And(a, b), user{}, false)
	if a.n != 1 || b.n != 0 {
		t.Fatalf("and evaluated a=%d b=%d", a.n, b.n)
	}
	a, b = &counter{result: true}, &counter{result: false}
	mustMatch(t, Or(a, b), user{}, true)
	if a.n != 1 || b.n != 0 {
		t.Fatalf("or evaluated a=%d b=%d", a.n, b.n)
	}
}

func TestFolding(t *testing.T) {
	c := &counter{}
	if _, ok := And(c, False[user]()).(constCond[user]); !ok {
		t.Fatal("And with False should fold to a constant")
	}
	if got := And(True[user](), c); got != Condition[user](c) {
		t.Fatal("And(True, c) should reduce to c")
	}
	if g, ok := And(And(c, c), c).(andCond[user]); !ok || len(g) != 3 {
		t.Fatalf("nested And should flatten, got %#v", g)
	}
	if g, ok := Or(And(c, c), c).(orCond[user]); !ok || len(g) != 2 {
		t.Fatalf("And inside Or must not be flattened, got %#v", g)
	}
	if _, ok := Gt(Const[user](2), Const[user](1)).(constCond[user]); !ok {
		t.Fatal("constant comparison should fold")
	}
	if _, ok := Add(Const[user](1), Const[user](2)).(constExpr[user, int]); !ok {
		t.Fatal("constant arithmetic should fold")
	}
	// A constant division by zero must not fold away its error.
	if _, err := Div(Const[user](1), Const[user](0)).Eval(user{}); !errors.Is(err, ErrDivideByZero) {
		t.Fatalf("want ErrDivideByZero, got %v", err)
	}
}

func TestErrorsPropagate(t *testing.T) {
	bad := Eq(failing[user, int](), Const[user](1))
	conds := map[string]Condition[user]{
		"cmp":    bad,
		"not":    Not(bad),
		"and":    And(True[user](), bad, True[user]()),
		"or":     Or(bad, Condition[user](&counter{result: true})),
		"in":     In(failing[user, string](), "a"),
		"inlist": InList(Const[user]("a"), failing[user, []string]()),
		"any":    Any(failing[user, []item](), True[item]()),
		"pred":   Any(items, Gt(failing[item, float64](), Const[item](0.0))),
		"str":    Contains(Const[user]("a"), failing[user, string]()),
	}
	u := user{Items: []item{{}}}
	for name, c := range conds {
		t.Run(name, func(t *testing.T) {
			ok, err := c.Match(u)
			if !errors.Is(err, errBoom) || ok {
				t.Fatalf("got (%v, %v), want (false, boom)", ok, err)
			}
		})
	}
}

func TestComparisons(t *testing.T) {
	u := user{Age: 30, Name: "alice", City: "bj"}
	cases := []struct {
		name string
		c    Condition[user]
		want bool
	}{
		{"eq", Eq(age, Const[user](30)), true},
		{"ne", Ne(name, Const[user]("bob")), true},
		{"lt", Lt(age, Const[user](30)), false},
		{"le", Le(age, Const[user](30)), true},
		{"gt", Gt(age, Const[user](18)), true},
		{"ge", Ge(age, Const[user](31)), false},
		{"string order", Lt(name, Const[user]("bob")), true},
		{"contains", Contains(name, Const[user]("lic")), true},
		{"contains empty", Contains(name, Const[user]("")), true},
		{"prefix", HasPrefix(name, Const[user]("al")), true},
		{"suffix", HasSuffix(name, Const[user]("x")), false},
		{"field vs field", Eq(name, city), false},
		{"nan never ordered", Lt(Const[user](math.NaN()), Const[user](1.0)), false},
		{"nan never equal", Eq(Field(func(user) float64 { return math.NaN() }), Field(func(user) float64 { return math.NaN() })), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { mustMatch(t, tc.c, u, tc.want) })
	}
}

func TestIn(t *testing.T) {
	many := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "bj"} // > linearSetMax: map path
	cases := []struct {
		name string
		c    Condition[user]
		want bool
	}{
		{"small hit", In(city, "sh", "bj"), true},
		{"small miss", In(city, "sh"), false},
		{"large hit", In(city, many...), true},
		{"large miss", NotIn(city, many...), false},
		{"empty in", In(city), false},
		{"empty not in", NotIn(city), true},
		{"dynamic list", InList(city, Const[user]([]string{"bj"})), true},
		{"dynamic nil list", NotInList(city, Const[user]([]string(nil))), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { mustMatch(t, tc.c, user{City: "bj"}, tc.want) })
	}

	// In must copy its input so later mutation cannot change the rule.
	vals := []string{"bj"}
	c := In(city, vals...)
	vals[0] = "sh"
	mustMatch(t, c, user{City: "bj"}, true)
}

func TestQuantifiers(t *testing.T) {
	cheap := Lt(price, Const[item](10.0))
	full := user{Items: []item{{Price: 5}, {Price: 20}}}
	empty := user{}
	cases := []struct {
		name string
		c    Condition[user]
		u    user
		want bool
	}{
		{"any hit", Any(items, cheap), full, true},
		{"all miss", All(items, cheap), full, false},
		{"none miss", None(items, cheap), full, false},
		{"any empty", Any(items, cheap), empty, false},
		{"all empty", All(items, cheap), empty, true}, // vacuous truth, documented
		{"none empty", None(items, cheap), empty, true},
		{"scalar list", All(scores, Ge(Field(func(f float64) float64 { return f }), Const[float64](60.0))), user{Scores: []float64{60, 99}}, true},
		{"len", Eq(Len(items), Const[user](2)), full, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { mustMatch(t, tc.c, tc.u, tc.want) })
	}
}

func TestArithmetic(t *testing.T) {
	half := Div(age, Const[user](2))
	if v, _ := half.Eval(user{Age: 7}); v != 3 {
		t.Fatalf("integer division: got %v", v)
	}
	if _, err := Div(Const[user](1.0), Field(func(user) float64 { return 0 })).Eval(user{}); !errors.Is(err, ErrDivideByZero) {
		t.Fatalf("float division by zero: %v", err)
	}
	total := Sub(Mul(age, Const[user](3)), Add(Const[user](1), Const[user](2)))
	if v, _ := total.Eval(user{Age: 10}); v != 27 {
		t.Fatalf("got %v", v)
	}
	if _, err := Add(failing[user, int](), Const[user](1)).Eval(user{}); !errors.Is(err, errBoom) {
		t.Fatalf("got %v", err)
	}
}

func TestNilOperandsPanic(t *testing.T) {
	cases := map[string]func(){
		"and":   func() { And[user](nil) },
		"not":   func() { Not[user](nil) },
		"eq":    func() { Eq[user](nil, Const[user](1)) },
		"in":    func() { In[user, string](nil) },
		"field": func() { Field[user, int](nil) },
		"any":   func() { Any(items, nil) },
		"add":   func() { Add(age, nil) },
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected panic")
				}
			}()
			f()
		})
	}
}

// Named string types work with the string operators.
func TestNamedString(t *testing.T) {
	type Code string
	c := HasPrefix(Field(func(c Code) Code { return c }), Const[Code](Code("E")))
	if ok, _ := c.Match("E42"); !ok {
		t.Fatal("named string prefix")
	}
}
