// Command p3bench is the stage-0 prototype P-3: how fast can a feature get its own copy of
// the main Postgres database, and what does refreshing the golden copy cost main?
// Throwaway code: see README.md.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type target struct {
	Key   string
	Title string
	Image string
	XFS   bool // data directory on a reflink-enabled XFS loop device
}

var targets = []target{
	{"pg17-ext4", "PostgreSQL 17, том Docker (ext4)", "postgres:17-alpine", false},
	{"pg18-ext4", "PostgreSQL 18, том Docker (ext4)", "postgres:18-alpine", false},
	{"pg18-xfs", "PostgreSQL 18, XFS с reflink", "postgres:18-alpine", true},
}

type method struct {
	Name     string
	SQL      string // statements before CREATE DATABASE
	Strategy string
	MinMajor int
}

var methods = []method{
	{"WAL_LOG (по умолчанию с PG 15)", "", "WAL_LOG", 15},
	{"FILE_COPY, file_copy_method=copy", "", "FILE_COPY", 15},
	{"FILE_COPY, file_copy_method=clone", "SET file_copy_method = clone;", "FILE_COPY", 18},
}

type result struct {
	Target      target
	DBSize      string
	SeedTime    time.Duration
	GoldenTime  time.Duration
	GoldenGap   time.Duration
	BaseGap     time.Duration
	Clones      map[string][]time.Duration
	CloneErrs   map[string]string
	DiskPerCopy map[string]string
	DataDirCopy time.Duration
	DataDirSize string
	Verified    bool
}

func main() {
	sizeGB := flag.Float64("size", 1, "database size in GB (pgbench scale ≈ 68 per GB)")
	runs := flag.Int("runs", 3, "clones per method")
	only := flag.String("targets", "", "comma-separated target keys (default: all)")
	keep := flag.Bool("keep", false, "keep containers and volumes")
	flag.Parse()

	scale := int(*sizeGB * 68)
	var results []result
	for _, t := range targets {
		if *only != "" && !strings.Contains(","+*only+",", ","+t.Key+",") {
			continue
		}
		fmt.Fprintf(os.Stderr, "\n== %s ==\n", t.Title)
		r, err := bench(t, scale, *runs)
		if !*keep {
			cleanup(t)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %s: %v\n", t.Key, err)
			os.Exit(1)
		}
		results = append(results, r)
	}
	fmt.Print(report(results, *runs))
}

func container(t target) string { return "p3-" + t.Key }

func cleanup(t target) {
	_, _ = run("docker", "rm", "-f", container(t))
	_, _ = run("docker", "volume", "rm", "-f", container(t))
}

