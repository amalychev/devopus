// Package selfhost installs the workspace server on a customer's own server (stage 4,
// ADR-0016): Ubuntu 24.04+ with Docker and systemd. PostgreSQL runs as a container
// with a volume; the server is the devopus binary run by systemd as a system user,
// with its settings in an environment file only root reads. Every action goes through
// a Host, so a dry run prints them and tests check them.
package selfhost

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Where things go on the server.
const (
	EnvFile     = "/etc/devopus/server.env"
	PGEnvFile   = "/etc/devopus/postgres.env"
	UnitFile    = "/etc/systemd/system/devopus-server.service"
	Service     = "devopus-server"
	DataDir     = "/var/lib/devopus-server"
	BackupDir   = "/var/backups/devopus-server"
	User        = "devopus"
	PGContainer = "devopus-server-postgres"
	PGVolume    = "devopus-server-pgdata"
	PGImage     = "postgres:17-alpine"
	PGPort      = "5439"
	Database    = "devopus"
)

// Host runs commands and writes files on the server.
type Host interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
	// RunInput runs a command with stdin (a restore feeds the dump in).
	RunInput(ctx context.Context, in io.Reader, name string, args ...string) (string, error)
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte, perm os.FileMode) error
	Remove(path string) error
	MkdirAll(path string, perm os.FileMode) error
}

// System is the real host.
type System struct{}

func (System) Run(ctx context.Context, name string, args ...string) (string, error) {
	return System{}.RunInput(ctx, nil, name, args...)
}

func (System) RunInput(ctx context.Context, in io.Reader, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = in
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(errOut.String()))
	}
	return out.String(), nil
}

func (System) ReadFile(p string) ([]byte, error) { return os.ReadFile(p) }
func (System) WriteFile(p string, d []byte, perm os.FileMode) error {
	if err := os.WriteFile(p, d, perm); err != nil {
		return err
	}
	return os.Chmod(p, perm)
}
func (System) Remove(p string) error {
	if err := os.RemoveAll(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
func (System) MkdirAll(p string, perm os.FileMode) error { return os.MkdirAll(p, perm) }

// DryRun prints what would be done and does nothing; reads find nothing.
type DryRun struct{ Out io.Writer }

func (d DryRun) Run(_ context.Context, name string, args ...string) (string, error) {
	fmt.Fprintf(d.Out, "  $ %s %s\n", name, strings.Join(args, " "))
	// Questions about what is installed answer "nothing": the dry run shows a fresh install.
	if name == "id" || (name == "docker" && len(args) > 0 && args[0] == "inspect") {
		return "", errors.New("dry run")
	}
	return "", nil
}
func (d DryRun) RunInput(ctx context.Context, _ io.Reader, name string, args ...string) (string, error) {
	return d.Run(ctx, name, args...)
}
func (d DryRun) ReadFile(string) ([]byte, error) { return nil, os.ErrNotExist }
func (d DryRun) WriteFile(p string, data []byte, perm os.FileMode) error {
	fmt.Fprintf(d.Out, "  write %s (%o):\n", p, perm)
	for _, l := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if k, _, ok := strings.Cut(l, "="); ok && (k == "DEVOPUS_SERVER_DATABASE_URL" || k == "DEVOPUS_SERVER_DB_PASSWORD" || k == "POSTGRES_PASSWORD") {
			l = k + "=…"
		}
		fmt.Fprintf(d.Out, "    %s\n", l)
	}
	return nil
}
func (d DryRun) Remove(p string) error { fmt.Fprintf(d.Out, "  remove %s\n", p); return nil }
func (d DryRun) MkdirAll(p string, perm os.FileMode) error {
	fmt.Fprintf(d.Out, "  mkdir %s (%o)\n", p, perm)
	return nil
}

// Options of an install.
type Options struct {
	// Domain the server answers on, with HTTPS from Let's Encrypt; its DNS must point
	// here and ports 80 and 443 be open.
	Domain string
	// AuthURL is Devopus's Auth: accounts stay in the cloud, the workspace here.
	AuthURL string
	// Mail sends invites: "log" or smtp://user:password@host:587, from MailFrom.
	Mail, MailFrom string
	// Binary is the devopus binary systemd runs.
	Binary string
	Log    io.Writer
}

func (o Options) say(format string, a ...any) {
	if o.Log != nil {
		fmt.Fprintf(o.Log, format+"\n", a...)
	}
}

// ReadEnv reads the settings of an installed server.
func ReadEnv(h Host) (map[string]string, error) {
	b, err := h.ReadFile(EnvFile)
	if err != nil {
		return nil, err
	}
	env := map[string]string{}
	for _, l := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok && !strings.HasPrefix(k, "#") {
			env[k] = v
		}
	}
	return env, nil
}

func envFile(env map[string]string) []byte {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("# The Devopus workspace server (devopus server install). Root reads it: it holds\n# the database password.\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, env[k])
	}
	return []byte(b.String())
}

