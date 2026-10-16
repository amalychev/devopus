// Package catalog knows the types of services a project is made of (docs/product/
// glossary.md): runtimes the applications run on, stores and tools that run as their
// own containers, and processes that run an application's code in the background.
package catalog

import (
	"sort"
	"strings"
)

// Kind tells how a service runs.
type Kind string

const (
	// Runtime: no container of its own; the applications it is attached to run on its
	// image (php 8.3 → php:8.3) with their code mounted.
	Runtime Kind = "runtime"
	// Container: its own container from its image (mysql, redis, typesense…).
	Container Kind = "container"
	// Process: runs the code of the application it is attached to with its own command
	// (a queue worker, a cron job).
	Process Kind = "process"
)

// Type is a kind of service the catalog knows.
type Type struct {
	Name string
	Kind Kind
	// Repo is the image repository; the version is the tag.
	Repo string
	// Port is where other containers connect; UIPort serves a web UI.
	Port, UIPort int
	// Aliases are other names people use for the type.
	Aliases []string
}

// Custom is a container service from an image the catalog does not know.
const Custom = "custom"

// Queue and Cronjob are the process types.
const (
	Queue   = "queue"
	Cronjob = "cronjob"
)

var types = []Type{
	{Name: "php", Kind: Runtime, Repo: "php"},
	{Name: "nodejs", Kind: Runtime, Repo: "node", Aliases: []string{"node"}},
	{Name: "python", Kind: Runtime, Repo: "python"},
	{Name: "ruby", Kind: Runtime, Repo: "ruby"},
	{Name: "go", Kind: Runtime, Repo: "golang", Aliases: []string{"golang"}},
	{Name: "java", Kind: Runtime, Repo: "eclipse-temurin"},
	{Name: "dotnet", Kind: Runtime, Repo: "mcr.microsoft.com/dotnet/sdk"},
	{Name: "bun", Kind: Runtime, Repo: "oven/bun"},
	{Name: "deno", Kind: Runtime, Repo: "denoland/deno"},

	{Name: "mysql", Kind: Container, Repo: "mysql", Port: 3306},
	{Name: "mariadb", Kind: Container, Repo: "mariadb", Port: 3306},
	{Name: "postgres", Kind: Container, Repo: "postgres", Port: 5432, Aliases: []string{"postgresql"}},
	{Name: "mongodb", Kind: Container, Repo: "mongo", Port: 27017, Aliases: []string{"mongo"}},
	{Name: "redis", Kind: Container, Repo: "redis", Port: 6379},
	{Name: "valkey", Kind: Container, Repo: "valkey/valkey", Port: 6379},
	{Name: "memcached", Kind: Container, Repo: "memcached", Port: 11211},
	{Name: "typesense", Kind: Container, Repo: "typesense/typesense", Port: 8108},
	{Name: "meilisearch", Kind: Container, Repo: "getmeili/meilisearch", Port: 7700},
	{Name: "elasticsearch", Kind: Container, Repo: "elasticsearch", Port: 9200},
	{Name: "rabbitmq", Kind: Container, Repo: "rabbitmq", Port: 5672, UIPort: 15672},
	{Name: "minio", Kind: Container, Repo: "minio/minio", Port: 9000, UIPort: 9001},
	{Name: "mailpit", Kind: Container, Repo: "axllent/mailpit", Port: 1025, UIPort: 8025, Aliases: []string{"mailtrap", "mailhog"}},
	{Name: Custom, Kind: Container},

	{Name: Queue, Kind: Process},
	{Name: Cronjob, Kind: Process},
}

var byName = func() map[string]Type {
	m := map[string]Type{}
	for _, t := range types {
		m[t.Name] = t
		for _, a := range t.Aliases {
			m[a] = t
		}
	}
	return m
}()

// Lookup finds a type by its name or an alias.
func Lookup(name string) (Type, bool) {
	t, ok := byName[strings.ToLower(name)]
	return t, ok
}

// Names lists the types, for help and errors.
func Names() []string {
	out := make([]string, 0, len(types))
	for _, t := range types {
		out = append(out, t.Name)
	}
	sort.Strings(out)
	return out
}

// Image is the image of a version of the type: php 8.3 → php:8.3.
func (t Type) Image(version string) string {
	if t.Repo == "" {
		return ""
	}
	if version == "" {
		return t.Repo
	}
	return t.Repo + ":" + version
}

// FromImage recognises the type of a service by its image: postgres:17.6 → postgres
// 17.6, axllent/mailpit:v1.27 → mailpit v1.27. Images the catalog does not know are
// Custom; a digest is not a version.
func FromImage(image string) (Type, string) {
	ref, _, _ := strings.Cut(image, "@")
	repo, tag := ref, ""
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		repo, tag = ref[:i], ref[i+1:]
	}
	repo = strings.TrimPrefix(strings.TrimPrefix(repo, "docker.io/"), "library/")
	for _, t := range types {
		if t.Kind == Container && t.Repo != "" && t.Repo == repo {
			return t, tag
		}
	}
	return byName[Custom], tag
}

// FromRuntime reads a runtime of workspace.yaml v1: php:8.3 → php 8.3, node:22 → nodejs 22.
func FromRuntime(runtime string) (Type, string, bool) {
	lang, version, _ := strings.Cut(runtime, ":")
	t, ok := Lookup(lang)
	if !ok || t.Kind != Runtime {
		return Type{}, "", false
	}
	return t, version, true
}
