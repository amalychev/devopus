package importer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/amalychev/devopus/internal/compose"
	"github.com/amalychev/devopus/internal/workspace"
)

// TestCorpus runs the compose corpus of P-8 (prototypes/p8-compose-import/.corpus, kept
// out of git) through the stage 1 import and into the model: every workspace.yaml
// without errors must convert.
func TestCorpus(t *testing.T) {
	files, _ := filepath.Glob("../../prototypes/p8-compose-import/.corpus/*compose*.y*ml")
	if len(files) == 0 {
		t.Skip("no corpus here")
	}
	converted := 0
	for _, f := range files {
		res, err := compose.Import(context.Background(), []string{f}, compose.Options{Root: filepath.Dir(f)})
		if err != nil {
			t.Logf("%s: compose import: %v", filepath.Base(f), err)
			continue
		}
		data, err := workspace.Marshal(res.Workspace, "")
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := workspace.Parse(data)
		if err != nil || parsed.Diagnostics.HasErrors() {
			t.Logf("%s: workspace.yaml has errors (level C), skipped: %v %v", filepath.Base(f), err, parsed.Diagnostics.Errors())
			continue
		}
		p, err := FromV1(parsed.Workspace)
		if err != nil {
			t.Errorf("%s: %v", filepath.Base(f), err)
			continue
		}
		if len(p.Apps)+len(p.Services) == 0 {
			t.Errorf("%s: empty project", filepath.Base(f))
		}
		converted++
	}
	t.Logf("%d of %d compose files converted", converted, len(files))
}
