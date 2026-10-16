// Package pgtest gives tests a real PostgreSQL: one container per test binary, a fresh
// database per test. Tests skip when no container runtime answers. To use a server of
// your own instead, set DEVOPUS_TEST_DATABASE_URL to a connection URL with the right
// to create databases.
//
// The package's TestMain must run the tests with Run so the container goes away after
// them: func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }
package pgtest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/amalychev/devopus/internal/agent/runtime"
)

// Image is the PostgreSQL the tests run against.
const Image = "postgres:17-alpine"

var (
	once      sync.Once
	serverURL string
	container string
	startErr  error
)

// Run runs the tests and removes the container; it returns the exit code.
func Run(m *testing.M) int {
	code := m.Run()
	if container != "" {
		_ = docker("rm", "-f", "-v", container).Run()
	}
	return code
}

func docker(args ...string) *exec.Cmd {
	cmd := exec.Command("docker", args...)
	cmd.Env = append(os.Environ(), "DOCKER_HOST="+runtime.Detect().Host)
	return cmd
}

func start() {
	if u := os.Getenv("DEVOPUS_TEST_DATABASE_URL"); u != "" {
		serverURL = u
		return
	}
	var out, errOut bytes.Buffer
	cmd := docker("run", "-d", "--rm", "-e", "POSTGRES_PASSWORD=test", "-p", "127.0.0.1::5432",
		"--label", "com.devopus.test=pgtest", Image, "-c", "fsync=off", "-c", "max_connections=300")
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		startErr = fmt.Errorf("start %s: %v: %s", Image, err, strings.TrimSpace(errOut.String()))
		return
	}
	container = strings.TrimSpace(out.String())
	port, err := docker("port", container, "5432/tcp").Output()
	if err != nil {
		startErr = fmt.Errorf("port of %s: %v", Image, err)
		return
	}
	addr := strings.TrimSpace(strings.SplitN(string(port), "\n", 2)[0])
	serverURL = "postgres://postgres:test@" + addr + "/postgres?sslmode=disable"

	// The server restarts once after initdb: wait until it answers queries for a while.
	deadline := time.Now().Add(60 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		conn, err := pgx.Connect(ctx, serverURL)
		if err == nil {
			err = conn.Ping(ctx)
			conn.Close(ctx)
		}
		cancel()
		if err == nil && logReady() {
			return
		}
		if time.Now().After(deadline) {
			startErr = fmt.Errorf("%s did not start: %v", Image, err)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// logReady reports whether the server finished its first-start restart.
func logReady() bool {
	out, _ := docker("logs", container).CombinedOutput()
	return bytes.Count(out, []byte("database system is ready to accept connections")) >= 2
}

// URL creates an empty database for the test and returns its connection URL. The
// database is dropped after the test.
func URL(t testing.TB) string {
	t.Helper()
	once.Do(start)
	if startErr != nil {
		t.Skipf("no PostgreSQL for the test: %v", startErr)
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "t_" + hex.EncodeToString(b)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, serverURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		conn, err := pgx.Connect(ctx, serverURL)
		if err != nil {
			return
		}
		defer conn.Close(ctx)
		_, _ = conn.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
	})
	u, err := url.Parse(serverURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

// Pool is a connection pool to a fresh database, closed after the test.
func Pool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), URL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
