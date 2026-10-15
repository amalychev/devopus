// Command p4bench is the stage-0 prototype P-4: MySQL has no CREATE DATABASE … TEMPLATE,
// so which way of giving a feature its own copy of the main database should the agent use?
// Throwaway code: see README.md.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

const image = "devcorb-proto/p4-mysql:8.4.3"

type target struct {
	Key, Title string
	XFS        bool
}

var targets = []target{
	{"ext4", "MySQL 8.4, том Docker (ext4)", false},
	{"xfs", "MySQL 8.4, XFS с reflink", true},
}

type result struct {
	Target                    target
	Size                      string
	Tables                    int
	CloneLocal, CloneLocalGap time.Duration
	GoldenTT, GoldenTTGap     time.Duration
	BaseGap                   time.Duration
	Methods                   []methodResult
}

type methodResult struct {
	Name     string
	Times    []time.Duration
	Disk     string
	Verified bool
	Extra    string
	Err      string
}

func main() {
	sizeGB := flag.Float64("size", 1, "approximate size of the main database in GB")
	runs := flag.Int("runs", 3, "clones per method")
	only := flag.String("targets", "", "comma-separated: ext4,xfs")
	keep := flag.Bool("keep", false, "keep containers and volumes")
	flag.Parse()

	var rs []result
	for _, t := range targets {
		if *only != "" && !strings.Contains(","+*only+",", ","+t.Key+",") {
			continue
		}
		fmt.Fprintf(os.Stderr, "\n== %s ==\n", t.Title)
		r, err := bench(t, *sizeGB, *runs)
		if !*keep {
			cleanup(t)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		rs = append(rs, r)
	}
	fmt.Print(report(rs, *runs))
}

func name(t target) string { return "p4-" + t.Key }

func cleanup(t target) {
	_, _ = run("docker", "rm", "-f", name(t))
	_, _ = run("docker", "volume", "rm", "-f", name(t))
}

func bench(t target, sizeGB float64, runs int) (result, error) {
	r := result{Target: t}
	cleanup(t)
	if err := start(t); err != nil {
		return r, err
	}

	logf("seeding ~%.1f GB…", sizeGB)
	if err := seed(t, sizeGB); err != nil {
		return r, err
	}
	// information_schema sizes lag behind for fresh InnoDB tables; count the files instead.
	mb, _ := sh(t, "du -sm /data/main/app | cut -f1")
	r.Size = strings.TrimSpace(mb) + " MB"
	n, _ := sql(t, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='app'")
	r.Tables, _ = strconv.Atoi(n)
	logf("app: %s in %d tables", r.Size, r.Tables)

	r.BaseGap = probe(t, func() {})

	// Golden instance: CLONE LOCAL makes a consistent physical copy of the running server.
	if _, err := sql(t, "INSTALL PLUGIN clone SONAME 'mysql_clone.so'"); err != nil {
		return r, err
	}
	r.CloneLocalGap = probe(t, func() {
		t0 := time.Now()
		if _, err := sql(t, "CLONE LOCAL DATA DIRECTORY = '/data/golden'"); err != nil {
			logf("clone local: %v", err)
		}
		r.CloneLocal = time.Since(t0)
	})
	logf("golden instance (CLONE LOCAL): %s; writes to main blocked ≤ %s", r.CloneLocal.Round(10*time.Millisecond), r.CloneLocalGap.Round(10*time.Millisecond))

	// Golden schema in the same instance via transportable tablespaces.
	r.GoldenTTGap = probe(t, func() {
		t0 := time.Now()
		if _, _, err := transport(t, "app", "golden"); err != nil {
			logf("golden schema: %v", err)
		}
		r.GoldenTT = time.Since(t0)
	})
	logf("golden schema (transportable tablespaces): %s; writes to main blocked ≤ %s", r.GoldenTT.Round(10*time.Millisecond), r.GoldenTTGap.Round(10*time.Millisecond))

	want, _ := sql(t, "SELECT COUNT(*) FROM golden.accounts")

	// 1. mysqldump | mysql into a new schema.
	m := methodResult{Name: "mysqldump → mysql, новая схема"}
	for i := 0; i < runs; i++ {
		db := fmt.Sprintf("feature_%d", i)
		before := usedMB(t)
		t0 := time.Now()
		_, err := sh(t, fmt.Sprintf(`mysql -uroot -S /tmp/main.sock -e "CREATE DATABASE %s" && mysqldump -uroot -S /tmp/main.sock --single-transaction --skip-lock-tables golden | mysql -uroot -S /tmp/main.sock %s`, db, db))
		if err != nil {
			m.Err = firstLine(err.Error())
			break
		}
		m.Times = append(m.Times, time.Since(t0))
		if i == 0 {
			m.Disk = fmt.Sprintf("%d МБ", usedMB(t)-before)
			got, _ := sql(t, "SELECT COUNT(*) FROM "+db+".accounts")
			m.Verified = got == want
		}
	}
	dropFeatures(t, runs)
	r.Methods = append(r.Methods, m)
	logf("%s: %v %s", m.Name, m.Times, m.Err)

	// 2. Transportable tablespaces golden → new schema.
	m = methodResult{Name: "transportable tablespaces, новая схема"}
	if t.XFS {
		m.Name += " (cp --reflink)"
	}
	var copyPart, importPart []time.Duration
	for i := 0; i < runs; i++ {
		db := fmt.Sprintf("feature_%d", i)
		before := usedMB(t)
		t0 := time.Now()
		cp, imp, err := transport(t, "golden", db)
		if err != nil {
			m.Err = firstLine(err.Error())
			break
		}
		m.Times = append(m.Times, time.Since(t0))
		copyPart, importPart = append(copyPart, cp), append(importPart, imp)
		if i == 0 {
			m.Disk = fmt.Sprintf("%d МБ", usedMB(t)-before)
			got, _ := sql(t, "SELECT COUNT(*) FROM "+db+".accounts")
			m.Verified = got == want
		}
	}
	if len(copyPart) > 0 {
		m.Extra = fmt.Sprintf("копирование файлов под блокировкой %s, IMPORT %d таблиц %s", fmtDur(median(copyPart)), r.Tables, fmtDur(median(importPart)))
	}
	dropFeatures(t, runs)
	r.Methods = append(r.Methods, m)
	logf("%s: %v (%s) %s", m.Name, m.Times, m.Extra, m.Err)

	// 3. Separate instance: copy the stopped golden instance and start mysqld on it.
	m = methodResult{Name: "отдельный инстанс: копия golden + запуск mysqld"}
	cp := "cp -a"
	if t.XFS {
		cp = "cp -a --reflink=always"
		m.Name = "отдельный инстанс: cp --reflink golden + запуск mysqld"
	}
	var rss []int
	for i := 0; i < runs; i++ {
		dir := fmt.Sprintf("/data/f%d", i)
		sock := fmt.Sprintf("/tmp/f%d.sock", i)
		before := usedMB(t)
		t0 := time.Now()
		_, err := sh(t, fmt.Sprintf(`%s /data/golden %s && chown -R mysql:mysql %s && \
			(mysqld --user=mysql --datadir=%s --socket=%s --port=%d --mysqlx=OFF --innodb-buffer-pool-size=128M \
			  --pid-file=/tmp/f%d.pid --log-error=%s.err >/dev/null 2>&1 &) && \
			for k in $(seq 1 600); do mysqladmin -uroot -S %s ping >/dev/null 2>&1 && break; sleep 0.05; done; \
			mysqladmin -uroot -S %s ping`, cp, dir, dir, dir, sock, 3310+i, i, dir, sock, sock))
		if err != nil {
			m.Err = firstLine(err.Error())
			break
		}
		m.Times = append(m.Times, time.Since(t0))
		if i == 0 {
			m.Disk = fmt.Sprintf("%d МБ", usedMB(t)-before)
			got, _ := run("docker", "exec", name(t), "mysql", "-uroot", "-S", sock, "-N", "-B", "-e", "SELECT COUNT(*) FROM app.accounts")
			m.Verified = strings.TrimSpace(got) == want
		}
		kb, _ := sh(t, fmt.Sprintf(`awk '/VmRSS/ {print $2}' /proc/$(cat /tmp/f%d.pid)/status`, i))
		if v, err := strconv.Atoi(strings.TrimSpace(kb)); err == nil {
			rss = append(rss, v/1024)
		}
	}
	if len(rss) > 0 {
		sort.Ints(rss)
		m.Extra = fmt.Sprintf("RSS инстанса сразу после старта ~%d МБ (buffer pool 128 МБ)", rss[len(rss)/2])
	}
	for i := 0; i < runs; i++ {
		_, _ = sh(t, fmt.Sprintf("mysqladmin -uroot -S /tmp/f%d.sock shutdown 2>/dev/null; sleep 1; rm -rf /data/f%d", i, i))
	}
	r.Methods = append(r.Methods, m)
	logf("%s: %v (%s) %s", m.Name, m.Times, m.Extra, m.Err)
	return r, nil
}

func start(t target) error {
	args := []string{"run", "-d", "--name", name(t)}
	if t.XFS {
		// Format the loop image with a helper; the MySQL image has no mkfs.xfs.
		if _, err := run("docker", "run", "--rm", "-v", name(t)+":/loop", "alpine:3.20", "sh", "-c",
			"apk add -q xfsprogs >/dev/null && truncate -s 48G /loop/xfs.img && mkfs.xfs -q -m reflink=1 /loop/xfs.img"); err != nil {
			return err
		}
		args = append(args, "--privileged", "-e", "XFS=1", "-v", name(t)+":/loop")
	} else {
		args = append(args, "-v", name(t)+":/data")
	}
	if _, err := run("docker", append(args, image)...); err != nil {
		return err
	}
	for i := 0; i < 120; i++ {
		if _, err := sh(t, "mysqladmin -uroot -S /tmp/main.sock ping"); err == nil {
			return nil
		}
		time.Sleep(time.Second)
	}
	logs, _ := sh(t, "tail -20 /data/main.err")
	return fmt.Errorf("mysqld did not start: %s", logs)
}

func seed(t target, sizeGB float64) error {
	// ~250 bytes per row on disk: 1 GB ≈ 4.1M rows = 1000 rows doubled 12 times.
	doublings := int(math.Round(math.Log2(sizeGB * 4096)))
	var b strings.Builder
	b.WriteString(`CREATE DATABASE app; USE app;
SET SESSION cte_max_recursion_depth = 10000;
CREATE TABLE accounts (id BIGINT AUTO_INCREMENT PRIMARY KEY, aid INT NOT NULL, balance INT NOT NULL,
  filler CHAR(200) CHARACTER SET latin1 NOT NULL, KEY (aid)) ENGINE=InnoDB;
INSERT INTO accounts (aid, balance, filler)
  WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM s WHERE n < 1000)
  SELECT n, n % 1000, REPEAT('x', 200) FROM s;
CREATE TABLE probe (id BIGINT AUTO_INCREMENT PRIMARY KEY, ts DOUBLE NOT NULL) ENGINE=InnoDB;
`)
	for i := 0; i < doublings; i++ {
		fmt.Fprintf(&b, "INSERT INTO accounts (aid, balance, filler) SELECT aid + %d, balance, filler FROM accounts;\n", 1000<<i)
	}
	// Real schemas have many tables; per-table cost matters for transportable tablespaces.
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&b, `CREATE TABLE t%03d (id INT PRIMARY KEY, v VARCHAR(100) NOT NULL) ENGINE=InnoDB;
INSERT INTO t%03d WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM s WHERE n < 100) SELECT n, MD5(n) FROM s;
`, i, i)
	}
	cmd := exec.Command("docker", "exec", "-i", name(t), "mysql", "-uroot", "-S", "/tmp/main.sock")
	cmd.Stdin = strings.NewReader(b.String())
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("seed: %v: %s", err, out)
	}
	return nil
}

