//go:build integration

// Integration test for P-1/P-2. Needs a running Docker; run with
//
//	go test -tags integration -run TestIsolation -v -timeout 15m
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIsolation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DOCKLET_HOME", home)
	repo := filepath.Join(t.TempDir(), "shop")
	copyDir(t, "example/shop", repo)
	sh(t, repo, "git", "init", "-q", "-b", "main")
	sh(t, repo, "git", "add", "-A")
	sh(t, repo, "git", "-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false", "commit", "-qm", "init")

	cli := func(t *testing.T, args ...string) {
		t.Helper()
		if err := run(args, testWriter{t}); err != nil {
			t.Fatalf("docklet %s: %v", strings.Join(args, " "), err)
		}
	}
	t.Cleanup(func() { _ = run([]string{"down", "--purge", "--force"}, io.Discard) })

	cfg := filepath.Join(repo, "workspace.yaml")
	cli(t, "up", "-f", cfg)

	start := time.Now()
	cli(t, "feature", "create", "PAY-123", "--modules", "payments")
	t.Logf("feature with 1 fork (+1 dependent, own Postgres) created in %s", time.Since(start).Round(100*time.Millisecond))
	cli(t, "feature", "create", "PAY-124", "--modules", "payments")
	cli(t, "feature", "create", "UI-7", "--modules", "frontend")

	st, ws, err := loadAll()
	if err != nil {
		t.Fatal(err)
	}
	client, err := HTTPClient(st)
	if err != nil {
		t.Fatal(err)
	}
	env := func(name string) *Env {
		if name == "base" {
			return st.Base()
		}
		return st.FindFeature(name)
	}
	get := func(t *testing.T, envName, path string) map[string]any {
		t.Helper()
		resp, err := client.Get(st.URL(env(envName), ws) + path)
		if err != nil {
			t.Fatalf("%s%s: %v", envName, path, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("%s%s: %d %s", envName, path, resp.StatusCode, body)
		}
		var out map[string]any
		if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
			if err := json.Unmarshal(body, &out); err != nil {
				t.Fatal(err)
			}
		} else {
			out = map[string]any{"body": string(body)}
		}
		return out
	}

	t.Run("routing picks the feature's copy and falls back to main", func(t *testing.T) {
		pay := env("PAY-123")
		got := get(t, "PAY-123", "/api/checkout?amount=7")
		if got["env"] != pay.ID {
			t.Errorf("/api served by %v, want feature copy %s", got["env"], pay.ID)
		}
		if charge := got["charge"].(map[string]any); charge["env"] != pay.ID {
			t.Errorf("backend reached payments of %v, want %s (logical name http://payments)", charge["env"], pay.ID)
		}
		if got := get(t, "PAY-123", "/"); !strings.Contains(got["body"].(string), "shop frontend: main") {
			t.Errorf("frontend in PAY-123 should come from main, got %q", got["body"])
		}
	})

	t.Run("data written in one feature is invisible elsewhere", func(t *testing.T) {
		count := func(envName string) int {
			return len(get(t, envName, "/payments/list")["charges"].([]any))
		}
		if n := count("PAY-123"); n != 1 {
			t.Errorf("PAY-123 has %d charges, want 1", n)
		}
		for _, other := range []string{"PAY-124", "base"} {
			if n := count(other); n != 0 {
				t.Errorf("%s sees %d charges written in PAY-123", other, n)
			}
		}
	})

	t.Run("worktree change is served only by its feature", func(t *testing.T) {
		ui := env("UI-7")
		page := filepath.Join(ui.Code, "frontend", "index.html")
		data, _ := os.ReadFile(page)
		if err := os.WriteFile(page, []byte(strings.Replace(string(data), "frontend: main", "frontend: UI-7", 1)), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := get(t, "UI-7", "/")["body"].(string); !strings.Contains(got, "frontend: UI-7") {
			t.Errorf("UI-7 does not serve its worktree: %q", got)
		}
		for _, other := range []string{"base", "PAY-123"} {
			if got := get(t, other, "/")["body"].(string); !strings.Contains(got, "frontend: main") {
				t.Errorf("%s picked up UI-7's change: %q", other, got)
			}
		}
	})

	t.Run("shared services are reachable from a feature by logical name", func(t *testing.T) {
		out, err := docker("exec", st.Container(env("PAY-123"), "backend"), "python", "-c",
			"import socket;print(socket.create_connection(('mail',1025),3).recv(64))")
		if err != nil || !strings.Contains(out, "220") {
			t.Errorf("mail from PAY-123 backend: %q %v", out, err)
		}
	})

	t.Run("docker resources are namespaced and not published", func(t *testing.T) {
		out, err := docker("ps", "-a", "--filter", "label="+labelProject+"=shop", "--format", `{{.Names}}|{{.Label "com.docklet.env"}}|{{.Ports}}`)
		if err != nil {
			t.Fatal(err)
		}
		names := map[string]bool{}
		for _, line := range strings.Split(out, "\n") {
			parts := strings.SplitN(line, "|", 3)
			if parts[0] == envoyName {
				continue
			}
			if names[parts[0]] {
				t.Errorf("duplicate container name %s", parts[0])
			}
			names[parts[0]] = true
			if !strings.Contains(parts[0], parts[1]) {
				t.Errorf("container %s does not carry its env id %q", parts[0], parts[1])
			}
			if strings.Contains(parts[2], "->") {
				t.Errorf("container %s publishes ports to the host: %s", parts[0], parts[2])
			}
		}
		vols, _ := docker("volume", "ls", "-q", "--filter", "label="+labelProject+"=shop")
		if n := len(strings.Fields(vols)); n != 3 {
			t.Errorf("want 3 Postgres volumes (base, PAY-123, PAY-124), got %d: %s", n, vols)
		}
	})

	t.Run("client-supplied baggage is dropped at the edge", func(t *testing.T) {
		req := st.URL(env("PAY-123"), ws) + "/api/whoami"
		r, _ := httpGetWithHeader(client, req, "baggage", "feature=pay-124")
		if r["env"] != env("PAY-123").ID {
			t.Errorf("spoofed baggage changed routing: %v", r)
		}
	})

	t.Run("removing a feature with uncommitted work is refused", func(t *testing.T) {
		err := run([]string{"feature", "rm", "UI-7"}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "uncommitted") {
			t.Fatalf("want refusal for uncommitted change, got %v", err)
		}
		cli(t, "feature", "rm", "PAY-124")
		t.Logf("envoy restart on config change: every environment unavailable for %s", LastEnvoyGap.Round(10*time.Millisecond))
		cli(t, "feature", "rm", "UI-7", "--force")
		if got := get(t, "PAY-123", "/payments/list"); len(got["charges"].([]any)) != 1 {
			t.Errorf("removing other features touched PAY-123 data: %v", got)
		}
	})
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func sh(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	sh(t, ".", "cp", "-R", src, dst)
}

func httpGetWithHeader(client *http.Client, url, key, value string) (map[string]any, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(key, value)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	return out, json.NewDecoder(resp.Body).Decode(&out)
}
