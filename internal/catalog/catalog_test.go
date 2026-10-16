package catalog

import "testing"

func TestFromImage(t *testing.T) {
	for image, want := range map[string][2]string{
		"postgres:17.6":                     {"postgres", "17.6"},
		"docker.io/library/mysql:8.4":       {"mysql", "8.4"},
		"axllent/mailpit:v1.27":             {"mailpit", "v1.27"},
		"redis:7@sha256:0123":               {"redis", "7"},
		"typesense/typesense:30.2":          {"typesense", "30.2"},
		"ghcr.io/acme/thing:1":              {Custom, "1"},
		"localhost:5000/postgres":           {Custom, ""},
		"mcr.microsoft.com/mssql/server:22": {Custom, "22"},
	} {
		typ, version := FromImage(image)
		if typ.Name != want[0] || version != want[1] {
			t.Errorf("%s → %s %s, want %v", image, typ.Name, version, want)
		}
	}
}

func TestRuntimes(t *testing.T) {
	for runtime, image := range map[string]string{"php:8.3": "php:8.3", "node:22": "node:22", "go:1.25": "golang:1.25", "bun:1": "oven/bun:1"} {
		typ, version, ok := FromRuntime(runtime)
		if !ok || typ.Image(version) != image {
			t.Errorf("%s → %s %s %v, want image %s", runtime, typ.Name, version, ok, image)
		}
	}
	if _, _, ok := FromRuntime("elixir:1.17"); ok {
		t.Error("an unknown language is not a catalog runtime")
	}
	if typ, ok := Lookup("mailtrap"); !ok || typ.Name != "mailpit" {
		t.Errorf("mailtrap → %v %v", typ.Name, ok)
	}
}