// transport copies every table of src into a new schema dst with transportable tablespaces:
// CREATE … LIKE + DISCARD, then FLUSH … FOR EXPORT and copy the .ibd/.cfg files while the
// read lock is held, then IMPORT. Writes to src wait during the copy; reads continue.
func transport(t target, src, dst string) (copyDur, importDur time.Duration, err error) {
	list, err := sql(t, fmt.Sprintf("SELECT GROUP_CONCAT(table_name ORDER BY table_name) FROM information_schema.tables WHERE table_schema='%s'", src))
	if err != nil {
		return 0, 0, err
	}
	tables := strings.Split(list, ",")
	var prep, lock, imp strings.Builder
	fmt.Fprintf(&prep, "CREATE DATABASE %s;\n", dst)
	var qualified []string
	for _, tb := range tables {
		fmt.Fprintf(&prep, "CREATE TABLE %s.%s LIKE %s.%s; ALTER TABLE %s.%s DISCARD TABLESPACE;\n", dst, tb, src, tb, dst, tb)
		fmt.Fprintf(&imp, "ALTER TABLE %s.%s IMPORT TABLESPACE;\n", dst, tb)
		qualified = append(qualified, src+"."+tb)
	}
	cp := "cp"
	if t.XFS {
		cp = "cp --reflink=always"
	}
	fmt.Fprintf(&lock, "FLUSH TABLES %s FOR EXPORT;\n", strings.Join(qualified, ", "))
	fmt.Fprintf(&lock, "system %s /data/main/%s/*.ibd /data/main/%s/*.cfg /data/main/%s/ && chown mysql:mysql /data/main/%s/*\n", cp, src, src, dst, dst)
	lock.WriteString("UNLOCK TABLES;\n")

	if err := script(t, prep.String()); err != nil {
		return 0, 0, err
	}
	t0 := time.Now()
	if err := script(t, lock.String()); err != nil {
		return 0, 0, err
	}
	copyDur = time.Since(t0)
	t0 = time.Now()
	if err := script(t, imp.String()); err != nil {
		return copyDur, 0, err
	}
	return copyDur, time.Since(t0), nil
}