func bench(t target, scale, runs int) (result, error) {
	r := result{Target: t, Clones: map[string][]time.Duration{}, CloneErrs: map[string]string{}, DiskPerCopy: map[string]string{}}
	cleanup(t)
	if err := start(t); err != nil {
		return r, err
	}
	major := 17
	if strings.Contains(t.Image, ":18") {
		major = 18
	}

	logf("seeding pgbench scale %d…", scale)
	t0 := time.Now()
	if _, err := sh(t, fmt.Sprintf("pgbench -U postgres -i -q -s %d app", scale)); err != nil {
		return r, err
	}
	r.SeedTime = time.Since(t0)
	r.DBSize, _ = psql(t, "postgres", "SELECT pg_size_pretty(pg_database_size('app'))")
	logf("app is %s (seeded in %s)", r.DBSize, r.SeedTime.Round(time.Second))

	// Golden copy: main keeps running, so new connections are refused while the copy is made
	// (docs/architecture/data.md). A probe loop measures how long main is unreachable.
	goldenMethod := methods[1]
	if major >= 18 && t.XFS {
		goldenMethod = methods[2]
	}
	r.BaseGap = probe(t, func() {})
	r.GoldenGap = probe(t, func() {
		t0 := time.Now()
		_, err := psql(t, "postgres",
			"ALTER DATABASE app ALLOW_CONNECTIONS false;",
			"SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity WHERE datname = 'app';",
			// CREATE DATABASE cannot share a query string with other statements (implicit transaction).
			setting(goldenMethod),
			"CREATE DATABASE golden TEMPLATE app STRATEGY "+goldenMethod.Strategy+";",
			"ALTER DATABASE app ALLOW_CONNECTIONS true;")
		r.GoldenTime = time.Since(t0)
		if err != nil {
			logf("golden copy failed: %v", err)
		}
	})
	logf("golden copy (%s): %s; main unreachable for %s (normal probe gap %s)",
		goldenMethod.Name, r.GoldenTime.Round(10*time.Millisecond), r.GoldenGap.Round(10*time.Millisecond), r.BaseGap.Round(10*time.Millisecond))

	for _, m := range methods {
		if major < m.MinMajor {
			continue
		}
		before := usedKB(t)
		for i := 0; i < runs; i++ {
			name := fmt.Sprintf("feature_%d", i)
			d, err := timedSQL(t, setting(m), "CREATE DATABASE "+name+" TEMPLATE golden STRATEGY "+m.Strategy+";")
			if err != nil {
				r.CloneErrs[m.Name] = firstLine(err.Error())
				break
			}
			r.Clones[m.Name] = append(r.Clones[m.Name], d)
			if i == 0 {
				r.DiskPerCopy[m.Name] = fmt.Sprintf("%.0f МБ", float64(usedKB(t)-before)/1024)
				if !r.Verified {
					src, _ := psql(t, "golden", "SELECT count(*) FROM pgbench_accounts")
					dst, _ := psql(t, name, "SELECT count(*) FROM pgbench_accounts")
					r.Verified = src != "" && src == dst
				}
			}
		}
		for i := 0; i < runs; i++ {
			_, _ = psql(t, "postgres", fmt.Sprintf("DROP DATABASE IF EXISTS feature_%d;", i))
		}
		if ds := r.Clones[m.Name]; len(ds) > 0 {
			logf("%s: %v", m.Name, ds)
		} else {
			logf("%s: %s", m.Name, r.CloneErrs[m.Name])
		}
	}

	// "Separate instance per feature": copy the whole data directory after a checkpoint.
	// Timing only; a real copy needs a stopped or backup-mode server.
	_, _ = psql(t, "postgres", "CHECKPOINT;")
	cp := "cp -a"
	if t.XFS {
		cp = "cp -a --reflink=always"
	}
	t0 = time.Now()
	if _, err := sh(t, cp+` "$PGDATA" "$PGDATA.copy" && sync`); err != nil {
		return r, err
	}
	r.DataDirCopy = time.Since(t0)
	r.DataDirSize, _ = sh(t, `du -sh "$PGDATA.copy" | cut -f1`)
	logf("data dir copy (%s, %s): %s", cp, r.DataDirSize, r.DataDirCopy.Round(10*time.Millisecond))
	return r, nil
}

func start(t target) error {
	args := []string{"run", "-d", "--name", container(t), "-e", "POSTGRES_PASSWORD=dev", "-e", "POSTGRES_DB=app"}
	if t.XFS {
		// Loop-mounted XFS inside the container; the image file lives on the Docker volume.
		args = append(args, "--privileged", "-v", container(t)+":/loop", "-e", "PGDATA=/mnt/xfs/pgdata",
			"--entrypoint", "sh", t.Image, "-c",
			`set -e; apk add -q xfsprogs >/dev/null
			 [ -f /loop/xfs.img ] || { truncate -s 48G /loop/xfs.img; mkfs.xfs -q -m reflink=1 /loop/xfs.img; }
			 mkdir -p /mnt/xfs && mount -o loop /loop/xfs.img /mnt/xfs
			 exec docker-entrypoint.sh postgres -c max_wal_size=4GB`)
	} else {
		args = append(args, "-v", container(t)+":/var/lib/postgresql", "-e", "PGDATA=/var/lib/postgresql/pgdata",
			t.Image, "postgres", "-c", "max_wal_size=4GB")
	}
	if _, err := run("docker", args...); err != nil {
		return err
	}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		// The entrypoint restarts the server after initdb; wait for the final one.
		if out, err := psql(t, "app", "SELECT 1"); err == nil && out == "1" {
			time.Sleep(2 * time.Second)
			if out, err := psql(t, "app", "SELECT 1"); err == nil && out == "1" {
				return nil
			}
		}
		time.Sleep(time.Second)
	}
	logs, _ := run("docker", "logs", "--tail", "30", container(t))
	return fmt.Errorf("postgres did not start:\n%s", logs)
}

