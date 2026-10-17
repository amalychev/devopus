package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	authv1 "github.com/amalychev/devopus/gen/devopus/auth/v1"
	"github.com/amalychev/devopus/gen/devopus/auth/v1/authv1connect"
	"github.com/amalychev/devopus/internal/agent/link"
	"github.com/amalychev/devopus/internal/credentials"
)

// The account commands (ADR-0014, docs/architecture/auth.md): register, login, logout,
// whoami, auth sessions, auth revoke.

// noSignIn are the commands that work without signing in (specs/cli.md): the account
// commands, what the services run (they start before anyone signs in), and stopping
// or removing the components, so a machine can be cleaned up after signing out.
var noSignIn = map[string]bool{
	"devopus register": true, "devopus login": true, "devopus logout": true,
	"devopus version": true, "devopus help": true, "devopus completion": true,
	"devopus agent run": true, "devopus agent stop": true, "devopus agent uninstall": true,
	"devopus panel run": true, "devopus panel stop": true, "devopus panel uninstall": true,
	"devopus server run": true, "devopus server install": true, "devopus server status": true, "devopus server backup": true,
	"devopus server restore": true, "devopus server upgrade": true, "devopus server uninstall": true,
}

// requireSignIn refuses commands when nobody is signed in on this device. It checks
// the stored session, not Auth: commands that call Auth find out about a revoked
// session there, and so does the agent within minutes.
func requireSignIn(cmd *cobra.Command) error {
	path := cmd.CommandPath()
	for p := path; ; {
		if noSignIn[p] {
			return nil
		}
		i := strings.LastIndex(p, " ")
		if i < 0 {
			break
		}
		p = p[:i]
	}
	if cmd.Name() == cobra.ShellCompRequestCmd || cmd.Name() == cobra.ShellCompNoDescRequestCmd {
		return nil
	}
	// A server's agent is set up with a join code and works with its device token:
	// nobody signs in on a server (ADR-0019).
	if f := cmd.Flags().Lookup("server"); f != nil && f.Changed && strings.HasPrefix(path, "devopus agent ") {
		return nil
	}
	if c, err := link.LoadServerConfig(); err == nil && c != nil {
		return nil
	}
	s, err := credentials.Load()
	if err != nil {
		return fail(ExitError, "%v", err)
	}
	if s == nil {
		return fail(ExitAuth, "sign in first: 'devopus login', or 'devopus register' for a new account")
	}
	if !s.Live(time.Now()) {
		_ = credentials.Clear()
		return fail(ExitAuth, "the session of %s ended; sign in again: devopus login", s.Email)
	}
	return nil
}

func (a *app) registerCmd() *cobra.Command {
	var email, plan, authURL string
	cmd := &cobra.Command{
		Use:   "register",
		Short: "Create a Devopus account: email and a one-time code",
		Long: "Create a Devopus account with your email: Devopus emails you a one-time code, you\n" +
			"choose a plan, and this device is signed in. Registering an email that has an\n" +
			"account signs in.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c := credentials.Client(authURL)
			chosen, err := a.choosePlan(ctx, c, plan)
			if err != nil {
				return err
			}
			res, err := a.emailSignIn(ctx, c, email, authv1.Purpose_PURPOSE_REGISTER, chosen)
			if err != nil {
				return err
			}
			return a.signedIn(authURL, res.User, res.Tokens, res.Created)
		},
	}
	cmd.Flags().StringVar(&email, "email", "", "email address (asked when not given)")
	cmd.Flags().StringVar(&plan, "plan", "", "plan: free (default) or self-hosted (your own workspace server)")
	cmd.Flags().StringVar(&authURL, "auth-url", credentials.AuthURL(), "Auth service ($DEVOPUS_AUTH_URL)")
	return cmd
}

