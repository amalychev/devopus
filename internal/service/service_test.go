package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testService() *Service {
	return &Service{
		Component: Panel,
		Label:     "com.devopus.panel.test",
		Exe:       "/opt/Dev Tools/devopus",
		Home:      "/Users/a/.devopus",
		Log:       "/Users/a/.devopus/agent.log",
		Env:       map[string]string{"DEVOPUS_HOME": "/Users/a/.devopus", "PATH": "/opt/homebrew/bin:/usr/bin", "DOCKER_HOST": "unix:///x&y/docker.sock"},
	}
}

func TestDefinition(t *testing.T) {
	def, err := testService().Definition()
	if err != nil {
		t.Fatal(err)
	}
	s := string(def)
	switch runtime.GOOS {
	case "darwin":
		for _, want := range []string{"<string>com.devopus.panel.test</string>", "<string>/opt/Dev Tools/devopus</string>", "<string>panel</string>", "<string>run</string>",
			"<key>PATH</key>", "unix:///x&amp;y/docker.sock", "<key>SuccessfulExit</key>"} {
			if !strings.Contains(s, want) {
				t.Errorf("plist lacks %q:\n%s", want, s)
			}
		}
		if _, err := exec.LookPath("plutil"); err == nil {
			f := filepath.Join(t.TempDir(), "a.plist")
			if err := os.WriteFile(f, def, 0o644); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command("plutil", "-lint", f).CombinedOutput(); err != nil {
				t.Fatalf("plutil: %v: %s", err, out)
			}
		}
	case "linux":
		for _, want := range []string{`ExecStart="/opt/Dev Tools/devopus" panel run`, `Environment="DOCKER_HOST=unix:///x&y/docker.sock"`,
			"Restart=on-failure", "StandardOutput=append:/Users/a/.devopus/agent.log", "WantedBy=default.target"} {
			if !strings.Contains(s, want) {
				t.Errorf("unit lacks %q:\n%s", want, s)
			}
		}
	}
}

func TestSystemdQuote(t *testing.T) {
	if got := systemdQuote(`a "b" 50% c\d`); got != `"a \"b\" 50%% c\\d"` {
		t.Fatal(got)
	}
}

func TestLabels(t *testing.T) {
	t.Setenv("DEVOPUS_HOME", t.TempDir())
	for _, c := range []string{Agent, Panel} {
		s, err := For(c)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(s.Label, "com.devopus."+c+".") {
			t.Fatalf("a non-default home must get its own label, got %q", s.Label)
		}
		if s.Env["DEVOPUS_HOME"] == "" {
			t.Fatal("the service must pin DEVOPUS_HOME")
		}
	}
	if _, err := For("server"); err == nil {
		t.Fatal("unknown components must be refused")
	}
}
