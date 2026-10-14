package workspace

import (
	"fmt"
	"regexp"
	"strings"
)

// exprRe matches ${{ a.b }}; the body is checked by ParseRef.
var exprRe = regexp.MustCompile(`\$\{\{\s*([^{}]*?)\s*\}\}`)

type RefKind int

const (
	RefSecret  RefKind = iota // secrets.NAME
	RefEnv                    // env.url | env.slug | env.id
	RefModule                 // <module>.url | .host | .port
	RefService                // <service>.host | .port | .database
)

// Ref is a parsed ${{ … }} expression.
type Ref struct {
	Kind RefKind
	Name string // secret, module or service name; empty for env
	Attr string
	Raw  string // the whole ${{ … }}
}

var (
	secretNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	envAttrs     = map[string]bool{"url": true, "slug": true, "id": true}
	moduleAttrs  = map[string]bool{"url": true, "host": true, "port": true}
	serviceAttrs = map[string]bool{"host": true, "port": true, "database": true}
)

// ParseRef parses the body of an expression against the names of the workspace.
func (w *Workspace) ParseRef(body string) (Ref, error) {
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
	if _, ok := w.Modules[head]; ok {
		if !moduleAttrs[attr] {
			return Ref{}, fmt.Errorf("%s.%s: a module has url, host and port", head, attr)
		}
		return Ref{Kind: RefModule, Name: head, Attr: attr}, nil
	}
	if _, ok := w.Services[head]; ok {
		if !serviceAttrs[attr] {
			return Ref{}, fmt.Errorf("%s.%s: a service has host, port and database", head, attr)
		}
		return Ref{Kind: RefService, Name: head, Attr: attr}, nil
	}
	return Ref{}, fmt.Errorf("%q is neither secrets, env, a module nor a service", head)
}

// Refs returns the expressions in s, or the first malformed one.
func (w *Workspace) Refs(s string) ([]Ref, error) {
	var refs []Ref
	for _, m := range exprRe.FindAllStringSubmatch(s, -1) {
		r, err := w.ParseRef(m[1])
		if err != nil {
			return refs, err
		}
		r.Raw = m[0]
		refs = append(refs, r)
	}
	if rest := exprRe.ReplaceAllString(s, ""); strings.Contains(rest, "${{") {
		return refs, fmt.Errorf("unterminated ${{ expression")
	}
	return refs, nil
}

// Expand replaces every expression in s with the value the resolver gives.
func (w *Workspace) Expand(s string, resolve func(Ref) (string, error)) (string, error) {
	var firstErr error
	out := exprRe.ReplaceAllStringFunc(s, func(raw string) string {
		body := exprRe.FindStringSubmatch(raw)[1]
		r, err := w.ParseRef(body)
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