func (a *app) loginCmd() *cobra.Command {
	var useEmail, device, noOpen bool
	var authURL string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Sign in on this device: in the browser, or by email code (--email), or from another device (--device)",
		Long: `Sign in on this device. By default the browser opens the sign-in page and comes back
to devopus on 127.0.0.1. Without a browser:
  --email   email and a one-time code right in the terminal
  --device  a code to confirm on another device (a server over SSH)

The session lives while you use it: 30 days without use or 90 days in all, then sign
in again. The CLI and the agent on this device share it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c := credentials.Client(authURL)
			var (
				user   *authv1.User
				tokens *authv1.Tokens
			)
			switch {
			case useEmail && device:
				return fail(ExitInvalid, "choose one: --email or --device")
			case useEmail:
				res, err := a.emailSignIn(ctx, c, "", authv1.Purpose_PURPOSE_SIGN_IN, authv1.Plan_PLAN_UNSPECIFIED)
				if err != nil {
					return err
				}
				user, tokens = res.User, res.Tokens
			case device:
				res, err := a.deviceSignIn(ctx, c)
				if err != nil {
					return err
				}
				user, tokens = res.User, res.Tokens
			default:
				res, err := a.browserSignIn(ctx, c, authURL, noOpen)
				if err != nil {
					return err
				}
				user, tokens = res.User, res.Tokens
			}
			return a.signedIn(authURL, user, tokens, false)
		},
	}
	cmd.Flags().BoolVar(&useEmail, "email", false, "sign in with an email code in the terminal")
	cmd.Flags().BoolVar(&device, "device", false, "sign in by confirming a code on another device")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "print the sign-in link instead of opening the browser")
	cmd.Flags().StringVar(&authURL, "auth-url", credentials.AuthURL(), "Auth service ($DEVOPUS_AUTH_URL)")
	return cmd
}

// signedIn keeps a new session on this device, replacing the one before.
func (a *app) signedIn(authURL string, user *authv1.User, tokens *authv1.Tokens, created bool) error {
	if old, _ := credentials.Load(); old != nil {
		if rt, err := credentials.StoredRefreshToken(); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = credentials.Client(old.AuthURL).SignOut(ctx, connect.NewRequest(&authv1.SignOutRequest{RefreshToken: rt}))
			cancel()
		}
		_ = credentials.Clear()
	}
	s := credentials.FromTokens(authURL, user, tokens)
	if err := credentials.Save(s, tokens.RefreshToken); err != nil {
		return fail(ExitError, "keep the sign-in: %v", err)
	}
	what := "Signed in as"
	if created {
		what = "Account created; signed in as"
	}
	return a.done(map[string]any{"email": user.Email, "userId": user.Id, "sessionId": s.SessionID, "created": created},
		"✓ %s %s\n", what, user.Email)
}

// prompt asks for a line; without a terminal it reads stdin all the same, so scripts
// and tests can answer.
func (a *app) prompt(r *bufio.Reader, question string) (string, error) {
	fmt.Fprint(a.stderr, question)
	line, err := r.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" && err != nil {
		return "", fail(ExitInvalid, "no answer to %q", strings.TrimSpace(question))
	}
	return line, nil
}

func (a *app) choosePlan(ctx context.Context, c authv1connect.AuthServiceClient, want string) (authv1.Plan, error) {
	res, err := c.ListPlans(ctx, connect.NewRequest(&authv1.ListPlansRequest{}))
	if err != nil {
		return 0, rpcErr(err)
	}
	var availableNames, coming []string
	var only *authv1.PlanInfo
	for _, p := range res.Msg.Plans {
		if p.Available {
			availableNames = append(availableNames, strings.ToLower(p.Name))
			only = p
		} else {
			coming = append(coming, p.Name)
		}
	}
	if want != "" {
		for _, p := range res.Msg.Plans {
			if strings.EqualFold(p.Name, want) {
				if !p.Available {
					return 0, fail(ExitInvalid, "the %s plan is coming; available now: %s", p.Name, strings.Join(availableNames, ", "))
				}
				return p.Plan, nil
			}
		}
		return 0, fail(ExitInvalid, "unknown plan %q; available: %s", want, strings.Join(availableNames, ", "))
	}
	// The first available plan is the default: Free.
	for _, p := range res.Msg.Plans {
		if p.Available {
			only = p
			break
		}
	}
	if only == nil {
		return 0, fail(ExitError, "no plan is available now")
	}
	if !a.json {
		var notes []string
		if others := len(availableNames) - 1; others > 0 {
			notes = append(notes, "or --plan "+strings.Join(availableNames[1:], ", "))
		}
		if len(coming) > 0 {
			notes = append(notes, strings.Join(coming, " and ")+" coming")
		}
		note := ""
		if len(notes) > 0 {
			note = " (" + strings.Join(notes, "; ") + ")"
		}
		fmt.Fprintf(a.stderr, "Plan: %s%s\n", only.Name, note)
	}
	return only.Plan, nil
}

// emailSignIn asks for the email (unless given) and the code sent to it.
func (a *app) emailSignIn(ctx context.Context, c authv1connect.AuthServiceClient, email string, purpose authv1.Purpose, plan authv1.Plan) (*authv1.CompleteEmailSignInResponse, error) {
	in := bufio.NewReader(a.stdin)
	var err error
	if email == "" {
		if email, err = a.prompt(in, "Email: "); err != nil {
			return nil, err
		}
	}
	st, err := c.StartEmailSignIn(ctx, connect.NewRequest(&authv1.StartEmailSignInRequest{Email: email, Purpose: purpose, DeviceId: credentials.DeviceID()}))
	if err != nil {
		return nil, rpcErr(err)
	}
	minutes := int(time.Until(st.Msg.ExpiresAt.AsTime()).Round(time.Minute).Minutes())
	question := fmt.Sprintf("Code sent to %s (valid %d minutes): ", strings.ToLower(strings.TrimSpace(email)), minutes)
	for {
		code, err := a.prompt(in, question)
		if err != nil {
			return nil, err
		}
		res, err := c.CompleteEmailSignIn(ctx, connect.NewRequest(&authv1.CompleteEmailSignInRequest{
			ChallengeId: st.Msg.ChallengeId, Code: code, Plan: plan, DeviceName: credentials.DeviceName(),
		}))
		if connect.CodeOf(err) == connect.CodeInvalidArgument && strings.Contains(err.Error(), "code is wrong") {
			fmt.Fprintln(a.stderr, "The code is wrong; try again.")
			continue
		}
		if err != nil {
			return nil, rpcErr(err)
		}
		return res.Msg, nil
	}
}

// browserSignIn is OAuth 2.0 with PKCE: the browser signs in on Auth's page and
// brings the code back to a listener on 127.0.0.1.
func (a *app) browserSignIn(ctx context.Context, c authv1connect.AuthServiceClient, authURL string, noOpen bool) (*authv1.ExchangeAuthorizationCodeResponse, error) {
	verifier, state := random(32), random(16)
	sum := sha256.Sum256([]byte(verifier))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fail(ExitError, "listen for the browser: %v", err)
	}
	defer ln.Close()
	redirect := fmt.Sprintf("http://%s/callback", ln.Addr())

	type result struct {
		code string
		err  error
	}
	got := make(chan result, 1)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		var res result
		switch {
		case q.Get("state") != state:
			res.err = fail(ExitError, "the browser came back with a wrong state; run 'devopus login' again")
		case q.Get("error") != "":
			res.err = fail(ExitError, "sign-in failed: %s", q.Get("error"))
		default:
			res.code = q.Get("code")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		msg := "You are signed in. Go back to the terminal."
		if res.err != nil {
			msg = "Sign-in failed: " + res.err.Error()
		}
		fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>Devopus</title><body style="font:16px system-ui;display:grid;place-items:center;min-height:90vh"><p>%s</p>`, html.EscapeString(msg))
		select {
		case got <- res:
		default:
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	u := authURL + "/authorize?" + url.Values{
		"client_id": {"devopus-cli"}, "response_type": {"code"}, "redirect_uri": {redirect}, "state": {state},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
	}.Encode()
	if noOpen || a.openURL(u) != nil {
		fmt.Fprintf(a.stderr, "Open this link in a browser on this machine to sign in:\n  %s\n", u)
		fmt.Fprintln(a.stderr, "No browser here? Use 'devopus login --email' or 'devopus login --device'.")
	} else {
		fmt.Fprintf(a.stderr, "Opened the browser to sign in. If it did not open, visit:\n  %s\n", u)
	}
	fmt.Fprintln(a.stderr, "Waiting for the browser…")

	wait, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	var res result
	select {
	case res = <-got:
	case <-wait.Done():
		return nil, fail(ExitError, "the browser did not come back in 10 minutes; run 'devopus login' again")
	}
	if res.err != nil {
		return nil, res.err
	}
	out, err := c.ExchangeAuthorizationCode(ctx, connect.NewRequest(&authv1.ExchangeAuthorizationCodeRequest{
		Code: res.code, CodeVerifier: verifier, RedirectUri: redirect, DeviceName: credentials.DeviceName(),
	}))
	if err != nil {
		return nil, rpcErr(err)
	}
	return out.Msg, nil
}

