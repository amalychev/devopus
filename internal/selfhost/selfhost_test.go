package selfhost

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// testHost runs docker for real and plays the rest: systemd, users and files go to a
// folder of the test. What it ran is recorded.
type testHost struct {
	root string
	ran  []string
}

func (h *testHost) Run(ctx context.Context, name string, args ...string) (string, error) {
	return h.RunInput(ctx, nil, name, args...)
}

func (h *testHost) RunInput(ctx context.Context, in io.Reader, name string, args ...string) (string, error) {
	h.ran = append(h.ran, name+" "+strings.Join(args, " "))
	switch name {
	case "docker":
		real := append([]string{}, args...)
		for i, a := range real {
			if a == PGEnvFile {
				real[i] = h.path(a) // the file is in the test's folder
			}
		}
		return System{}.RunInput(ctx, in, name, real...)
	case "systemctl":
		if len(args) > 0 && args[0] == "is-active" {
			return "active\n", nil
		}
	}
	return "", nil
}

func (h *testHost) path(p string) string              { return filepath.Join(h.root, p) }
func (h *testHost) ReadFile(p string) ([]byte, error) { return os.ReadFile(h.path(p)) }
func (h *testHost) WriteFile(p string, d []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(h.path(p)), 0o755); err != nil {
		return err
	}
	return os.WriteFile(h.path(p), d, perm)
}
func (h *testHost) Remove(p string) error                     { return os.RemoveAll(h.path(p)) }
func (h *testHost) MkdirAll(p string, perm os.FileMode) error { return os.MkdirAll(h.path(p), perm) }

func psql(t *testing.T, ctx context.Context, sql string) string {
	t.Helper()
	out, err := System{}.Run(ctx, "docker", "exec", PGContainer, "psql", "-U", "postgres", "-d", Database, "-Atc", sql)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(out)
}

