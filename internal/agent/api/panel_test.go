package api

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	agentv1 "github.com/amalychev/devopus/gen/devopus/agent/v1"
)

func collect(ch chan string) func(*agentv1.LogLine) error {
	return func(l *agentv1.LogLine) error {
		ch <- l.Text
		return nil
	}
}

func next(t *testing.T, ch chan string) string {
	t.Helper()
	select {
	case s := <-ch:
		return s
	case <-time.After(3 * time.Second):
		t.Fatal("no line")
		return ""
	}
}

func TestTailFileLastLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api-install.log")
	if err := os.WriteFile(path, []byte("a\nb\nc\nd\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var got []string
	err := tailFile(context.Background(), path, 2, false, func(l *agentv1.LogLine) error {
		got = append(got, l.Text)
		return nil
	})
	if err != nil || !slices.Equal(got, []string{"c", "d"}) {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestTailFileFollows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api-migrate.log")
	if err := os.WriteFile(path, []byte("one\ntw"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan string, 16)
	done := make(chan error, 1)
	go func() { done <- tailFile(ctx, path, 0, true, collect(ch)) }()
	if s := next(t, ch); s != "one" {
		t.Fatalf("first line %q", s)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("o\nthree\n")
	f.Close()
	// The half-written line comes whole.
	if a, b := next(t, ch), next(t, ch); a != "two" || b != "three" {
		t.Fatalf("appended %q %q", a, b)
	}
	// The agent recreates the log when the hook runs again.
	if err := os.WriteFile(path, []byte("fresh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s := next(t, ch); s != "--- the log started over ---" {
		t.Fatalf("restart marker %q", s)
	}
	if s := next(t, ch); s != "fresh" {
		t.Fatalf("after restart %q", s)
	}
	// Written over with more than there was: the top of the file differs.
	if err := os.WriteFile(path, []byte("again, and this time a much longer first line\nsecond\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s := next(t, ch); s != "--- the log started over ---" {
		t.Fatalf("second restart marker %q", s)
	}
	if s := next(t, ch); s != "again, and this time a much longer first line" {
		t.Fatalf("after second restart %q", s)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
