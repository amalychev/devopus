// Package authtest runs a real Auth service for tests of its clients: the CLI, the
// agent, later the workspace server. It needs PostgreSQL from pgtest; the codes Auth
// emails land in an Inbox.
package authtest

import (
	"context"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/amalychev/devopus/internal/auth"
	"github.com/amalychev/devopus/internal/auth/mail"
	"github.com/amalychev/devopus/internal/auth/store"
	"github.com/amalychev/devopus/internal/pgtest"
)

// Inbox keeps the emails Auth sends.
type Inbox struct {
	mu   sync.Mutex
	msgs []mail.Message
}

func (b *Inbox) Send(_ context.Context, m mail.Message) error {
	b.mu.Lock()
	b.msgs = append(b.msgs, m)
	b.mu.Unlock()
	return nil
}

var codeRe = regexp.MustCompile(`\b\d{6}\b`)

// Code waits for the n-th email (from 1) to an address and returns its code.
func (b *Inbox) Code(t testing.TB, to string, n int) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		b.mu.Lock()
		seen := 0
		for _, m := range b.msgs {
			if m.To == to {
				if seen++; seen == n {
					b.mu.Unlock()
					return codeRe.FindString(m.Text)
				}
			}
		}
		b.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("no email #%d to %s", n, to)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Count is how many emails went to an address.
func (b *Inbox) Count(to string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, m := range b.msgs {
		if m.To == to {
			n++
		}
	}
	return n
}

// Server is Auth on a test HTTP server.
type Server struct {
	URL     string
	Service *auth.Service
	Inbox   *Inbox
}

// Start runs Auth on a fresh database for the test; it skips without PostgreSQL.
func Start(t testing.TB) *Server {
	t.Helper()
	pool := pgtest.Pool(t)
	ctx := context.Background()
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(nil)
	srv.Start()
	t.Cleanup(srv.Close)
	inbox := &Inbox{}
	svc, err := auth.New(ctx, pool, auth.Config{Issuer: srv.URL, Mailer: inbox})
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = svc.Handler()
	return &Server{URL: srv.URL, Service: svc, Inbox: inbox}
}
