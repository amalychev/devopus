package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/devopus/devopus/gen/devopus/agent/v1"
	"github.com/devopus/devopus/internal/agent/client"
	"github.com/devopus/devopus/internal/agent/paths"
	"github.com/devopus/devopus/internal/agent/proxy"
	"github.com/devopus/devopus/internal/agent/runtime"
	"github.com/devopus/devopus/internal/agent/store"
)

// Each module's migration records which environment ran it, once: the base writes
// "main", a feature its slug on top of the data it copied from main.
const dataConfig = `version: 1
project: %s
domain: %s.localhost
services:
  pg:
    image: %s
    port: 5432
    env:
      POSTGRES_USER: app
      POSTGRES_PASSWORD: dev
      POSTGRES_DB: app
    volumes: ["pgdata:%s"]
    healthcheck:
      test: pg_isready -U app -d app
      interval: 1s
      retries: 60
  my:
    image: mysql:8.4.3
    port: 3306
    env:
      MYSQL_ROOT_PASSWORD: dev
      MYSQL_DATABASE: app
    volumes: ["mydata:/var/lib/mysql"]
    healthcheck:
      test: mysqladmin ping -h 127.0.0.1 -uroot -pdev --silent
      interval: 1s
      retries: 120
modules:
  pgapp:
    path: pgapp
    image: %s
    uses: [pg]
    command: sleep infinity
    env:
      PGPASSWORD: dev
    hooks:
      migrate: >-
        psql -v ON_ERROR_STOP=1 -h pg -U app -d app -c "CREATE TABLE IF NOT EXISTS runs(env text primary key);
        INSERT INTO runs VALUES ('${{ env.slug }}') ON CONFLICT DO NOTHING"
  myapp:
    path: myapp
    image: mysql:8.4.3
    uses: [my]
    command: sleep infinity
    hooks:
      migrate: >-
        mysql -h my -uroot -pdev app -e "CREATE TABLE IF NOT EXISTS runs(env varchar(64) primary key);
        INSERT IGNORE INTO runs VALUES ('${{ env.slug }}')"
`

