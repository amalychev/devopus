package workspace

import (
	"bytes"
	"regexp"

	"gopkg.in/yaml.v3"
)

// Marshal renders the workspace the way people write it: short lists on one line
// (routes: [/api]) and a blank line between sections and between entries.
func Marshal(w *Workspace, header string) ([]byte, error) {
	var n yaml.Node
	if err := n.Encode(w); err != nil {
		return nil, err
	}
	flowShortLists(&n)
	var buf bytes.Buffer
	buf.WriteString(header)
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&n); err != nil {
		return nil, err
	}
	return spaceSections(buf.Bytes()), nil
}

// flowShortLists switches lists of short scalars to flow style.
func flowShortLists(n *yaml.Node) {
	if n.Kind == yaml.SequenceNode && len(n.Content) > 0 {
		total, scalars := 0, true
		for _, c := range n.Content {
			if c.Kind != yaml.ScalarNode {
				scalars = false
				break
			}
			total += len(c.Value) + 2
		}
		if scalars && total <= 60 {
			n.Style = yaml.FlowStyle
		}
	}
	for _, c := range n.Content {
		flowShortLists(c)
	}
}

var (
	topLevelKey = regexp.MustCompile(`(?m)^([a-z_]+:)`)
	entryKey    = regexp.MustCompile(`(?m)^(  [a-z][a-z0-9-]*:\n    )`)
)

// spaceSections puts a blank line before top-level sections from services on, and
// before every entry of services and modules.
func spaceSections(b []byte) []byte {
	b = entryKey.ReplaceAll(b, []byte("\n$1"))
	b = topLevelKey.ReplaceAllFunc(b, func(k []byte) []byte {
		switch string(k) {
		case "version:", "project:", "domain:":
			return k
		}
		return append([]byte("\n"), k...)
	})
	// a section's first entry follows its key directly
	return regexp.MustCompile(`(?m)^([a-z_]+:)\n\n`).ReplaceAll(b, []byte("$1\n"))
}