// probe runs a connect+select loop against app inside the container while fn runs and
// returns the longest interval between two successful probes.
func probe(t target, fn func()) time.Duration {
	_, _ = sh(t, "rm -f /tmp/probe.log; touch /tmp/probe.on")
	_, _ = run("docker", "exec", "-d", container(t), "sh", "-c",
		`while [ -f /tmp/probe.on ]; do psql -U postgres -d app -Atqc "SELECT extract(epoch from clock_timestamp())" >> /tmp/probe.log 2>/dev/null; done`)
	time.Sleep(1500 * time.Millisecond)
	fn()
	time.Sleep(1500 * time.Millisecond)
	_, _ = sh(t, "rm -f /tmp/probe.on")
	time.Sleep(300 * time.Millisecond)
	out, _ := sh(t, "cat /tmp/probe.log")
	var ts []float64
	for _, line := range strings.Fields(out) {
		if v, err := strconv.ParseFloat(line, 64); err == nil {
			ts = append(ts, v)
		}
	}
	sort.Float64s(ts)
	var maxGap float64
	for i := 1; i < len(ts); i++ {
		if g := ts[i] - ts[i-1]; g > maxGap {
			maxGap = g
		}
	}
	return time.Duration(maxGap * float64(time.Second))
}

var timingRe = regexp.MustCompile(`Time: ([0-9.]+) ms`)

// timedSQL returns the server-side duration psql reports for the last statement.
func timedSQL(t target, stmts ...string) (time.Duration, error) {
	args := []string{"exec", container(t), "psql", "-U", "postgres", "-d", "postgres", "-v", "ON_ERROR_STOP=1", "-c", `\timing on`}
	for _, s := range stmts {
		args = append(args, "-c", s)
	}
	out, err := run("docker", args...)
	if err != nil {
		return 0, err
	}
	m := timingRe.FindAllStringSubmatch(out, -1)
	if len(m) == 0 {
		return 0, fmt.Errorf("no timing in %q", out)
	}
	ms, _ := strconv.ParseFloat(m[len(m)-1][1], 64)
	return time.Duration(ms * float64(time.Millisecond)), nil
}

// setting returns the method's SET statement, or a no-op so every call has the same shape.
func setting(m method) string {
	if m.SQL == "" {
		return "SELECT 1;"
	}
	return m.SQL
}

func psql(t target, db string, stmts ...string) (string, error) {
	args := []string{"exec", container(t), "psql", "-U", "postgres", "-d", db, "-Atq", "-v", "ON_ERROR_STOP=1"}
	for _, s := range stmts {
		args = append(args, "-c", s)
	}
	return run("docker", args...)
}

func sh(t target, script string) (string, error) {
	return run("docker", "exec", container(t), "sh", "-c", script)
}

func usedKB(t target) int64 {
	dir := "/var/lib/postgresql"
	if t.XFS {
		dir = "/mnt/xfs"
	}
	out, err := sh(t, "sync; df -k "+dir+" | tail -1 | awk '{print $3}'")
	if err != nil {
		return 0
	}
	v, _ := strconv.ParseInt(out, 10, 64)
	return v
}

func run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(out.String()), fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func logf(format string, a ...any) { fmt.Fprintf(os.Stderr, "  "+format+"\n", a...) }

func firstLine(s string) string {
	if i := strings.LastIndex(s, "ERROR:"); i >= 0 {
		s = s[i:]
	}
	s, _, _ = strings.Cut(s, "\n")
	return s
}

func median(ds []time.Duration) time.Duration {
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}

func fmtDur(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%d мс", d.Milliseconds())
	}
	return strings.Replace(fmt.Sprintf("%.1f с", d.Seconds()), ".", ",", 1)
}

func report(rs []result, runs int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Клон из золотой копии (медиана из %d)\n\n", runs)
	fmt.Fprintf(&b, "| Окружение | Размер БД | Способ | Время клона | Место на диске за клон |\n|---|---|---|---|---|\n")
	for _, r := range rs {
		for _, m := range methods {
			if ds, ok := r.Clones[m.Name]; ok {
				fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", r.Target.Title, r.DBSize, m.Name, fmtDur(median(ds)), r.DiskPerCopy[m.Name])
			} else if e, ok := r.CloneErrs[m.Name]; ok {
				fmt.Fprintf(&b, "| %s | %s | %s | ошибка: `%s` | — |\n", r.Target.Title, r.DBSize, m.Name, e)
			}
		}
	}
	fmt.Fprintf(&b, "\n## Обновление золотой копии и копия каталога данных\n\n")
	fmt.Fprintf(&b, "| Окружение | Создание золотой копии | main недоступна | Обычный интервал проб | Копия всего PGDATA | Данные клона совпадают |\n|---|---|---|---|---|---|\n")
	for _, r := range rs {
		ok := "да"
		if !r.Verified {
			ok = "нет"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s (%s) | %s |\n", r.Target.Title, fmtDur(r.GoldenTime), fmtDur(r.GoldenGap), fmtDur(r.BaseGap), fmtDur(r.DataDirCopy), r.DataDirSize, ok)
	}
	return b.String()
}
