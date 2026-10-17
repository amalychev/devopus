package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// run executes the CLI in dir and returns the exit code and outputs.
func run(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	t.Chdir(dir)
	var out, errOut bytes.Buffer
	code := Run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const composeFile = `services:
  web:
    build: ./web
    ports: ["3000:3000"]
    environment:
      SESSION_SECRET: s3cr3t value
    depends_on: [db]
  db:
    image: postgres:17.2
    environment:
      POSTGRES_PASSWORD: devpass
`

func TestBadArgs(t *testing.T) {
	dir := t.TempDir()
	if code, _, _ := run(t, dir, "status", "--nope"); code != ExitInvalid {
		t.Errorf("unknown flag exit %d", code)
	}
	if code, _, _ := run(t, dir, "frobnicate"); code != ExitInvalid {
		t.Errorf("unknown command exit %d", code)
	}
	// The commands of the mode without a workspace are gone (R10).
	for _, c := range []string{"init", "validate", "schema"} {
		if code, _, _ := run(t, dir, c); code != ExitInvalid {
			t.Errorf("%s exit %d", c, code)
		}
	}
}
