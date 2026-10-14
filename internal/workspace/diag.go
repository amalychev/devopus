package workspace

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type Severity int

const (
	Warning Severity = iota
	Error
)

func (s Severity) String() string {
	if s == Error {
		return "error"
	}
	return "warning"
}

func (s Severity) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// Diagnostic is one finding of validation, anchored to a field of the file.
type Diagnostic struct {
	Severity Severity `json:"severity"`
	Path     string   `json:"path"` // dotted field path: modules.api.routes[0]
	Line     int      `json:"line,omitempty"`
	Message  string   `json:"message"`
}

func (d Diagnostic) String() string {
	loc := d.Path
	if d.Line > 0 {
		loc = fmt.Sprintf("line %d: %s", d.Line, d.Path)
	}
	if loc == "" {
		return d.Severity.String() + ": " + d.Message
	}
	return fmt.Sprintf("%s: %s: %s", d.Severity, loc, d.Message)
}

type Diagnostics []Diagnostic

func (ds Diagnostics) HasErrors() bool {
	for _, d := range ds {
		if d.Severity == Error {
			return true
		}
	}
	return false
}

func (ds Diagnostics) Errors() Diagnostics   { return ds.filter(Error) }
func (ds Diagnostics) Warnings() Diagnostics { return ds.filter(Warning) }

func (ds Diagnostics) filter(s Severity) Diagnostics {
	var out Diagnostics
	for _, d := range ds {
		if d.Severity == s {
			out = append(out, d)
		}
	}
	return out
}

// Sort orders by line, then path, errors first; duplicates are dropped.
func (ds Diagnostics) Sort() Diagnostics {
	sort.SliceStable(ds, func(i, j int) bool {
		a, b := ds[i], ds[j]
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Severity > b.Severity
	})
	out := ds[:0]
	for i, d := range ds {
		if i > 0 && d == ds[i-1] {
			continue
		}
		out = append(out, d)
	}
	return out
}

// Error makes a set with errors usable as an error value.
func (ds Diagnostics) Error() string {
	var b strings.Builder
	for i, d := range ds {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(d.String())
	}
	return b.String()
}

// collector accumulates diagnostics and resolves their lines in the source document.
type collector struct {
	doc   *yaml.Node
	diags Diagnostics
}

func (c *collector) add(sev Severity, path []string, format string, a ...any) {
	c.diags = append(c.diags, Diagnostic{
		Severity: sev,
		Path:     joinPath(path),
		Line:     lineOf(c.doc, path),
		Message:  fmt.Sprintf(format, a...),
	})
}

func (c *collector) errorf(path []string, format string, a ...any) {
	c.add(Error, path, format, a...)
}

func (c *collector) warnf(path []string, format string, a ...any) {
	c.add(Warning, path, format, a...)
}

// joinPath renders ["modules", "api", "routes", "0"] as modules.api.routes[0].
func joinPath(path []string) string {
	var b strings.Builder
	for _, p := range path {
		if _, err := strconv.Atoi(p); err == nil {
			b.WriteString("[" + p + "]")
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(p)
	}
	return b.String()
}

// p builds a path from parts; numbers become list indexes.
func p(parts ...any) []string {
	out := make([]string, len(parts))
	for i, x := range parts {
		out[i] = fmt.Sprint(x)
	}
	return out
}

// lineOf finds the line of the deepest node of path that exists in the document.
func lineOf(doc *yaml.Node, path []string) int {
	if doc == nil {
		return 0
	}
	n := doc
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		n = n.Content[0]
	}
	line := 0
	for _, key := range path {
		switch n.Kind {
		case yaml.MappingNode:
			var next *yaml.Node
			for i := 0; i+1 < len(n.Content); i += 2 {
				if n.Content[i].Value == key {
					line = n.Content[i].Line
					next = n.Content[i+1]
					break
				}
			}
			if next == nil {
				return line
			}
			n = next
		case yaml.SequenceNode:
			i, err := strconv.Atoi(key)
			if err != nil || i < 0 || i >= len(n.Content) {
				return line
			}
			n = n.Content[i]
			line = n.Line
		default:
			return line
		}
	}
	return line
}
