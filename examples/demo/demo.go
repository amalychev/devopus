// Package demo embeds the demo project: 'devopus workspace demo' creates a workspace
// with it for developing and debugging Devopus itself.
package demo

import _ "embed"

// Config is the demo project in the workspace.yaml v1 format.
//
//go:embed workspace.yaml
var Config []byte
