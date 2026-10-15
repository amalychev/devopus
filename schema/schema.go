// Package schema embeds the JSON Schema of workspace.yaml so the binary validates
// and prints it without network access.
package schema

import _ "embed"

//go:embed workspace.v1.json
var WorkspaceV1 []byte

// WorkspaceV1ID is the $id of the schema.
const WorkspaceV1ID = "urn:devopus:workspace:v1"

// URL is where the schema is published for editors (yaml-language-server).
const URL = "https://raw.githubusercontent.com/devopus/devopus/main/schema/workspace.v1.json"
