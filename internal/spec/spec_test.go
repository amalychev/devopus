package spec

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	p := &Project{Name: "p", Domain: "p.localhost",
		Apps: map[string]*App{"api": {Uses: []string{"nope"}}, "web": {}},
		Services: map[string]*Service{
			"php":   {Type: "php", Version: "8.3", Apps: []string{"api"}},
			"node":  {Type: "nodejs", Version: "22", Apps: []string{"api"}},
			"q":     {Type: "queue", Apps: []string{"api", "web"}},
			"x":     {Type: "kafka"},
			"cache": {Type: "redis", Apps: []string{"ghost"}},
			"web":   {Type: "redis"},
		}}
	err := p.Validate()
	if err == nil {
		t.Fatal("invalid project passed")
	}
	for _, want := range []string{
		"apps.api: one runtime per application", "apps.api: uses unknown application \"nope\"",
		"services.q: a queue runs the code of exactly one application", "services.q: a queue needs a command",
		"services.x: unknown type \"kafka\"", "services.cache: attached to unknown application \"ghost\"",
		"services.web: an application has the same name",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}

func TestRefs(t *testing.T) {
	p := &Project{Name: "shop-app", Apps: map[string]*App{"api": {Port: 9000}},
		Services: map[string]*Service{"db": {Type: "postgres"}, "php": {Type: "php", Apps: []string{"api"}}}}
	out, err := p.Expand("${{ api.port }} ${{ db.host }}", func(r Ref) (string, error) { return r.Name + "." + r.Attr, nil })
	if err != nil || out != "api.port db.host" {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := p.Expand("${{ php.host }}", func(Ref) (string, error) { return "", nil }); err == nil {
		t.Error("a runtime is not something to connect to")
	}
	if p.DatabaseName("db") != "shop_app" {
		t.Errorf("database %q", p.DatabaseName("db"))
	}
}