// Unit is the systemd service.
func Unit(binary string) string {
	return fmt.Sprintf(`[Unit]
Description=Devopus workspace server
After=network-online.target docker.service
Wants=network-online.target
Requires=docker.service

[Service]
User=%s
EnvironmentFile=%s
ExecStart=%s server run
Restart=always
RestartSec=3
# Ports 80 and 443 without root.
AmbientCapabilities=CAP_NET_BIND_SERVICE
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=%s
PrivateTmp=true

[Install]
WantedBy=multi-user.target
`, User, EnvFile, binary, DataDir)
}

func password() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Install sets the server up, or updates an installed one: the database password and
// data stay.
func Install(ctx context.Context, h Host, o Options) error {
	if o.Domain == "" {
		return errors.New("name the domain the server answers on: --domain ws.example.com")
	}
	if o.AuthURL == "" || o.Binary == "" {
		return errors.New("the Auth URL and the binary are not set")
	}
	if u, err := url.Parse(o.AuthURL); err == nil {
		if ip := net.ParseIP(u.Hostname()); (ip != nil && ip.IsLoopback()) || u.Hostname() == "localhost" {
			o.say("Note: Auth is %s, on this machine. People sign in with Devopus's Auth: pass its address with --auth-url.", o.AuthURL)
		}
	}
	if _, err := h.Run(ctx, "docker", "version", "--format", "{{.Server.Version}}"); err != nil {
		return fmt.Errorf("Docker does not answer: install it first (https://docs.docker.com/engine/install/ubuntu/): %w", err)
	}
	if _, err := h.Run(ctx, "systemctl", "--version"); err != nil {
		return fmt.Errorf("systemd is needed: %w", err)
	}

	env, err := ReadEnv(h)
	pw := ""
	if err == nil {
		pw = env["DEVOPUS_SERVER_DB_PASSWORD"]
		o.say("Updating the installed server (its data stays).")
	}
	if pw == "" {
		pw = password()
	}

	o.say("The service user and folders…")
	if _, err := h.Run(ctx, "id", "-u", User); err != nil {
		if _, err := h.Run(ctx, "useradd", "--system", "--home-dir", DataDir, "--shell", "/usr/sbin/nologin", User); err != nil {
			return err
		}
	}
	for _, d := range []string{filepath.Dir(EnvFile), DataDir, BackupDir} {
		if err := h.MkdirAll(d, 0o750); err != nil {
			return err
		}
	}
	if _, err := h.Run(ctx, "chown", User+":"+User, DataDir); err != nil {
		return err
	}

	o.say("PostgreSQL in a container (%s, volume %s)…", PGContainer, PGVolume)
	// The password goes in a file, not in arguments every process can list.
	if err := h.WriteFile(PGEnvFile, []byte("POSTGRES_PASSWORD="+pw+"\nPOSTGRES_DB="+Database+"\n"), 0o600); err != nil {
		return err
	}
	if _, err := h.Run(ctx, "docker", "inspect", "--format", "{{.State.Running}}", PGContainer); err != nil {
		if _, err := h.Run(ctx, "docker", "run", "-d", "--name", PGContainer, "--restart", "unless-stopped", "--env-file", PGEnvFile,
			"-v", PGVolume+":/var/lib/postgresql/data", "-p", "127.0.0.1:"+PGPort+":5432", PGImage); err != nil {
			return err
		}
	} else if _, err := h.Run(ctx, "docker", "start", PGContainer); err != nil {
		return err
	}
	if err := waitPostgres(ctx, h); err != nil {
		return err
	}

	mail := o.Mail
	if mail == "" {
		mail = "log"
	}
	next := map[string]string{
		"DEVOPUS_SERVER_DATABASE_URL": fmt.Sprintf("postgres://postgres:%s@127.0.0.1:%s/%s?sslmode=disable", pw, PGPort, Database),
		"DEVOPUS_SERVER_DB_PASSWORD":  pw,
		"DEVOPUS_SERVER_DOMAIN":       o.Domain,
		"DEVOPUS_SERVER_DATA":         DataDir,
		"DEVOPUS_AUTH_URL":            strings.TrimRight(o.AuthURL, "/"),
		"DEVOPUS_SERVER_MAIL":         mail,
	}
	if o.MailFrom != "" {
		next["DEVOPUS_SERVER_MAIL_FROM"] = o.MailFrom
	}
	if err := h.WriteFile(EnvFile, envFile(next), 0o600); err != nil {
		return err
	}
	if err := h.WriteFile(UnitFile, []byte(Unit(o.Binary)), 0o644); err != nil {
		return err
	}
	o.say("The service…")
	for _, args := range [][]string{{"daemon-reload"}, {"enable", Service}, {"restart", Service}} {
		if _, err := h.Run(ctx, "systemctl", args...); err != nil {
			return err
		}
	}
	return nil
}