// deviceSignIn is the device flow: a code to confirm on another device.
func (a *app) deviceSignIn(ctx context.Context, c authv1connect.AuthServiceClient) (*authv1.PollDeviceSignInResponse, error) {
	st, err := c.StartDeviceSignIn(ctx, connect.NewRequest(&authv1.StartDeviceSignInRequest{DeviceName: credentials.DeviceName(), DeviceId: credentials.DeviceID()}))
	if err != nil {
		return nil, rpcErr(err)
	}
	m := st.Msg
	fmt.Fprintf(a.stderr, "On another device, open %s\nand enter the code %s\n  (or open %s)\nWaiting for the confirmation…\n", m.VerificationUri, m.UserCode, m.VerificationUriComplete)
	interval := time.Duration(m.IntervalSeconds) * time.Second
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
		res, err := c.PollDeviceSignIn(ctx, connect.NewRequest(&authv1.PollDeviceSignInRequest{DeviceCode: m.DeviceCode}))
		if err != nil {
			return nil, rpcErr(err)
		}
		switch res.Msg.Status {
		case authv1.DeviceSignInStatus_DEVICE_SIGN_IN_STATUS_DONE:
			return res.Msg, nil
		case authv1.DeviceSignInStatus_DEVICE_SIGN_IN_STATUS_SLOW_DOWN:
			interval += 5 * time.Second
		}
	}
}

