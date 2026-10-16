package credentials

import (
	"context"
	"errors"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	authv1 "github.com/amalychev/devopus/gen/devopus/auth/v1"
	"github.com/amalychev/devopus/internal/agent/paths"
	"github.com/amalychev/devopus/internal/auth/authtest"
	"github.com/amalychev/devopus/internal/pgtest"
)

func TestMain(m *testing.M) { os.Exit(pgtest.Run(m)) }

// signIn registers an account on Auth and keeps the session on this "device".
func signIn(t *testing.T, a *authtest.Server, email string) {
	t.Helper()
	ctx := context.Background()
	c := Client(a.URL)
	st, err := c.StartEmailSignIn(ctx, connect.NewRequest(&authv1.StartEmailSignInRequest{Email: email, Purpose: authv1.Purpose_PURPOSE_REGISTER}))
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.CompleteEmailSignIn(ctx, connect.NewRequest(&authv1.CompleteEmailSignInRequest{
		ChallengeId: st.Msg.ChallengeId, Code: a.Inbox.Code(t, email, a.Inbox.Count(email)), Plan: authv1.Plan_PLAN_FREE, DeviceName: "test",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(FromTokens(a.URL, res.Msg.User, res.Msg.Tokens), res.Msg.Tokens.RefreshToken); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentRefreshesShareTheSession(t *testing.T) {
	a := authtest.Start(t)
	t.Setenv("DEVOPUS_HOME", t.TempDir())
	t.Setenv("DEVOPUS_CREDENTIAL_STORE", StoreFile)
	signIn(t, a, "dev@acme.dev")

	// The CLI and the agent refresh at once: under the lock each one presents the
	// token the one before saved, so Auth sees no reuse.
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := Refresh(context.Background())
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("refresh: %v", err)
		}
	}

	var k Keeper
	tok, err := k.Token(context.Background())
	if err != nil || tok == "" {
		t.Fatalf("keeper: %q %v", tok, err)
	}
	// The token in memory is reused while fresh.
	if again, _ := k.Token(context.Background()); again != tok {
		t.Errorf("keeper refreshed a fresh token")
	}
	if s := k.Session(); s == nil || s.Email != "dev@acme.dev" {
		t.Errorf("keeper session = %+v", s)
	}

	// A sign-out on the device: the keeper lets go of the session.
	if err := Clear(); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Token(context.Background()); !errors.Is(err, ErrSignedOut) {
		t.Errorf("after sign-out: %v", err)
	}
	if k.Session() != nil {
		t.Errorf("keeper still has a session")
	}
	if _, err := os.Stat(tokenFile()); !os.IsNotExist(err) {
		t.Errorf("refresh token file left: %v", err)
	}
}

func TestRevokedSessionIsForgotten(t *testing.T) {
	a := authtest.Start(t)
	t.Setenv("DEVOPUS_HOME", t.TempDir())
	t.Setenv("DEVOPUS_CREDENTIAL_STORE", StoreFile)
	signIn(t, a, "dev@acme.dev")
	rt, err := StoredRefreshToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Client(a.URL).SignOut(context.Background(), connect.NewRequest(&authv1.SignOutRequest{RefreshToken: rt})); err != nil {
		t.Fatal(err)
	}
	if _, err := Refresh(context.Background()); !errors.Is(err, ErrSignedOut) {
		t.Fatalf("refresh of a revoked session: %v", err)
	}
	if s, _ := Load(); s != nil {
		t.Errorf("the session is still on the device: %+v", s)
	}
}

func TestSessionLive(t *testing.T) {
	now := time.Now()
	s := Session{IdleExpiresAt: now.Add(time.Hour), ExpiresAt: now.Add(2 * time.Hour)}
	if !s.Live(now) || s.Live(now.Add(time.Hour)) {
		t.Error("idle expiry")
	}
	s.IdleExpiresAt = now.Add(3 * time.Hour)
	if s.Live(now.Add(2 * time.Hour)) {
		t.Error("absolute expiry")
	}
}

// TestKeychain writes to the login keychain: run with DEVOPUS_TEST_KEYCHAIN=1 on macOS.
func TestKeychain(t *testing.T) {
	if runtime.GOOS != "darwin" || os.Getenv("DEVOPUS_TEST_KEYCHAIN") != "1" {
		t.Skip("set DEVOPUS_TEST_KEYCHAIN=1 on macOS")
	}
	t.Setenv("DEVOPUS_HOME", t.TempDir())
	k := keychain{}
	acct := paths.Home()
	t.Cleanup(func() { _ = k.del(acct) })
	if _, err := k.get(acct); !errors.Is(err, errNoSecret) {
		t.Fatalf("empty get: %v", err)
	}
	for _, v := range []string{"dvr_first", "dvr_second"} {
		if err := k.set(acct, v); err != nil {
			t.Fatal(err)
		}
		if got, err := k.get(acct); err != nil || got != v {
			t.Fatalf("get = %q %v", got, err)
		}
	}
	if err := k.del(acct); err != nil {
		t.Fatal(err)
	}
	if err := k.del(acct); err != nil {
		t.Errorf("second delete: %v", err)
	}
}