func waitPostgres(ctx context.Context, h Host) error {
	if _, dry := h.(DryRun); dry {
		return nil
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		_, err := h.Run(ctx, "docker", "exec", PGContainer, "pg_isready", "-q", "-U", "postgres", "-d", Database)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("PostgreSQL did not start: %w (docker logs %s)", err, PGContainer)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// Health asks the running server whether it and its database answer.
func Health(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(url, "/")+"/healthz", nil)
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(res.Body)
		return fmt.Errorf("%s: %s", res.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

// Status describes the installed server.
type Status struct {
	Installed bool
	Domain    string
	Service   string // active, inactive, failed…
	Database  string // running, stopped, missing
}

func ReadStatus(ctx context.Context, h Host) Status {
	var st Status
	env, err := ReadEnv(h)
	if err != nil {
		return st
	}
	st.Installed, st.Domain = true, env["DEVOPUS_SERVER_DOMAIN"]
	out, _ := h.Run(ctx, "systemctl", "is-active", Service)
	st.Service = strings.TrimSpace(out)
	switch out, err := h.Run(ctx, "docker", "inspect", "--format", "{{.State.Running}}", PGContainer); {
	case err != nil:
		st.Database = "missing"
	case strings.TrimSpace(out) == "true":
		st.Database = "running"
	default:
		st.Database = "stopped"
	}
	return st
}

// Backup dumps the database into the backup folder and returns the file.
func Backup(ctx context.Context, h Host, now time.Time) (string, error) {
	if err := h.MkdirAll(BackupDir, 0o750); err != nil {
		return "", err
	}
	dump, err := h.Run(ctx, "docker", "exec", PGContainer, "pg_dump", "-U", "postgres", "-Fc", Database)
	if err != nil {
		return "", err
	}
	file := filepath.Join(BackupDir, "devopus-server-"+now.UTC().Format("20060102-150405")+".dump")
	return file, h.WriteFile(file, []byte(dump), 0o600)
}

// Restore loads a backup into the database, with the server stopped meanwhile.
func Restore(ctx context.Context, h Host, file string) error {
	data, err := h.ReadFile(file)
	if err != nil {
		return err
	}
	if _, err := h.Run(ctx, "systemctl", "stop", Service); err != nil {
		return err
	}
	_, rerr := h.RunInput(ctx, bytes.NewReader(data), "docker", "exec", "-i", PGContainer,
		"pg_restore", "-U", "postgres", "-d", Database, "--clean", "--if-exists", "--no-owner")
	if _, err := h.Run(ctx, "systemctl", "start", Service); err != nil && rerr == nil {
		rerr = err
	}
	return rerr
}

// Uninstall removes the service; purge removes the database with its data too.
func Uninstall(ctx context.Context, h Host, purge bool) error {
	_, _ = h.Run(ctx, "systemctl", "disable", "--now", Service)
	if err := h.Remove(UnitFile); err != nil {
		return err
	}
	_, _ = h.Run(ctx, "systemctl", "daemon-reload")
	if !purge {
		_, _ = h.Run(ctx, "docker", "stop", PGContainer)
		return nil
	}
	_, _ = h.Run(ctx, "docker", "rm", "-f", PGContainer)
	_, _ = h.Run(ctx, "docker", "volume", "rm", PGVolume)
	for _, p := range []string{EnvFile, PGEnvFile, DataDir} {
		if err := h.Remove(p); err != nil {
			return err
		}
	}
	return nil
}

// ReleaseURL is where releases are: $DEVOPUS_BASE_URL, as for install.sh.
func ReleaseURL(version string) string {
	if b := os.Getenv("DEVOPUS_BASE_URL"); b != "" {
		return strings.TrimRight(b, "/")
	}
	if version == "" || version == "latest" {
		return "https://github.com/amalychev/devopus/releases/latest/download"
	}
	return "https://github.com/amalychev/devopus/releases/download/v" + strings.TrimPrefix(version, "v")
}

// Download fetches the devopus binary of a release for this machine, checked against
// the release's checksums.
func Download(ctx context.Context, base string) ([]byte, error) {
	archive := fmt.Sprintf("devopus_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	sums, err := fetch(ctx, base+"/checksums.txt")
	if err != nil {
		return nil, err
	}
	want := ""
	for _, l := range strings.Split(string(sums), "\n") {
		if f := strings.Fields(l); len(f) == 2 && f[1] == archive {
			want = f[0]
		}
	}
	if want == "" {
		return nil, fmt.Errorf("checksums.txt has no line for %s", archive)
	}
	tgz, err := fetch(ctx, base+"/"+archive)
	if err != nil {
		return nil, err
	}
	if sum := sha256.Sum256(tgz); hex.EncodeToString(sum[:]) != want {
		return nil, fmt.Errorf("%s does not match its checksum", archive)
	}
	zr, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("%s has no devopus binary", archive)
		}
		if err != nil {
			return nil, err
		}
		if filepath.Base(hdr.Name) == "devopus" {
			return io.ReadAll(io.LimitReader(tr, 512<<20))
		}
	}
}

func fetch(ctx context.Context, url string) ([]byte, error) {
	if path, ok := strings.CutPrefix(url, "file://"); ok {
		return os.ReadFile(path)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, res.Status)
	}
	return io.ReadAll(io.LimitReader(res.Body, 512<<20))
}

// Replace puts a new binary in place of the running one with a rename: the running
// server keeps its file until it restarts.
func Replace(binary string, data []byte) error {
	tmp := binary + ".new"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, binary)
}
