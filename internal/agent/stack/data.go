package stack

import (
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/amalychev/devopus/internal/spec"
)

// Engines whose data a feature gets as a copy of the base environment's (ADR-0007).
const (
	EnginePostgres = "postgres"
	EngineMySQL    = "mysql"
)

// GoldenMount is where the base environment's database container sees its golden
// volume; the copy itself is in the data folder there.
const GoldenMount = "/devopus/golden"

// Data is a database of the environment whose data features copy: the base keeps a
// golden copy of it, a feature starts its own instance from that copy.
type Data struct {
	Service string
	Engine  string
	Image   string
	// Volume holds the data; Path is the data folder inside it, "" for its root.
	Volume string
	Path   string
	// DataDir is the data folder in the container.
	DataDir string
	// Golden is the base environment's volume with the copy.
	Golden string
	// Container is the service's container in this environment.
	Container string
	// Uses are the modules of the environment that use the service: their migrations
	// shape its data.
	Uses []string
}

// Engine recognises a database by its image. MariaDB has no clone plugin, so it is not
// one of them yet.
func Engine(image string) string {
	repo := image
	if i := strings.IndexAny(repo, ":@"); i >= 0 {
		repo = repo[:i]
	}
	name := path.Base(repo)
	switch name {
	case "postgres", "postgis", "pgvector", "timescaledb", "timescaledb-ha":
		return EnginePostgres
	case "mysql", "percona-server", "mysql-server":
		return EngineMySQL
	}
	return ""
}

var (
	pgSuffix = regexp.MustCompile(`(?:^|[^a-z0-9])pg(\d+)`)
	leading  = regexp.MustCompile(`^(\d+)`)
)

// DataDir is where the engine keeps its data in the container: PGDATA if set, the
// image's default otherwise (PostgreSQL 18 moved it).
func DataDir(engine, image string, env map[string]string) string {
	switch engine {
	case EnginePostgres:
		if v := env["PGDATA"]; v != "" && !spec.HasExpr(v) {
			return path.Clean(v)
		}
		if major := postgresMajor(image); major >= 18 {
			return "/var/lib/postgresql/" + strconv.Itoa(major) + "/docker"
		}
		return "/var/lib/postgresql/data"
	case EngineMySQL:
		return "/var/lib/mysql"
	}
	return ""
}

func postgresMajor(image string) int {
	if i := strings.Index(image, "@"); i >= 0 {
		image = image[:i]
	}
	tag := ""
	if i := strings.LastIndex(image, ":"); i >= 0 && !strings.Contains(image[i:], "/") {
		tag = image[i+1:]
	}
	// pgvector:pg18 and timescaledb:2.17-pg18 name the major after pg; postgres:18.1 leads with it.
	m := pgSuffix.FindStringSubmatch(tag)
	if m == nil {
		m = leading.FindStringSubmatch(tag)
	}
	if m == nil {
		return 0
	}
	v, _ := strconv.Atoi(m[1])
	return v
}

// dataOf finds the database data of a service: its engine and the named volume that
// holds its data folder. ok is false for services that are not databases.
func (b *builder) dataOf(name string, s *spec.Service, container string) (Data, string, bool) {
	image := s.ImageRef()
	engine := Engine(image)
	if engine == "" {
		return Data{}, "", false
	}
	dir := DataDir(engine, image, s.Env)
	best, rel := "", ""
	for _, v := range s.Volumes {
		parts := strings.Split(v, ":")
		if len(parts) < 2 || strings.HasPrefix(parts[0], ".") || strings.HasPrefix(parts[0], "/") {
			continue
		}
		target := path.Clean(parts[1])
		r, ok := within(dir, target)
		if ok && len(target) > len(best) {
			best, rel = parts[0], r
		}
	}
	if best == "" {
		return Data{}, name + " keeps its data in the container (no volume at " + dir + "): features get an empty " + name, true
	}
	return Data{
		Service: name, Engine: engine, Image: image,
		Volume: VolumeName(b.env.Project, b.env.ID, best), Path: rel, DataDir: dir,
		Golden:    GoldenVolume(b.env.Project, b.baseID(), name),
		Container: container,
	}, "", true
}

// GoldenVolume is the base environment's volume with the golden copy of a service.
func GoldenVolume(project, baseID, service string) string {
	return VolumeName(project, baseID, service+".golden")
}

func (b *builder) baseID() string {
	if b.env.BaseID != "" {
		return b.env.BaseID
	}
	return b.env.ID
}

// within reports whether dir is target or inside it, and the path relative to it.
func within(dir, target string) (string, bool) {
	if dir == target {
		return "", true
	}
	if strings.HasPrefix(dir, strings.TrimSuffix(target, "/")+"/") {
		return strings.TrimPrefix(dir, strings.TrimSuffix(target, "/")+"/"), true
	}
	return "", false
}

// leanMySQL are the settings of a feature's MySQL: about 120 MB instead of 420 (P-4).
var leanMySQL = []string{"mysqld", "--performance-schema=OFF", "--innodb-buffer-pool-size=32M", "--max-connections=50"}
