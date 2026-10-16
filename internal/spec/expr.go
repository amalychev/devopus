package spec

import (
	"fmt"
	"regexp"
	"strings"
)

// exprRe matches ${{ a.b }}.
var exprRe = regexp.MustCompile(`\$\{\{\s*([^{}]*?)\s*\}\}`)

type RefKind int

const (
	RefSecret  RefKind = iota // secrets.NAME
	RefEnv                    // env.url | env.slug | env.id
	RefApp                    // <app>.url | .host | .port
	RefService                // <service>.host | .port | .database
)

// Ref is a parsed ${{ … }} expression.
type Ref struct {
	Kind RefKind
	Name string // secret, application or service; empty for env
	Attr string
	Raw  string // the whole ${{ … }}
}

var (
	secretNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	envAttrs     = map[string]bool{"url": true, "slug": true, "id": true}
	appAttrs     = map[string]bool{"url": true, "host": true, "port": true}
	serviceAttrs = map[string]bool{"host": true, "port": true, "database": true}
)

// ParseRef parses the body of an expression against the names of the project.
func (p *Project) ParseRef(body string) (Ref, error) {
	head, attr, ok := strings.Cut(body, ".")
	if !ok || attr == "" || strings.Contains(attr, ".") {
		return Ref{}, fmt.Errorf("expected <name>.<attribute>, got %q", body)
	}
	switch head {
	case "secrets":
		if !secretNameRe.MatchString(attr) {
			return Ref{}, fmt.Errorf("secret name %q must match %s", attr, secretNameRe)
		}
		return Ref{Kind: RefSecret, Name: attr}, nil
	case "env":
		if !envAttrs[attr] {
			return Ref{}, fmt.Errorf("env.%s: expected env.url, env.slug or env.id", attr)
		}
		return Ref{Kind: RefEnv, Attr: attr}, nil
	}
	if _, ok := p.Apps[head]; ok {
		if !appAttrs[attr] {
			return Ref{}, fmt.Errorf("%s.%s: an application has url, host and port", head, attr)
		}
		return Ref{Kind: RefApp, Name: head, Attr: attr}, nil
	}
	if s, ok := p.Services[head]; ok {
		if !s.IsContainer() {
			return Ref{}, fmt.Errorf("%s.%s: %s is a %s, not something to connect to", head, attr, head, s.Type)
		}
		if !serviceAttrs[attr] {
			return Ref{}, fmt.Errorf("%s.%s: a service has host, port and database", head, attr)
		}
		return Ref{Kind: RefService, Name: head, Attr: attr}, nil
	}
	return Ref{}, fmt.Errorf("%q is neither secrets, env, an application nor a service", head)
}

// Expand replaces every expression in s with the value the resolver gives.
func (p *Project) Expand(s string, resolve func(Ref) (string, error)) (string, error) {
	var firstErr error
	out := exprRe.ReplaceAllStringFunc(s, func(raw string) string {
		body := exprRe.FindStringSubmatch(raw)[1]
		r, err := p.ParseRef(body)
		if err == nil {
			r.Raw = raw
			var v string
			if v, err = resolve(r); err == nil {
				return v
			}
		}
		if firstErr == nil {
			firstErr = err
		}
		return raw
	})
	return out, firstErr
}

// HasExpr reports whether s contains an expression.
func HasExpr(s string) bool { return strings.Contains(s, "${{") }