func TestInstallBackupRestoreUninstall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if _, err := (System{}).Run(ctx, "docker", "version"); err != nil {
		t.Skipf("no Docker: %v", err)
	}
	if _, err := (System{}).Run(ctx, "docker", "inspect", PGContainer); err == nil {
		t.Skipf("%s exists on this machine: a real install, leave it alone", PGContainer)
	}
	h := &testHost{root: t.TempDir()}
	t.Cleanup(func() { _ = Uninstall(context.Background(), h, true) })

	o := Options{Domain: "ws.acme.test", AuthURL: "https://auth.devopus.example/", Binary: "/usr/local/bin/devopus", MailFrom: "devopus@acme.test"}
	if err := Install(ctx, h, o); err != nil {
		t.Fatal(err)
	}
	env, err := ReadEnv(h)
	if err != nil {
		t.Fatal(err)
	}
	if env["DEVOPUS_SERVER_DOMAIN"] != "ws.acme.test" || env["DEVOPUS_AUTH_URL"] != "https://auth.devopus.example" ||
		!strings.Contains(env["DEVOPUS_SERVER_DATABASE_URL"], "127.0.0.1:"+PGPort+"/"+Database) {
		t.Fatalf("env: %v", env)
	}
	for _, f := range []string{EnvFile, PGEnvFile} {
		if fi, err := os.Stat(h.path(f)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", f, fi, err)
		}
	}
	unit, _ := h.ReadFile(UnitFile)
	if !strings.Contains(string(unit), "ExecStart=/usr/local/bin/devopus server run") || !strings.Contains(string(unit), "User="+User) {
		t.Errorf("unit:\n%s", unit)
	}
	for _, r := range h.ran {
		if strings.Contains(r, env["DEVOPUS_SERVER_DB_PASSWORD"]) {
			t.Errorf("the password is in arguments: %s", r)
		}
	}
	if st := ReadStatus(ctx, h); !st.Installed || st.Database != "running" || st.Service != "active" {
		t.Errorf("status: %+v", st)
	}

	// Installing again keeps the password and the data.
	psql(t, ctx, "CREATE TABLE marks (v text); INSERT INTO marks VALUES ('before')")
	if err := Install(ctx, h, o); err != nil {
		t.Fatal(err)
	}
	if again, _ := ReadEnv(h); again["DEVOPUS_SERVER_DB_PASSWORD"] != env["DEVOPUS_SERVER_DB_PASSWORD"] {
		t.Error("a re-install changed the password")
	}
	if got := psql(t, ctx, "SELECT v FROM marks"); got != "before" {
		t.Errorf("data after a re-install: %q", got)
	}

	// Backup, a loss, restore.
	file, err := Backup(ctx, h, time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC))
	if err != nil || file != BackupDir+"/devopus-server-20261017-090000.dump" {
		t.Fatalf("backup: %q %v", file, err)
	}
	psql(t, ctx, "DROP TABLE marks")
	if err := Restore(ctx, h, file); err != nil {
		t.Fatal(err)
	}
	if got := psql(t, ctx, "SELECT v FROM marks"); got != "before" {
		t.Errorf("after restore: %q", got)
	}
	if !strings.Contains(strings.Join(h.ran, "\n"), "systemctl stop "+Service+"\ndocker exec -i "+PGContainer) {
		t.Errorf("the server was not stopped for the restore:\n%s", strings.Join(h.ran, "\n"))
	}

	// Uninstall keeps the database; purge removes it.
	if err := Uninstall(ctx, h, false); err != nil {
		t.Fatal(err)
	}
	if st := ReadStatus(ctx, h); st.Database != "stopped" {
		t.Errorf("after uninstall: %+v", st)
	}
	if err := Uninstall(ctx, h, true); err != nil {
		t.Fatal(err)
	}
	if _, err := (System{}).Run(ctx, "docker", "volume", "inspect", PGVolume); err == nil {
		t.Error("the volume is still there after purge")
	}
	if st := ReadStatus(ctx, h); st.Installed {
		t.Errorf("after purge: %+v", st)
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	var out strings.Builder
	o := Options{Domain: "ws.acme.test", AuthURL: "http://127.0.0.1:7480", Binary: "/usr/local/bin/devopus", Log: &out}
	if err := Install(context.Background(), DryRun{Out: &out}, o); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"Note: Auth is http://127.0.0.1:7480", "docker run -d --name " + PGContainer, "useradd --system", "write " + UnitFile, "systemctl enable " + Service} {
		if !strings.Contains(s, want) {
			t.Errorf("dry run lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "postgres://postgres:") {
		t.Error("the dry run shows the database password")
	}
	if err := Install(context.Background(), DryRun{Out: &out}, Options{}); err == nil {
		t.Error("an install without a domain")
	}
}

func TestDownloadChecksBinary(t *testing.T) {
	dir := t.TempDir()
	archive := "devopus_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	body := []byte("#!/bin/sh\necho new devopus\n")
	_ = tw.WriteHeader(&tar.Header{Name: "devopus", Mode: 0o755, Size: int64(len(body))})
	_, _ = tw.Write(body)
	_ = tw.Close()
	_ = zw.Close()
	os.WriteFile(filepath.Join(dir, archive), buf.Bytes(), 0o644)
	sum := sha256.Sum256(buf.Bytes())
	os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(hex.EncodeToString(sum[:])+"  "+archive+"\n"), 0o644)

	got, err := Download(context.Background(), "file://"+dir)
	if err != nil || string(got) != string(body) {
		t.Fatalf("download: %q %v", got, err)
	}
	// A tampered archive is refused.
	os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(strings.Repeat("0", 64)+"  "+archive+"\n"), 0o644)
	if _, err := Download(context.Background(), "file://"+dir); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Errorf("tampered: %v", err)
	}
	bin := filepath.Join(dir, "devopus")
	os.WriteFile(bin, []byte("old"), 0o755)
	if err := Replace(bin, body); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(bin); string(b) != string(body) {
		t.Error("not replaced")
	}
}
