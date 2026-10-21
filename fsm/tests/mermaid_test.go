package fsm_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/k0923/go/fsm"
)

func TestMermaid(t *testing.T) {
	b := fsm.NewBuilder[orderState, struct{}]()
	b.State(pending, "待支付").State(paid, "已支付").
		State(cancelled, "已取消").State(orderState("archived"), "归档")
	b.On(pay).From(pending).To(paid).Description("金额一致").
		Guard(func(context.Context, struct{}, payment) (bool, error) { return true, nil }).
		Action(func(context.Context, struct{}, payment) error { return nil })
	b.On(fsm.NewEvent[orderState, struct{}]("cancel", "取消")).
		From(pending, paid).To(cancelled)
	b.On(fsm.NewEvent[orderState, struct{}]("check")).From(paid).To(paid)
	d := mustBuild(t, b)
	want := `flowchart LR
    s0["pending<br/>待支付"]
    s1["paid<br/>已支付"]
    s2["cancelled<br/>已取消"]
    s3["archived<br/>归档"]
    s0 -->|"pay<br/>支付<br/>金额一致<br/>#91;guard#44; action#93;"| s1
    s0 -->|"cancel<br/>取消"| s2
    s1 -->|"cancel<br/>取消"| s2
    s1 -->|"check"| s1
`
	want += "    classDef default fill:#eff6ff,stroke:#3b82f6,color:#1e293b,stroke-width:1.5px\n"
	want += "    linkStyle default stroke:#64748b,stroke-width:1.5px\n"
	if _, got, ok := strings.Cut(d.Mermaid(), "\n"); !ok || got != want {
		t.Fatalf("unexpected diagram:\n%s\nwant:\n%s", got, want)
	}
	if got := d.Mermaid(); got != d.Mermaid() {
		t.Fatalf("diagram output is not stable:\n%s", got)
	}
}

func TestMermaidEscapesUserText(t *testing.T) {
	b := fsm.NewBuilder[orderState, struct{}]()
	from := orderState("end\"]\nclick s0 \"javascript:alert(1)\"")
	b.State(from, `<script>alert("x")</script>`, "第二行\r\n第三行")
	b.On(fsm.NewEvent[orderState, struct{}]("a|b", `quote" & # <b>`)).
		From(from).To(paid).Description("x --> y\n说明")
	got := mustBuild(t, b).Mermaid(fsm.MermaidOptions[orderState]{LabelWidth: 120})
	for _, unsafe := range []string{`<script>`, `</script>`, `javascript:`, `a|b`, `quote"`, "x --> y", "\nclick", "<b>"} {
		if strings.Contains(got, unsafe) {
			t.Fatalf("unescaped text %q in diagram:\n%s", unsafe, got)
		}
	}
	for _, escaped := range []string{"#34;", "#124;", "#38;", "#35;", "#60;", "#62;", "第二行<br/>第三行", "说明"} {
		if !strings.Contains(got, escaped) {
			t.Fatalf("missing escaped text %q:\n%s", escaped, got)
		}
	}
	if strings.Count(got, "\n") != 7 {
		t.Fatalf("user text introduced diagram statements:\n%s", got)
	}
}

