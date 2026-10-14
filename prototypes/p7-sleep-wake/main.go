// Command p7check is the stage-0 prototype P-7: idle features go to sleep, the first request
// wakes them, and nobody gets an error in between. It drives compose.yaml and prints a
// markdown report. Throwaway code: see README.md.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	edge       = "http://127.0.0.1:18095"
	sleepAfter = 6 * time.Second
)

type row struct {
	Group, Name, Result string
	OK                  bool
}

var rows []row

func check(group, name string, ok bool, format string, a ...any) {
	res := fmt.Sprintf(format, a...)
	rows = append(rows, row{group, name, res, ok})
	mark := "✓"
	if !ok {
		mark = "✗"
	}
	fmt.Fprintf(os.Stderr, "  %s %-50s %s\n", mark, name, res)
}

type envStatus struct {
	Name       string `json:"name"`
	Running    bool   `json:"running"`
	Wakes      int    `json:"wakes"`
	Sleeps     int    `json:"sleeps"`
	LastWakeMs int64  `json:"last_wake_ms"`
	StartMs    int64  `json:"start_ms"`
	SleepCause string `json:"sleep_cause"`
}

func main() {
	keep := len(os.Args) > 1 && os.Args[1] == "-keep"
	up(0)
	// os.Exit skips deferred calls, so tear down explicitly on every exit path.
	exit := func(code int) {
		if !keep {
			_, _ = run("docker", "compose", "down", "--remove-orphans")
		}
		os.Exit(code)
	}

	fmt.Fprintln(os.Stderr, "activity keeps a feature awake, idleness puts it to sleep:")
	waitAll(true)
	stop := every(2*time.Second, func() { get("pay-1.shop.localhost", "/", nil) })
	time.Sleep(sleepAfter + 6*time.Second)
	stop()
	s := status()
	check("idle", "pay-1: запрос каждые 2 с", s["pay-1"].Running, "running=%v", s["pay-1"].Running)
	check("idle", "ui-2: ни запросов, ни правок файлов", !s["ui-2"].Running && s["ui-2"].SleepCause == "idle",
		"running=%v, причина=%s", s["ui-2"].Running, s["ui-2"].SleepCause)

	get("ui-2.shop.localhost", "/", nil) // wake ui-2
	file := filepath.Join("worktrees", "ui-2", "server.js")
	stop = every(2*time.Second, func() { now := time.Now(); _ = os.Chtimes(file, now, now) })
	time.Sleep(sleepAfter + 6*time.Second)
	stop()
	s = status()
	check("idle", "ui-2: правка файла в worktree каждые 2 с, без запросов", s["ui-2"].Running, "running=%v", s["ui-2"].Running)
	time.Sleep(sleepAfter + 3*time.Second)
	s = status()
	check("idle", "ui-2: правки прекратились", !s["ui-2"].Running, "running=%v через %s простоя", s["ui-2"].Running, sleepAfter+3*time.Second)

	fmt.Fprintln(os.Stderr, "memory:")
	get("pay-1.shop.localhost", "/charge", nil)
	before := memMB("p7-pay-1-")
	sleepEnv("pay-1")
	after := memMB("p7-pay-1-")
	check("mem", "pay-1 (Python + Postgres) после усыпления", after < before, "%.0f МБ → %.0f МБ", before, after)

	fmt.Fprintln(os.Stderr, "wake latency, API client (request is held):")
	for _, c := range []struct{ env, path, what string }{
		{"pay-1", "/charge", "Python + Postgres, запрос пишет в БД"},
		{"ui-2", "/", "Node"},
		{"slow-3", "/", "приложение стартует 3 с"},
	} {
		var total, start []time.Duration
		ok := true
		for i := 0; i < 5; i++ {
			sleepEnv(c.env)
			t0 := time.Now()
			code, body := get(c.env+".shop.localhost", c.path, nil)
			total = append(total, time.Since(t0))
			start = append(start, time.Duration(status()[c.env].StartMs)*time.Millisecond)
			ok = ok && code == 200 && strings.Contains(body, `"env"`)
		}
		check("wake", fmt.Sprintf("%s: %s", c.env, c.what), ok && median(total) < 10*time.Second,
			"медиана %s, макс %s (из них docker start %s), 5/5 ответов 200: %v",
			fmtDur(median(total)), fmtDur(maxDur(total)), fmtDur(median(start)), ok)
	}

	fmt.Fprintln(os.Stderr, "browser:")
	html := map[string]string{"Accept": "text/html"}
	sleepEnv("pay-1")
	t0 := time.Now()
	code, _ := get("pay-1.shop.localhost", "/", html)
	check("browser", "быстрая фича (pay-1): сразу контент", code == 200, "%d через %s — страница «Запускаем» не понадобилась", code, fmtDur(time.Since(t0)))

	sleepEnv("slow-3")
	t0 = time.Now()
	code, body := get("slow-3.shop.localhost", "/", html)
	check("browser", "медленная фича (slow-3): первый ответ", code == 503 && strings.Contains(body, "Запускаем"),
		"%d «Запускаем slow-3…» через %s", code, fmtDur(time.Since(t0)))
	for code != 200 && time.Since(t0) < 30*time.Second {
		time.Sleep(time.Second) // the page refreshes itself every second
		code, _ = get("slow-3.shop.localhost", "/", html)
	}
	check("browser", "медленная фича: контент после автообновления", code == 200, "через %s", fmtDur(time.Since(t0)))

	fmt.Fprintln(os.Stderr, "concurrency and bodies:")
	sleepEnv("pay-1")
	wakesBefore := status()["pay-1"].Wakes
	var wg sync.WaitGroup
	var mu sync.Mutex
	okCount := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c, _ := get("pay-1.shop.localhost", "/charge", nil); c == 200 {
				mu.Lock()
				okCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	wakes := status()["pay-1"].Wakes - wakesBefore
	check("concurrency", "20 одновременных запросов к спящей фиче", okCount == 20 && wakes == 1, "%d/20 ответов 200, пробуждений: %d", okCount, wakes)

	sleepEnv("pay-1")
	payload := fmt.Sprintf("order-%d", rand.Int())
	code, body = post("pay-1.shop.localhost", "/orders", payload)
	check("concurrency", "POST к спящей фиче", code == 200 && strings.Contains(body, payload), "%d, тело дошло: %v", code, strings.Contains(body, payload))

	// Requests keep coming while the env is being put to sleep: none may hit a stopped container.
	get("pay-1.shop.localhost", "/", nil)
	total, failed := 0, 0
	var last string
	for round := 0; round < 3; round++ {
		done := make(chan struct{})
		go func() { sleepEnv("pay-1"); close(done) }()
		for loop := true; loop; {
			select {
			case <-done:
				loop = false
			default:
				c, b := get("pay-1.shop.localhost", "/charge", nil)
				total++
				if c != 200 {
					failed++
					last = fmt.Sprintf("%d %.80s", c, b)
				}
				time.Sleep(50 * time.Millisecond)
			}
		}
	}
	check("concurrency", "поток запросов во время усыпления (3 раза)", failed == 0, "%d запросов, ошибок: %d %s", total, failed, last)

	fmt.Fprintln(os.Stderr, "budget:")
	up(1)
	waitAll(true)
	sleepEnv("pay-1")
	sleepEnv("ui-2")
	sleepEnv("slow-3")
	get("pay-1.shop.localhost", "/", nil)
	time.Sleep(500 * time.Millisecond)
	get("ui-2.shop.localhost", "/", nil)
	time.Sleep(3 * time.Second)
	s = status()
	check("budget", "лимит 1 фича: разбудили pay-1, потом ui-2", s["ui-2"].Running && !s["pay-1"].Running && s["pay-1"].SleepCause == "budget",
		"ui-2 running=%v, pay-1 running=%v (причина: %s)", s["ui-2"].Running, s["pay-1"].Running, s["pay-1"].SleepCause)

	code, _ = get("shop.localhost", "/", nil)
	running, _ := run("docker", "inspect", "-f", "{{.State.Running}}", "p7-backend")
	check("budget", "main за весь прогон", code == 200 && strings.TrimSpace(running) == "true", "работает, ответ %d", code)

	fmt.Print(report())
	for _, r := range rows {
		if !r.OK {
			exit(1)
		}
	}
	exit(0)
}

func up(maxRunning int) {
	cmd := exec.Command("docker", "compose", "up", "-d", "--build", "--wait")
	cmd.Env = append(os.Environ(), "SLEEP_AFTER="+sleepAfter.String(), fmt.Sprintf("MAX_RUNNING=%d", maxRunning))
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "compose up: %v\n%s", err, out)
		os.Exit(1)
	}
	for i := 0; i < 60; i++ {
		if len(status()) == 3 {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func waitAll(running bool) {
	for i := 0; i < 60; i++ {
		s := status()
		if s["pay-1"].Running == running && s["ui-2"].Running == running && s["slow-3"].Running == running {
			return
		}
		get("pay-1.shop.localhost", "/", nil)
		get("ui-2.shop.localhost", "/", nil)
		get("slow-3.shop.localhost", "/", nil)
		time.Sleep(500 * time.Millisecond)
	}
}

func sleepEnv(env string) {
	_, _ = run("docker", "exec", "p7-waker", "wget", "-qO-", "localhost:8000/_waker/sleep?env="+env)
	// Let Envoy's health check notice, as it would after a real idle period.
	time.Sleep(2500 * time.Millisecond)
}

func status() map[string]envStatus {
	out, err := run("docker", "exec", "p7-waker", "wget", "-qO-", "localhost:8000/_waker/status")
	m := map[string]envStatus{}
	if err != nil {
		return m
	}
	var list []envStatus
	_ = json.Unmarshal([]byte(out), &list)
	for _, e := range list {
		m[e.Name] = e
	}
	return m
}

func every(d time.Duration, fn func()) (stop func()) {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(d)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				fn()
			}
		}
	}()
	return func() { close(done) }
}

