package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

func TestInitFromComposeThenValidateAndExport(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "docker-compose.yml"), composeFile)
	write(t, filepath.Join(dir, "web", "Dockerfile"), "FROM scratch\n")

	code, out, errOut := run(t, dir, "init", "--no-pin", "--project", "shop")
	if code != ExitOK {
		t.Fatalf("init exit %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(errOut, "Importing docker-compose.yml") || !strings.Contains(out, "level B") {
		t.Errorf("output:\n%s\n%s", out, errOut)
	}
	if !strings.Contains(out, "Add .env.secrets to .gitignore") {
		t.Errorf("no .gitignore hint:\n%s", out)
	}
	ws, err := os.ReadFile(filepath.Join(dir, "workspace.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(ws), "# yaml-language-server: $schema=") || !strings.Contains(string(ws), "routes: [/]") {
		t.Errorf("workspace.yaml:\n%s", ws)
	}
	secrets, _ := os.ReadFile(filepath.Join(dir, ".env.secrets"))
	if string(secrets) != "POSTGRES_PASSWORD=devpass\nSESSION_SECRET=\"s3cr3t value\"\n" {
		t.Errorf(".env.secrets = %q", secrets)
	}

	// No overwrite without --force.
	if code, _, errOut := run(t, dir, "init", "--no-pin"); code != ExitError || !strings.Contains(errOut, "--force") {
		t.Errorf("second init exit %d: %s", code, errOut)
	}

	// validate finds the file from a subdirectory.
	if code, out, errOut := run(t, filepath.Join(dir, "web"), "validate"); code != ExitOK || !strings.Contains(out, "is valid") {
		t.Errorf("validate exit %d\n%s\n%s", code, out, errOut)
	}

	code, out, errOut = run(t, dir, "export", "compose")
	if code != ExitOK {
		t.Fatalf("export exit %d: %s", code, errOut)
	}
	for _, want := range []string{"SESSION_SECRET: ${SESSION_SECRET}", "127.0.0.1:8080:3000", "context: ./web"} {
		if !strings.Contains(out, want) {
			t.Errorf("export lacks %q:\n%s", want, out)
		}
	}
}

func TestValidateErrorsAndJSON(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "workspace.yaml"), `version: 1
project: shop
domain: shop.localhost
modules:
  web:
    path: web
    image: node:latest
    routes: ["/"]
`)
	code, _, errOut := run(t, dir, "validate")
	if code != ExitInvalid {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"workspace.yaml:7: error: modules.web.image: image \"node:latest\" is not pinned", "workspace.yaml:6: error: modules.web.path: folder \"web\" does not exist"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut)
		}
	}

	code, out, _ := run(t, dir, "validate", "--json")
	if code != ExitInvalid {
		t.Fatalf("json exit %d", code)
	}
	var res struct {
		Valid       bool
		Diagnostics []struct{ Severity, Path, Message string }
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if res.Valid || len(res.Diagnostics) != 2 || res.Diagnostics[0].Severity != "error" {
		t.Errorf("json = %+v", res)
	}

	if code, _, errOut := run(t, dir, "export", "compose"); code != ExitInvalid || !strings.Contains(errOut, "fix the errors") {
		t.Errorf("export of invalid file: exit %d %s", code, errOut)
	}
}

func TestNoWorkspaceAndBadArgs(t *testing.T) {
	dir := t.TempDir()
	if code, _, errOut := run(t, dir, "validate"); code != ExitInvalid || !strings.Contains(errOut, "docklet init") {
		t.Errorf("exit %d: %s", code, errOut)
	}
	if code, _, _ := run(t, dir, "validate", "--nope"); code != ExitInvalid {
		t.Errorf("unknown flag exit %d", code)
	}
	if code, _, _ := run(t, dir, "frobnicate"); code != ExitInvalid {
		t.Errorf("unknown command exit %d", code)
	}
}

func TestInitTemplateIsValid(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "My Shop")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := run(t, dir, "init"); code != ExitOK {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	ws, _ := os.ReadFile(filepath.Join(dir, "workspace.yaml"))
	if !strings.Contains(string(ws), "project: my-shop") {
		t.Errorf("workspace.yaml:\n%s", ws)
	}
	code, out, errOut := run(t, dir, "validate")
	if code != ExitOK {
		t.Fatalf("template does not validate: %d\n%s\n%s", code, out, errOut)
	}
}

func TestSchemaIsJSON(t *testing.T) {
	code, out, _ := run(t, t.TempDir(), "schema")
	var v map[string]any
	if code != ExitOK || json.Unmarshal([]byte(out), &v) != nil || v["title"] == nil {
		t.Errorf("schema exit %d", code)
	}
}