func dropFeatures(t target, runs int) {
	for i := 0; i < runs; i++ {
		_, _ = sql(t, fmt.Sprintf("DROP DATABASE IF EXISTS feature_%d", i))
	}
}

// probe inserts into app.probe in a loop while fn runs and returns the longest interval
// between two committed inserts, i.e. how long writes to main were blocked.
func probe(t target, fn func()) time.Duration {
	_, _ = sh(t, "rm -f /tmp/probe.log; touch /tmp/probe.on")
	_, _ = run("docker", "exec", "-d", name(t), "sh", "-c",
		`while [ -f /tmp/probe.on ]; do mysql -uroot -S /tmp/main.sock -N -B -e "INSERT INTO app.probe (ts) VALUES (UNIX_TIMESTAMP(NOW(6))); SELECT UNIX_TIMESTAMP(NOW(6));" >> /tmp/probe.log 2>/dev/null; done`)
	time.Sleep(1500 * time.Millisecond)
	fn()
	time.Sleep(1500 * time.Millisecond)
	_, _ = sh(t, "rm -f /tmp/probe.on")
	time.Sleep(500 * time.Millisecond)
	out, _ := sh(t, "cat /tmp/probe.log")
	var ts []float64
	for _, f := range strings.Fields(out) {
		if v, err := strconv.ParseFloat(f, 64); err == nil {
			ts = append(ts, v)
		}
	}
	sort.Float64s(ts)
	var gap float64
	for i := 1; i < len(ts); i++ {
		gap = math.Max(gap, ts[i]-ts[i-1])
	}
	return time.Duration(gap * float64(time.Second))
}

