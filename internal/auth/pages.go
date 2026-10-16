package auth

import (
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net/http"
	"net/url"

	"connectrpc.com/connect"

	authv1 "github.com/amalychev/devopus/gen/devopus/auth/v1"
	"github.com/amalychev/devopus/internal/auth/store"
)

// The sign-in pages: the browser sign-in the CLI opens (/authorize) and the page where
// a device sign-in is confirmed (/device). Both ask for an email and the code sent to
// it; there are no cookies, every step carries its state in the form.

type page struct {
	Title    string
	Action   string
	Step     string // user_code, email, code, done
	Error    string
	Hidden   map[string]string
	Email    string
	UserCode string
	Message  string
}

var pageTemplate = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} · Devopus</title>
<style>
:root{color-scheme:light dark;--bg:#f6f7f9;--card:#fff;--fg:#14161a;--muted:#5d6470;--line:#d9dde3;--accent:#3355ff;--err:#c62828}
@media (prefers-color-scheme:dark){:root{--bg:#0f1115;--card:#171a20;--fg:#e8eaee;--muted:#9aa1ad;--line:#2a2f38;--accent:#7c93ff;--err:#ff6b6b}}
body{margin:0;min-height:100vh;display:grid;place-items:center;background:var(--bg);color:var(--fg);font:16px/1.5 system-ui,sans-serif}
main{width:min(380px,calc(100vw - 32px));background:var(--card);border:1px solid var(--line);border-radius:12px;padding:28px}
h1{font-size:20px;margin:0 0 6px}p{color:var(--muted);margin:0 0 18px}
label{display:block;font-size:14px;margin-bottom:6px}
input{box-sizing:border-box;width:100%;padding:10px 12px;font:inherit;border:1px solid var(--line);border-radius:8px;background:transparent;color:inherit}
input.code{letter-spacing:.3em;font-family:ui-monospace,monospace;font-size:20px}
button{margin-top:16px;width:100%;padding:10px;font:inherit;font-weight:600;border:0;border-radius:8px;background:var(--accent);color:#fff;cursor:pointer}
.err{color:var(--err);margin:0 0 14px}
</style></head><body><main>
<h1>{{.Title}}</h1>
{{if .Error}}<p class="err" role="alert">{{.Error}}</p>{{end}}
{{if eq .Step "done"}}<p>{{.Message}}</p>{{else}}
<form method="post" action="{{.Action}}">
{{range $k, $v := .Hidden}}<input type="hidden" name="{{$k}}" value="{{$v}}">{{end}}
<input type="hidden" name="step" value="{{.Step}}">
{{if eq .Step "user_code"}}<p>Enter the code shown in the terminal.</p>
<label for="user_code">Code</label><input id="user_code" class="code" name="user_code" value="{{.UserCode}}" autocomplete="off" autofocus required>
<button>Continue</button>
{{else if eq .Step "email"}}<p>We will email you a one-time code.</p>
<label for="email">Email</label><input id="email" name="email" type="email" value="{{.Email}}" autocomplete="email" autofocus required>
<button>Send code</button>
{{else if eq .Step "code"}}<p>Enter the code sent to {{.Email}}. It works for 10 minutes.</p>
<label for="code">Code</label><input id="code" class="code" name="code" inputmode="numeric" autocomplete="one-time-code" maxlength="7" autofocus required>
<button>Sign in</button>
{{end}}</form>{{end}}
</main></body></html>
`))

func render(w http.ResponseWriter, status int, p page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = pageTemplate.Execute(w, p)
}

// message is what a person reads for an error: the message of a connect error.
func message(err error) string {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Message()
	}
	return err.Error()
}

func (s *Service) authorizePage(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		render(w, http.StatusBadRequest, page{Title: "Sign-in failed", Step: "done", Message: err.Error()})
		return
	}
	ar := authorizeRequestFrom(r.Form)
	if err := ar.check(); err != nil {
		// Never redirect for a bad request: the redirect URI itself may be the problem.
		render(w, http.StatusBadRequest, page{Title: "Sign-in failed", Step: "done", Message: "This sign-in link is broken: " + err.Error() + ". Run 'devopus login' again."})
		return
	}
	hidden := map[string]string{
		"client_id": ar.ClientID, "redirect_uri": ar.RedirectURI, "state": ar.State,
		"code_challenge": ar.CodeChallenge, "code_challenge_method": ar.Method, "response_type": ar.ResponseType,
	}
	p := page{Title: "Sign in to Devopus", Action: "/authorize", Step: "email", Hidden: hidden}
	if r.Method == http.MethodGet {
		render(w, http.StatusOK, p)
		return
	}
	user, ok := s.emailSteps(r, &p)
	if !ok {
		render(w, http.StatusOK, p)
		return
	}
	code, err := s.authorizationCode(r.Context(), user, ar)
	if err != nil {
		s.cfg.Log.Error("authorization code", "err", err)
		render(w, http.StatusInternalServerError, page{Title: "Sign-in failed", Step: "done", Message: "Something went wrong; try again."})
		return
	}
	u, _ := url.Parse(ar.RedirectURI)
	q := u.Query()
	q.Set("code", code)
	q.Set("state", ar.State)
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

func (s *Service) devicePage(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		render(w, http.StatusBadRequest, page{Title: "Sign-in failed", Step: "done", Message: err.Error()})
		return
	}
	uc := normalizeUserCode(r.Form.Get("user_code"))
	p := page{Title: "Sign in a device", Action: "/device", Step: "user_code", UserCode: uc}
	if r.Method == http.MethodGet {
		render(w, http.StatusOK, p)
		return
	}
	if _, err := s.pendingDevice(r.Context(), uc); err != nil {
		p.Error = message(err)
		render(w, http.StatusOK, p)
		return
	}
	p.Hidden = map[string]string{"user_code": uc}
	if r.Form.Get("step") == "user_code" {
		p.Step = "email"
		render(w, http.StatusOK, p)
		return
	}
	p.Step = "email"
	user, ok := s.emailSteps(r, &p)
	if !ok {
		render(w, http.StatusOK, p)
		return
	}
	if err := s.approveDevice(r.Context(), uc, user); err != nil {
		p.Step, p.Error = "user_code", message(err)
		render(w, http.StatusOK, p)
		return
	}
	render(w, http.StatusOK, page{Title: "Device signed in", Step: "done",
		Message: "You are signed in as " + user.Email + ". Go back to the terminal: it continues on its own."})
}

// emailSteps runs the email and code steps of a form. It returns the user once the
// code is right; otherwise it sets the page's next step or error.
func (s *Service) emailSteps(r *http.Request, p *page) (user store.User, ok bool) {
	ctx := r.Context()
	ip := s.clientIP(r.RemoteAddr, r.Header)
	switch r.Form.Get("step") {
	case "email":
		p.Email = r.Form.Get("email")
		id, _, err := s.startEmail(ctx, p.Email, authv1.Purpose_PURPOSE_SIGN_IN, ip, "")
		if err != nil {
			p.Error = message(err)
			return user, false
		}
		p.Step = "code"
		p.Hidden = with(p.Hidden, "challenge_id", id, "email", p.Email)
		return user, false
	case "code":
		p.Step, p.Email = "code", r.Form.Get("email")
		p.Hidden = with(p.Hidden, "challenge_id", r.Form.Get("challenge_id"), "email", p.Email)
		ch, err := s.verifyCode(ctx, r.Form.Get("challenge_id"), r.Form.Get("code"))
		if err != nil {
			p.Error = message(err)
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				p.Step = "email" // the challenge is spent: start over
			}
			return user, false
		}
		u, err := s.q.UserByEmail(ctx, ch.Email)
		if noRows(err) {
			p.Step, p.Error = "email", "There is no Devopus account for "+ch.Email+". Create one in the terminal: devopus register"
			return user, false
		}
		if err != nil {
			p.Error = "Something went wrong; try again."
			return user, false
		}
		return u, true
	}
	return user, false
}

func with(m map[string]string, kv ...string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

func jsonEncode(w io.Writer, v any) error { return json.NewEncoder(w).Encode(v) }