func get(host, path string, headers map[string]string) (int, string) {
	return do("GET", host, path, "", headers)
}

func post(host, path, body string) (int, string) {
	return do("POST", host, path, body, map[string]string{"Content-Type": "text/plain"})
}

func do(method, host, path, body string, headers map[string]string) (int, string) {
	req, _ := http.NewRequest(method, edge+path, strings.NewReader(body))
	req.Host = host
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 70 * time.Second}).Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func memMB(prefix string) float64 {
	out, _ := run("docker", "stats", "--no-stream", "--format", "{{.Name}} {{.MemUsage}}")
	var total float64
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || !strings.HasPrefix(f[0], prefix) {
			continue
		}
		var v float64
		var unit string
		fmt.Sscanf(f[1], "%f%s", &v, &unit)
		switch unit {
		case "GiB":
			v *= 1024
		case "KiB":
			v /= 1024
		case "B":
			v /= 1024 * 1024
		}
		total += v
	}
	return total
}

func run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("%v: %s", err, errb.String())
	}
	return out.String(), nil
}

func median(ds []time.Duration) time.Duration {
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}

func maxDur(ds []time.Duration) time.Duration {
	var m time.Duration
	for _, d := range ds {
		if d > m {
			m = d
		}
	}
	return m
}

func fmtDur(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%d мс", d.Milliseconds())
	}
	return strings.Replace(fmt.Sprintf("%.1f с", d.Seconds()), ".", ",", 1)
}

func report() string {
	titles := map[string]string{
		"idle": "Активность и усыпление (sleep_after = 6 с)", "mem": "Память",
		"wake": "Пробуждение API-запросом (запрос удерживается)", "browser": "Пробуждение из браузера (страница «Запускаем» — если старт дольше 300 мс)",
		"concurrency": "Одновременные запросы и тело запроса", "budget": "Бюджет ресурсов и main",
	}
	var b strings.Builder
	for _, g := range []string{"idle", "mem", "wake", "browser", "concurrency", "budget"} {
		fmt.Fprintf(&b, "\n### %s\n\n| Проверка | Результат | Итог |\n|---|---|---|\n", titles[g])
		for _, r := range rows {
			if r.Group == g {
				mark := "✅"
				if !r.OK {
					mark = "❌"
				}
				fmt.Fprintf(&b, "| %s | %s | %s |\n", r.Name, r.Result, mark)
			}
		}
	}
	return b.String()
}
