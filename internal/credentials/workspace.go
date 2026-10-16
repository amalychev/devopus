package credentials

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/amalychev/devopus/internal/agent/paths"
)

// Workspace is the workspace this device works in: the CLI's commands go to it, and
// the agent runs its projects (devopus workspace use).
type Workspace struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	ServerURL string `json:"serverUrl"`
}

// WorkspaceFile keeps the current workspace of the data directory.
func WorkspaceFile() string { return filepath.Join(paths.Home(), "workspace.json") }

// CurrentWorkspace is the current workspace; nil when none is chosen.
func CurrentWorkspace() (*Workspace, error) {
	b, err := os.ReadFile(WorkspaceFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var w Workspace
	if err := json.Unmarshal(b, &w); err != nil {
		return nil, err
	}
	return &w, nil
}

// SetCurrentWorkspace chooses the current workspace; nil forgets it.
func SetCurrentWorkspace(w *Workspace) error {
	if w == nil {
		err := os.Remove(WorkspaceFile())
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	b, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(WorkspaceFile(), append(b, '\n'))
}