func TestMermaidOptions(t *testing.T) {
	b := fsm.NewBuilder[orderState, struct{}]()
	b.State(pending, "等待支付").State(paid, "完成支付")
	b.On(pay).From(pending).To(paid).Description("只有金额正确才可以完成支付")
	d := mustBuild(t, b)
	before := d.Mermaid()
	options := fsm.MermaidOptions[orderState]{
		Direction: fsm.TopToBottom, Layout: fsm.LayoutELK,
		NodeSpacing: 75, RankSpacing: 120, FontSize: 16,
		Compact:   true,
		NodeStyle: fsm.MermaidNodeStyle{Fill: "#fff"},
		StateStyles: map[orderState]fsm.MermaidNodeStyle{
			paid: {Fill: "#ecfdf5", Stroke: "#059669"},
		},
		EdgeColor: "#112233",
	}
	got := d.Mermaid(options)
	header, body, _ := strings.Cut(got, "\n")
	var config struct {
		Flowchart struct {
			DefaultRenderer string `json:"defaultRenderer"`
			NodeSpacing     int    `json:"nodeSpacing"`
			RankSpacing     int    `json:"rankSpacing"`
		} `json:"flowchart"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimPrefix(header, "%%{init: "), "}%%")), &config); err != nil {
		t.Fatal(err)
	}
	if config.Flowchart.DefaultRenderer != "elk" || config.Flowchart.NodeSpacing != 75 || config.Flowchart.RankSpacing != 120 {
		t.Fatalf("options not applied: %+v", config)
	}
	for _, text := range []string{
		"flowchart TB", `s0["等待支付"]`, `s0 -->|"支付"| s1`,
		"classDef default fill:#fff", "style s1 fill:#ecfdf5,stroke:#059669,color:#1e293b",
		"linkStyle default stroke:#112233",
	} {
		if !strings.Contains(body, text) {
			t.Fatalf("missing %q:\n%s", text, got)
		}
	}
	if strings.Contains(body, "pending") || strings.Contains(body, "只有金额") {
		t.Fatal("compact mode retained verbose labels")
	}
	if d.Mermaid() != before || d.Mermaid(options) != got {
		t.Fatal("export mutated the definition or output is unstable")
	}
}

func TestMermaidWrappingAndInvalidOptions(t *testing.T) {
	b := fsm.NewBuilder[orderState, struct{}]()
	b.On(pay).From(pending).To(paid).Description("一二三四五六七八")
	d := mustBuild(t, b)
	got := d.Mermaid(fsm.MermaidOptions[orderState]{
		LabelWidth: 8, Direction: "TB\nclick s0", Layout: "bogus",
		NodeSpacing: -1, RankSpacing: 9999,
		NodeStyle: fsm.MermaidNodeStyle{Fill: "#fff\nclick s0"},
		EdgeColor: "red;click s0",
	})
	for _, text := range []string{"一二三四<br/>五六七八", "flowchart LR", `"rankSpacing":500`, `"nodeSpacing":50`, `"defaultRenderer":"dagre-wrapper"`} {
		if !strings.Contains(got, text) {
			t.Fatalf("missing %q:\n%s", text, got)
		}
	}
	if strings.Contains(got, "click s0") || strings.Contains(got, "bogus") {
		t.Fatal("invalid configuration entered Mermaid syntax")
	}
}

func TestMermaidPresentationBoundaries(t *testing.T) {
	t.Run("last configuration replaces earlier options", func(t *testing.T) {
		b := fsm.NewBuilder[orderState, struct{}]()
		b.State(pending, "pending description")
		d := mustBuild(t, b)
		got := d.Mermaid(
			fsm.MermaidOptions[orderState]{Direction: fsm.TopToBottom, Compact: true, EdgeColor: "#fff"},
			fsm.MermaidOptions[orderState]{Direction: fsm.RightToLeft},
		)
		if !strings.Contains(got, "flowchart RL") || !strings.Contains(got, `s0["pending<br/>pending description"]`) || !strings.Contains(got, `"lineColor":"#64748b"`) {
			t.Fatalf("earlier options were merged into the last configuration:\n%s", got)
		}
		if strings.Contains(got, "linkStyle") {
			t.Fatalf("isolated states produced edge styles:\n%s", got)
		}
	})
	t.Run("hidden callbacks and compact fallback retain edges", func(t *testing.T) {
		event := fsm.NewEvent[orderState, struct{}]("finish")
		b := fsm.NewBuilder[orderState, struct{}]()
		b.On(event).From(pending, paid).To(paid).Description("detail").
			Guard(func(context.Context, struct{}, struct{}) (bool, error) { t.Fatal("export ran guard"); return true, nil }).
			Action(func(context.Context, struct{}, struct{}) error { t.Fatal("export ran action"); return nil })
		d := mustBuild(t, b)
		for _, options := range []fsm.MermaidOptions[orderState]{{HideCallbacks: true}, {Compact: true}} {
			got := d.Mermaid(options)
			if strings.Contains(got, "guard") || strings.Contains(got, "action") || strings.Count(got, " -->") != 2 || !strings.Contains(got, `s0["pending"]`) {
				t.Fatalf("labels or edges incorrect:\n%s", got)
			}
			if !options.Compact && !strings.Contains(got, "finish<br/>detail") {
				t.Fatalf("hiding callbacks removed transition description:\n%s", got)
			}
			if options.Compact && (!strings.Contains(got, `|"finish"|`) || strings.Contains(got, "detail")) {
				t.Fatalf("compact identifier fallback failed:\n%s", got)
			}
		}
	})
	t.Run("dimensions clamp and styles inherit per field", func(t *testing.T) {
		b := fsm.NewBuilder[orderState, struct{}]()
		b.State(pending, "abcdefghij")
		d := mustBuild(t, b)
		got := d.Mermaid(fsm.MermaidOptions[orderState]{
			Direction: fsm.BottomToTop, NodeSpacing: 1, RankSpacing: 1,
			FontSize: 100, LabelWidth: 1,
			NodeStyle: fsm.MermaidNodeStyle{Fill: "#ABC", Stroke: "#A1b2C3", Color: "#123"},
			StateStyles: map[orderState]fsm.MermaidNodeStyle{
				pending: {Fill: "#ggg", Stroke: "1234", Color: "#fff"},
				paid:    {Fill: "#000"},
			},
		})
		for _, text := range []string{
			"flowchart BT", `"nodeSpacing":10`, `"rankSpacing":10`, `"fontSize":"32px"`,
			"abcdefgh<br/>ij", "style s0 fill:#ABC,stroke:#A1b2C3,color:#fff",
		} {
			if !strings.Contains(got, text) {
				t.Fatalf("missing %q:\n%s", text, got)
			}
		}
		if strings.Contains(got, "style s1") || strings.Contains(got, "#ggg") {
			t.Fatalf("unknown state or invalid style exported:\n%s", got)
		}
	})
	t.Run("line endings and control characters", func(t *testing.T) {
		b := fsm.NewBuilder[orderState, struct{}]()
		b.State(pending, "a\rb\r\nc\t\x00d")
		got := mustBuild(t, b).Mermaid()
		if !strings.Contains(got, "a<br/>b<br/>c  d") || strings.ContainsAny(got, "\r\t\x00") {
			t.Fatalf("line endings or controls not normalized:\n%s", got)
		}
	})
}