func (a *app) logoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Sign out on this device: the session ends, the agent stops acting for you",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := credentials.Load()
			if err != nil {
				return fail(ExitError, "%v", err)
			}
			if s == nil {
				return a.done(map[string]any{"signedIn": false}, "Not signed in.\n")
			}
			if rt, err := credentials.StoredRefreshToken(); err == nil {
				ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
				_, err = credentials.Client(s.AuthURL).SignOut(ctx, connect.NewRequest(&authv1.SignOutRequest{RefreshToken: rt}))
				cancel()
				if err != nil {
					fmt.Fprintf(a.stderr, "Could not reach %s to end the session (%v); it ends on its own in 30 days without use, or revoke it from another device: devopus auth revoke %s\n", s.AuthURL, connect.CodeOf(err), s.SessionID)
				}
			}
			if err := credentials.Clear(); err != nil {
				return fail(ExitError, "%v", err)
			}
			return a.done(map[string]any{"signedIn": false, "email": s.Email}, "Signed out %s on this device.\n", s.Email)
		},
	}
}

// access refreshes the sign-in for a call to Auth.
func (a *app) access(ctx context.Context) (credentials.Access, authv1connect.AuthServiceClient, error) {
	acc, err := credentials.Refresh(ctx)
	if errors.Is(err, credentials.ErrSignedOut) {
		return acc, nil, fail(ExitAuth, "%v", err)
	}
	if err != nil {
		return acc, nil, fail(ExitError, "%v", err)
	}
	return acc, credentials.Client(acc.Session.AuthURL), nil
}

func bearer[T any](msg *T, acc credentials.Access) *connect.Request[T] {
	r := connect.NewRequest(msg)
	r.Header().Set("Authorization", "Bearer "+acc.Token)
	return r
}

