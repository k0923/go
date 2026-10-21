package fsm

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// Mermaid exports the complete definition as a Mermaid flowchart.
// Nodes show state values and descriptions. Edges show event names, event and
// transition descriptions, and the presence of guard/action callbacks.
// Isolated states, multi-source transitions and self-transitions are included.
//
// Optional presentation settings control styles, layout and label density.
// With no options, a light blue style and left-to-right Dagre layout are used.
// If multiple configurations are supplied, the last one wins.
// Output follows registration order and uses generated node IDs and escaped
// labels. It contains neither Markdown fences nor a Mermaid runtime dependency.
// Initial and terminal markers are not inferred: initial state is per machine,
// and having no outgoing transition does not declare a state terminal.
func (d *Definition[S, D]) Mermaid(options ...MermaidOptions[S]) string {
	o := normalizeMermaidOptions(options)
	var out strings.Builder
	config := map[string]any{
		"theme": "base",
		"themeVariables": map[string]any{
			"fontFamily": "sans-serif", "fontSize": fmt.Sprintf("%dpx", o.FontSize),
			"primaryColor": o.NodeStyle.Fill, "primaryTextColor": o.NodeStyle.Color,
			"primaryBorderColor": o.NodeStyle.Stroke, "lineColor": o.EdgeColor,
			"edgeLabelBackground": "#ffffff",
		},
		"flowchart": map[string]any{
			"defaultRenderer": o.Layout, "nodeSpacing": o.NodeSpacing,
			"rankSpacing": o.RankSpacing, "curve": "linear", "htmlLabels": true,
		},
	}
	// Modern Mermaid selects external layout loaders via the top-level key.
	// Retain defaultRenderer above for older hosts.
	if o.Layout == LayoutELK {
		config["layout"] = "elk"
	}
	encoded, _ := json.Marshal(config) // Only primitive, finite values above.
	fmt.Fprintf(&out, "%%%%{init: %s}%%%%\nflowchart %s\n", encoded, o.Direction)
	for i, state := range d.states {
		parts := []string{fmt.Sprint(state.value), state.description}
		if o.Compact && state.description != "" {
			parts = []string{state.description}
		}
		fmt.Fprintf(&out, "    s%d[\"%s\"]\n", i, diagramLabel(o.LabelWidth, parts...))
	}
	for _, r := range d.rules {
		var callbacks string
		if r.guard != nil && !o.HideCallbacks {
			callbacks = "guard"
		}
		if r.action != nil && !o.HideCallbacks {
			if callbacks != "" {
				callbacks += ", "
			}
			callbacks += "action"
		}
		if callbacks != "" {
			callbacks = "[" + callbacks + "]"
		}
		parts := []string{r.event.name, r.event.description, r.description, callbacks}
		if o.Compact {
			label := r.event.description
			if label == "" {
				label = r.event.name
			}
			parts = []string{label}
		}
		label := diagramLabel(o.LabelWidth, parts...)
		for _, from := range r.from {
			fmt.Fprintf(&out, "    s%d -->|\"%s\"| s%d\n", d.stateIndex[from], label, d.stateIndex[r.to])
		}
	}
	fmt.Fprintf(&out, "    classDef default %s\n", o.NodeStyle.mermaid())
	for i, state := range d.states {
		if style, exists := o.StateStyles[state.value]; exists {
			fmt.Fprintf(&out, "    style s%d %s\n", i, mergeNodeStyle(o.NodeStyle, style).mermaid())
		}
	}
	if len(d.rules) > 0 {
		fmt.Fprintf(&out, "    linkStyle default stroke:%s,stroke-width:1.5px\n", o.EdgeColor)
	}
	return out.String()
}

func diagramLabel(width int, parts ...string) string {
	var lines []string
	for _, part := range parts {
		if part == "" {
			continue
		}
		part = strings.ReplaceAll(part, "\r\n", "\n")
		part = strings.ReplaceAll(part, "\r", "\n")
		for line := range strings.SplitSeq(part, "\n") {
			var escaped strings.Builder
			columns := 0
			for _, char := range line {
				size := 1
				if unicode.In(char, unicode.Han, unicode.Hangul, unicode.Hiragana, unicode.Katakana) || char >= 0x1F300 {
					size = 2
				}
				if unicode.IsMark(char) {
					size = 0
				}
				if columns+size > width && columns > 0 {
					escaped.WriteString("<br/>")
					columns = 0
				}
				columns += size
				switch {
				case unicode.IsLetter(char), unicode.IsNumber(char), char == ' ':
					escaped.WriteRune(char)
				case unicode.IsControl(char):
					escaped.WriteByte(' ')
				default:
					// Mermaid entity notation keeps user text out of its syntax.
					escaped.WriteByte('#')
					escaped.WriteString(strconv.Itoa(int(char)))
					escaped.WriteByte(';')
				}
			}
			lines = append(lines, escaped.String())
		}
	}
	return strings.Join(lines, "<br/>")
}
