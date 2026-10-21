package fsm

import (
	"fmt"
	"strings"
)

// MermaidDirection controls the overall direction of the graph.
type MermaidDirection string

const (
	LeftToRight MermaidDirection = "LR"
	TopToBottom MermaidDirection = "TB"
	RightToLeft MermaidDirection = "RL"
	BottomToTop MermaidDirection = "BT"
)

// MermaidLayout selects a renderer supported by the Mermaid host.
type MermaidLayout string

const (
	LayoutDagre MermaidLayout = "dagre-wrapper"
	// LayoutELK requires a Mermaid host with ELK support.
	LayoutELK MermaidLayout = "elk"
)

// MermaidNodeStyle sets node colors using #RGB or #RRGGBB hex values.
// Empty or invalid fields inherit the default node style.
type MermaidNodeStyle struct {
	Fill   string
	Stroke string
	Color  string
}

// MermaidOptions configures presentation without changing execution rules.
// Zero values use defaults. Positive dimensions are clamped to the documented
// range; invalid enum values and colors fall back to defaults.
// Do not mutate StateStyles concurrently with Mermaid.
type MermaidOptions[S State] struct {
	Direction   MermaidDirection // Default LR.
	Layout      MermaidLayout    // Default Dagre.
	NodeSpacing int              // Default 50, range 10..500 pixels.
	RankSpacing int              // Default 90, range 10..500 pixels.
	LabelWidth  int              // Default 28, range 8..120 display units; CJK counts as two.
	FontSize    int              // Default 14, range 10..32 pixels.
	NodeStyle   MermaidNodeStyle
	StateStyles map[S]MermaidNodeStyle
	EdgeColor   string // Default #64748b; accepts #RGB or #RRGGBB.
	// Compact shows descriptions (falling back to identifiers) and omits
	// transition descriptions and callback markers. All edges are retained.
	Compact bool
	// HideCallbacks omits guard/action markers in the full view.
	HideCallbacks bool
}

func defaultMermaidOptions[S State]() MermaidOptions[S] {
	return MermaidOptions[S]{
		Direction: LeftToRight, Layout: LayoutDagre,
		NodeSpacing: 50, RankSpacing: 90, LabelWidth: 28, FontSize: 14,
		NodeStyle: MermaidNodeStyle{Fill: "#eff6ff", Stroke: "#3b82f6", Color: "#1e293b"},
		EdgeColor: "#64748b",
	}
}

func normalizeMermaidOptions[S State](options []MermaidOptions[S]) MermaidOptions[S] {
	o := defaultMermaidOptions[S]()
	// Each supplied struct is a complete configuration; the last one wins.
	if len(options) == 0 {
		return o
	}
	v := options[len(options)-1]
	switch v.Direction {
	case LeftToRight, TopToBottom, RightToLeft, BottomToTop:
		o.Direction = v.Direction
	}
	switch v.Layout {
	case LayoutDagre, LayoutELK:
		o.Layout = v.Layout
	}
	o.NodeSpacing = diagramDimension(v.NodeSpacing, o.NodeSpacing, 10, 500)
	o.RankSpacing = diagramDimension(v.RankSpacing, o.RankSpacing, 10, 500)
	o.LabelWidth = diagramDimension(v.LabelWidth, o.LabelWidth, 8, 120)
	o.FontSize = diagramDimension(v.FontSize, o.FontSize, 10, 32)
	o.NodeStyle = mergeNodeStyle(o.NodeStyle, v.NodeStyle)
	o.EdgeColor = diagramColor(v.EdgeColor, o.EdgeColor)
	o.StateStyles, o.Compact, o.HideCallbacks = v.StateStyles, v.Compact, v.HideCallbacks
	return o
}

func diagramDimension(value, fallback, minimum, maximum int) int {
	if value <= 0 {
		return fallback
	}
	return min(max(value, minimum), maximum)
}

func diagramColor(value, fallback string) string {
	if (len(value) != 4 && len(value) != 7) || value[0] != '#' {
		return fallback
	}
	for _, c := range value[1:] {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return fallback
		}
	}
	return value
}

func mergeNodeStyle(base, override MermaidNodeStyle) MermaidNodeStyle {
	return MermaidNodeStyle{
		Fill:   diagramColor(override.Fill, base.Fill),
		Stroke: diagramColor(override.Stroke, base.Stroke),
		Color:  diagramColor(override.Color, base.Color),
	}
}

func (s MermaidNodeStyle) mermaid() string {
	return fmt.Sprintf("fill:%s,stroke:%s,color:%s,stroke-width:1.5px", s.Fill, s.Stroke, s.Color)
}