// TestFeatureData copies the base environment's Postgres and MySQL into a feature
// (US-4, ADR-0007): the feature starts with main's data, its migrations change only its
// copy, and a reset brings main's data back.
func TestFeatureData(t *testing.T) {
	rt, err := runtime.New(runtime.Detect())
	if err != nil {
		t.Skip(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	if _, err := rt.Info(ctx); err != nil {
		t.Skipf("no container runtime: %v", err)
	}
	userHome, _ := os.UserHomeDir()
	home, err := os.MkdirTemp(userHome, ".dk-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("DEVOPUS_HOME", home)

	pgImage, pgData := "postgres:17.6", "/var/lib/postgresql/data"
	if v := os.Getenv("DEVOPUS_TEST_PG"); v != "" {
		pgImage, pgData = v, "/var/lib/postgresql"
	}
	project := "dt-" + store.NewID()
	root := filepath.Join(home, "repo")
	for _, d := range []string{"pgapp", "myapp"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
		os.WriteFile(filepath.Join(root, d, "README"), []byte(d), 0o644)
	}
	config := fmt.Appendf(nil, dataConfig, project, project, pgImage, pgData, pgImage)
	os.WriteFile(filepath.Join(root, "workspace.yaml"), config, 0o644)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "."}, {"-c", "commit.gpgsign=false", "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	wsPath := filepath.Join(root, "workspace.yaml")

	logPath := filepath.Join(home, "agent.log")
	logFile, _ := os.Create(logPath)
	t.Cleanup(func() {
		logFile.Close()
		if t.Failed() {
			data, _ := os.ReadFile(logPath)
			t.Logf("agent log:\n%s", data)
		}
	})
	ports := proxy.Ports{HTTP: freePort(t), HTTPS: freePort(t)}
	done := make(chan error, 1)
	agentCtx, stop := context.WithCancel(context.Background())
	go func() {
		done <- Run(agentCtx, Options{Version: "test", Log: slog.New(slog.NewTextHandler(logFile, &slog.HandlerOptions{Level: slog.LevelWarn})), ProxyPorts: &ports})
	}()
	t.Cleanup(func() {
		stop()
		<-done
	})
	c := client.New(paths.Socket())
	for i := 0; client.Ping(ctx, c) != nil; i++ {
		if i > 100 {
			t.Fatal("agent did not start")
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Cleanup(func() { purge(t, rt, c, project) })

	up, err := c.Up(ctx, connect.NewRequest(&agentv1.UpRequest{WorkspacePath: wsPath, Config: config}))
	if err != nil {
		t.Fatal(err)
	}
	baseID := up.Msg.EnvironmentId
	waitFor(t, ctx, c, baseID, func(s *agentv1.EnvironmentStatus) bool { return s.Phase == agentv1.Phase_PHASE_RUNNING })
	pgRuns := func(env string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "docker", "exec", project+"-env-"+env+"-pg", "psql", "-U", "app", "-d", "app", "-Atc", "SELECT string_agg(env, ',' ORDER BY env) FROM runs").CombinedOutput()
		if err != nil {
			return fmt.Sprintf("error: %v %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	myRuns := func(env string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "docker", "exec", project+"-env-"+env+"-my", "mysql", "-uroot", "-pdev", "app", "-NBe", "SELECT GROUP_CONCAT(env ORDER BY env) FROM runs").CombinedOutput()
		if err != nil {
			return fmt.Sprintf("error: %v %s", err, out)
		}
		return strings.TrimSpace(strings.ReplaceAll(string(out), "mysql: [Warning] Using a password on the command line interface can be insecure.", ""))
	}
	if got := pgRuns(baseID); got != "main" {
		t.Fatalf("base pg = %q", got)
	}
	// Data main has besides its migrations, made by hand: the copy carries it too.
	exec.CommandContext(ctx, "docker", "exec", project+"-env-"+baseID+"-pg", "psql", "-U", "app", "-d", "app", "-c", "INSERT INTO runs VALUES ('by-hand')").Run()

	// NFR-2 by hand: DEVOPUS_TEST_PG_SCALE=68 makes main's database about 1 GB.
	if scale := os.Getenv("DEVOPUS_TEST_PG_SCALE"); scale != "" {
		out, err := exec.CommandContext(ctx, "docker", "exec", project+"-env-"+baseID+"-pg", "pgbench", "-i", "-q", "-s", scale, "-U", "app", "app").CombinedOutput()
		if err != nil {
			t.Fatalf("pgbench: %v %s", err, out)
		}
		size, _ := exec.CommandContext(ctx, "docker", "exec", project+"-env-"+baseID+"-pg", "psql", "-U", "app", "-d", "app", "-Atc", "SELECT pg_size_pretty(pg_database_size('app'))").Output()
		t.Logf("main's database: %s", strings.TrimSpace(string(size)))
	}

	start := time.Now()
	f, err := c.CreateFeature(ctx, connect.NewRequest(&agentv1.CreateFeatureRequest{WorkspacePath: wsPath, Config: config, Name: "f1", Modules: []string{"pgapp", "myapp"}}))
	if err != nil {
		t.Fatal(err)
	}
	fID := f.Msg.EnvironmentId
	st := waitFor(t, ctx, c, fID, func(s *agentv1.EnvironmentStatus) bool { return s.Phase == agentv1.Phase_PHASE_RUNNING })
	t.Logf("feature with two database copies is up in %s", time.Since(start).Round(100*time.Millisecond))
	for _, s := range st.Steps {
		if s.Name == "data" {
			t.Logf("data: %s", s.Message)
		}
	}
	if got := pgRuns(fID); got != "by-hand,f1,main" {
		t.Errorf("feature pg = %q, want main's data and its own migration", got)
	}
	if got := myRuns(fID); got != "f1,main" {
		t.Errorf("feature mysql = %q", got)
	}
	if got := pgRuns(baseID); got != "by-hand,main" {
		t.Errorf("base pg after the feature = %q", got)
	}
	if got := myRuns(baseID); got != "main" {
		t.Errorf("base mysql after the feature = %q", got)
	}
	if len(st.Data) != 2 || st.Data[0].Behind || st.Data[0].CopiedAt == nil {
		t.Errorf("feature data status = %v", st.Data)
	}

	// The feature writes to its copy; a reset throws that away.
	exec.CommandContext(ctx, "docker", "exec", project+"-env-"+fID+"-pg", "psql", "-U", "app", "-d", "app", "-c", "INSERT INTO runs VALUES ('scratch')").Run()
	reset, err := c.ResetData(ctx, connect.NewRequest(&agentv1.ResetDataRequest{Project: project, Environment: "f1", Services: []string{"pg"}}))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, ctx, c, fID, func(s *agentv1.EnvironmentStatus) bool {
		return s.Phase == agentv1.Phase_PHASE_RUNNING && s.AppliedRevision >= reset.Msg.Revision
	})
	if got := pgRuns(fID); got != "by-hand,f1,main" {
		t.Errorf("feature pg after reset = %q", got)
	}

	// A golden refresh takes main's data as it is now; the feature's copy is behind.
	exec.CommandContext(ctx, "docker", "exec", project+"-env-"+baseID+"-pg", "psql", "-U", "app", "-d", "app", "-c", "INSERT INTO runs VALUES ('later')").Run()
	refresh, err := c.ResetData(ctx, connect.NewRequest(&agentv1.ResetDataRequest{Project: project, Services: []string{"pg"}}))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, ctx, c, baseID, func(s *agentv1.EnvironmentStatus) bool {
		return s.Phase == agentv1.Phase_PHASE_RUNNING && s.AppliedRevision >= refresh.Msg.Revision
	})
	if _, err := c.ResetData(ctx, connect.NewRequest(&agentv1.ResetDataRequest{Project: project, Environment: "f1", Services: []string{"nope"}})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("reset of an unknown database: %v", err)
	}
	reset, err = c.ResetData(ctx, connect.NewRequest(&agentv1.ResetDataRequest{Project: project, Environment: "f1"}))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, ctx, c, fID, func(s *agentv1.EnvironmentStatus) bool {
		return s.Phase == agentv1.Phase_PHASE_RUNNING && s.AppliedRevision >= reset.Msg.Revision
	})
	if got := pgRuns(fID); got != "by-hand,f1,later,main" {
		t.Errorf("feature pg after a golden refresh and a reset = %q", got)
	}
}