func (a *app) whoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Who is signed in on this device",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			acc, c, err := a.access(cmd.Context())
			if err != nil {
				return err
			}
			me, err := c.GetMe(cmd.Context(), bearer(&authv1.GetMeRequest{}, acc))
			if err != nil {
				return rpcErr(err)
			}
			if a.json {
				return a.writeJSON(map[string]any{
					"email": me.Msg.User.Email, "userId": me.Msg.User.Id, "plan": planLabel(me.Msg.User.Plan),
					"sessionId": me.Msg.Session.Id, "device": me.Msg.Session.DeviceName,
					"sessionExpiresAt": me.Msg.Session.ExpiresAt.AsTime(), "authUrl": acc.Session.AuthURL,
				})
			}
			w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "Email\t%s\n", me.Msg.User.Email)
			fmt.Fprintf(w, "Plan\t%s\n", planLabel(me.Msg.User.Plan))
			fmt.Fprintf(w, "Session\t%s on %s, until %s at the latest\n", me.Msg.Session.Id, me.Msg.Session.DeviceName, me.Msg.Session.ExpiresAt.AsTime().Local().Format("2 Jan 2006"))
			fmt.Fprintf(w, "Auth\t%s\n", acc.Session.AuthURL)
			return w.Flush()
		},
	}
}

func planLabel(p authv1.Plan) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(p.String(), "PLAN_")), "_", "-")
}

func (a *app) authCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Your sessions on devices: list and revoke",
	}
	sessions := &cobra.Command{
		Use:   "sessions",
		Short: "List your sessions on all devices",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			acc, c, err := a.access(cmd.Context())
			if err != nil {
				return err
			}
			res, err := c.ListSessions(cmd.Context(), bearer(&authv1.ListSessionsRequest{}, acc))
			if err != nil {
				return rpcErr(err)
			}
			if a.json {
				var out []map[string]any
				for _, s := range res.Msg.Sessions {
					out = append(out, map[string]any{
						"id": s.Id, "device": s.DeviceName, "current": s.Current, "createdAt": s.CreatedAt.AsTime(),
						"lastUsedAt": s.LastUsedAt.AsTime(), "idleExpiresAt": s.IdleExpiresAt.AsTime(), "expiresAt": s.ExpiresAt.AsTime(),
					})
				}
				return a.writeJSON(out)
			}
			w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tDEVICE\tSIGNED IN\tLAST USED\tENDS")
			for _, s := range res.Msg.Sessions {
				id := s.Id
				if s.Current {
					id += " (this device)"
				}
				ends := s.IdleExpiresAt.AsTime()
				if s.ExpiresAt.AsTime().Before(ends) {
					ends = s.ExpiresAt.AsTime()
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", id, s.DeviceName, day(s.CreatedAt.AsTime()), ago(time.Since(s.LastUsedAt.AsTime())), day(ends))
			}
			return w.Flush()
		},
	}
	revoke := &cobra.Command{
		Use:   "revoke <session-id>",
		Short: "End a session: that device has to sign in again",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			acc, c, err := a.access(cmd.Context())
			if err != nil {
				return err
			}
			if _, err := c.RevokeSession(cmd.Context(), bearer(&authv1.RevokeSessionRequest{SessionId: args[0]}, acc)); err != nil {
				return rpcErr(err)
			}
			if args[0] == acc.Session.SessionID {
				_ = credentials.Clear()
				return a.done(map[string]any{"revoked": args[0], "signedIn": false}, "Session %s revoked; this device is signed out.\n", args[0])
			}
			return a.done(map[string]any{"revoked": args[0]}, "Session %s revoked.\n", args[0])
		},
	}
	cmd.AddCommand(sessions, revoke)
	return cmd
}

func day(t time.Time) string { return t.Local().Format("2 Jan 2006") }

// rpcErr turns an error of Auth or the workspace server into a CLI failure with its
// message. A missing permission exits with 5, like a missing sign-in.
func rpcErr(err error) error {
	var ee *exitError
	if errors.As(err, &ee) {
		return err
	}
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return fail(ExitError, "%v", err)
	}
	switch ce.Code() {
	case connect.CodeUnauthenticated, connect.CodePermissionDenied:
		return fail(ExitAuth, "%s", ce.Message())
	case connect.CodeInvalidArgument, connect.CodeNotFound, connect.CodeFailedPrecondition, connect.CodeAlreadyExists:
		return fail(ExitInvalid, "%s", ce.Message())
	case connect.CodeUnavailable:
		return fail(ExitError, "not reachable (%s); check the network, or where DEVOPUS_AUTH_URL and DEVOPUS_SERVER_URL point", ce.Message())
	}
	return fail(ExitError, "%s", ce.Message())
}

func random(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