func usedMB(t target) int64 {
	out, _ := sh(t, "sync; df -m /data | tail -1 | awk '{print $3}'")
	v, _ := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	return v
}

func sql(t target, q string) (string, error) {
	out, err := run("docker", "exec", name(t), "mysql", "-uroot", "-S", "/tmp/main.sock", "-N", "-B", "-e", q)
	return strings.TrimSpace(out), err
}

func script(t target, s string) error {
	cmd := exec.Command("docker", "exec", "-i", name(t), "mysql", "-uroot", "-S", "/tmp/main.sock")
	cmd.Stdin = strings.NewReader(s)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}

func sh(t target, s string) (string, error) { return run("docker", "exec", name(t), "sh", "-c", s) }

func run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%v: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

func logf(f string, a ...any) { fmt.Fprintf(os.Stderr, "  "+f+"\n", a...) }

func firstLine(s string) string { l, _, _ := strings.Cut(s, "\n"); return l }

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
	fmt.Fprintf(&b, "## Клон базы фичи (медиана из %d)\n\n| Окружение | БД | Способ | Время | Место за клон | Данные совпадают | Детали |\n|---|---|---|---|---|---|---|\n", runs)
	for _, r := range rs {
		for _, m := range r.Methods {
			if m.Err != "" {
				fmt.Fprintf(&b, "| %s | %s | %s | ошибка: `%s` | — | — | — |\n", r.Target.Title, r.Size, m.Name, m.Err)
				continue
			}
			ok := "да"
			if !m.Verified {
				ok = "нет"
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s |\n", r.Target.Title, r.Size, m.Name, fmtDur(median(m.Times)), m.Disk, ok, m.Extra)
		}
	}
	fmt.Fprintf(&b, "\n## Золотая копия: цена для main\n\n| Окружение | Способ | Время | Запись в main заблокирована | Обычный интервал проб |\n|---|---|---|---|---|\n")
	for _, r := range rs {
		fmt.Fprintf(&b, "| %s | CLONE LOCAL (golden-инстанс) | %s | %s | %s |\n", r.Target.Title, fmtDur(r.CloneLocal), fmtDur(r.CloneLocalGap), fmtDur(r.BaseGap))
		fmt.Fprintf(&b, "| %s | transportable tablespaces (golden-схема, %d таблиц) | %s | %s | %s |\n", r.Target.Title, r.Tables, fmtDur(r.GoldenTT), fmtDur(r.GoldenTTGap), fmtDur(r.BaseGap))
	}
	return b.String()
}
