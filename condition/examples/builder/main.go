// Run with: go run ./condition/examples/builder
// 运行方式：go run ./condition/examples/builder
//
// Builds conditions directly in Go. Operand types are checked by the compiler:
// Gt on a bool or HasPrefix on an int does not type-check.
// 直接在 Go 代码中构建条件。操作数类型由编译器检查：
// 对 bool 使用 Gt、对 int 使用 HasPrefix 都无法通过编译。
package main

import (
	"fmt"
	"strings"

	c "github.com/k0923/go/condition"
)

type Level string

type User struct {
	ID      int64
	Name    string
	Email   string
	Age     int
	Level   Level
	Balance float64
	Banned  bool
	Roles   []string
}

// Accessors are declared once and reused; Field wraps an infallible getter.
// 访问器只声明一次并复用；Field 包装一个不会失败的取值函数。
var (
	name    = c.Field(func(u User) string { return u.Name })
	email   = c.Field(func(u User) string { return u.Email })
	age     = c.Field(func(u User) int { return u.Age })
	level   = c.Field(func(u User) Level { return u.Level })
	balance = c.Field(func(u User) float64 { return u.Balance })
	banned  = c.Field(func(u User) bool { return u.Banned })
	roles   = c.Field(func(u User) []string { return u.Roles })
)

func main() {
	alice := User{ID: 1, Name: "alice", Email: "alice@corp.com", Age: 30, Level: "gold", Balance: 88.5, Roles: []string{"admin"}}
	bob := User{ID: 2, Name: "bob", Email: "bob@gmail.com", Age: 16, Level: "silver", Banned: true}

	// 1. Comparison: Eq/Ne need comparable values, Lt/Le/Gt/Ge need ordered ones.
	// 1. 比较：Eq/Ne 要求可比较类型，Lt/Le/Gt/Ge 要求有序类型。
	adult := c.Ge(age, c.Const[User](18))
	rich := c.Gt(balance, c.Const[User](50.0))
	notBanned := c.Eq(banned, c.Const[User](false))
	show("adult", adult, alice, bob)
	show("rich", rich, alice, bob)
	show("not banned", notBanned, alice, bob)

	// Field-to-field comparison works the same as field-to-constant.
	// 字段之间比较和字段与常量比较写法一致。
	show("name < email", c.Lt(name, email), alice, bob)

	// 2. Strings: Contains / HasPrefix / HasSuffix, also on named string types.
	// 2. 字符串：Contains / HasPrefix / HasSuffix，同样适用于命名字符串类型。
	corpMail := c.HasSuffix(email, c.Const[User]("@corp.com"))
	goldish := c.HasPrefix(level, c.Const[User](Level("go")))
	show("corp mail", corpMail, alice, bob)
	show("level starts with go", goldish, alice, bob)

	// 3. Membership: In/NotIn take literal values (hashed when more than 8),
	//    InList checks against a list computed from the input.
	// 3. 集合判断：In/NotIn 接收字面量（超过 8 个时使用哈希），
	//    InList 则判断是否在从输入计算出的列表中。
	vipLevel := c.In(level, "gold", "platinum")
	isAdmin := c.InList(c.Const[User]("admin"), roles)
	show("vip level", vipLevel, alice, bob)
	show("is admin", isAdmin, alice, bob)
	show("not in empty set", c.NotIn(level), alice, bob) // NotIn() is always true

	// 4. Logic: And/Or short-circuit, Not negates. And() is true, Or() is false.
	// 4. 逻辑：And/Or 短路求值，Not 取反。And() 恒真，Or() 恒假。
	canBuy := c.And(adult, notBanned, c.Or(vipLevel, rich))
	show("can buy", canBuy, alice, bob)
	show("not(can buy)", c.Not(canBuy), alice, bob)

	// 5. Custom logic: CondFunc and ExprFunc plug any Go code into the tree.
	// 5. 自定义逻辑：CondFunc 和 ExprFunc 可以把任意 Go 代码接入条件树。
	evenID := c.CondFunc[User](func(u User) (bool, error) { return u.ID%2 == 0, nil })
	domain := c.ExprFunc[User, string](func(u User) (string, error) {
		_, d, ok := strings.Cut(u.Email, "@")
		if !ok {
			return "", fmt.Errorf("invalid email %q", u.Email)
		}
		return d, nil
	})
	show("even id", evenID, alice, bob)
	show("gmail user", c.Eq(domain, c.Const[User]("gmail.com")), alice, bob)

	// 6. Constant folding: statically known sub-trees are simplified at build
	//    time, so a disabled feature flag never evaluates the rest of the rule.
	// 6. 常量折叠：结果可静态确定的子树在构建期即被化简，
	//    因此关闭的功能开关不会触发规则其余部分的求值。
	calls := 0
	tracked := c.CondFunc[User](func(User) (bool, error) { calls++; return true, nil })
	featureOn := false
	rule := c.And(flag(featureOn), tracked)
	ok, _ := rule.Match(alice)
	fmt.Printf("%-22s result=%v tracked calls=%d\n", "flag off:", ok, calls)
}

// flag turns a configuration switch into a constant condition.
// flag 把配置开关转换为常量条件。
func flag(on bool) c.Condition[User] {
	if on {
		return c.True[User]()
	}
	return c.False[User]()
}

func show(label string, cond c.Condition[User], users ...User) {
	fmt.Printf("%-22s", label+":")
	for _, u := range users {
		ok, err := cond.Match(u)
		if err != nil {
			fmt.Printf(" %s=error(%v)", u.Name, err)
			continue
		}
		fmt.Printf(" %s=%v", u.Name, ok)
	}
	fmt.Println()
}
