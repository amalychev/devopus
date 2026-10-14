// Command p5check is the stage-0 prototype P-5: does Contextual Isolation work, i.e. does an
// unchanged main backend called from a feature reach the feature's fork through W3C baggage?
// It starts compose.yaml, runs the checks and prints a markdown report. Throwaway code.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

const edge = "http://127.0.0.1:18090"

type check struct {
	Group  string
	Name   string
	Want   string
	Got    string
	OK     bool
	Note   string
	Expect bool // false: the check documents a known gap (context is expected to be lost)
}

var checks []check

func record(group, name, want, got, note string, expectMatch bool) {
	ok := (got == want) == expectMatch
	checks = append(checks, check{group, name, want, got, ok, note, expectMatch})
	mark := "✓"
	if !ok {
		mark = "✗"
	}
	fmt.Fprintf(os.Stderr, "  %s %-55s want %-8s got %s\n", mark, name, want, got)
}

func main() {
	keep := flag.Bool("keep", false, "leave the stand running")
	flag.Parse()

	fmt.Fprintln(os.Stderr, "starting stand…")
	if _, err := run("docker", "compose", "up", "-d", "--build", "--wait"); err != nil {
		fail(err)
	}
	if !*keep {
		defer run("docker", "compose", "down", "--remove-orphans")
	}
	waitReady()

	envs := []string{"main", "pay-123", "pay-124"}
	host := func(env string) string {
		if env == "main" {
			return "shop.localhost"
		}
		return env + ".shop.localhost"
	}

	fmt.Fprintln(os.Stderr, "sync calls through an unchanged main backend:")
	entries := []struct{ name, path string }{
		{"Node, http.get", "/api/node/checkout"},
		{"Node, fetch", "/api/node/checkout-fetch"},
		{"Python, Flask + requests", "/api/python/checkout"},
		{"PHP, Slim + Guzzle", "/api/php/checkout"},
	}
	for _, e := range entries {
		for _, env := range envs {
			r := get(host(env), e.path, nil)
			record("sync", fmt.Sprintf("%s → payments (%s)", e.name, env), env, instance(r), "", true)
		}
	}

	fmt.Fprintln(os.Stderr, "known gap: uninstrumented client:")
	for _, env := range envs[1:] {
		r := get(host(env), "/api/python/checkout-raw", nil)
		record("gap", fmt.Sprintf("Python, http.client → payments (%s)", env), env, instance(r),
			"http.client не инструментирован: контекст теряется, вызов уходит в main", false)
	}

	fmt.Fprintln(os.Stderr, "edge does not trust client baggage:")
	record("edge", "main host + client baggage feature=pay-123", "main",
		instance(get(host("main"), "/api/python/checkout", map[string]string{"baggage": "feature=pay-123"})), "", true)
	record("edge", "pay-123 host + client baggage feature=pay-124", "pay-123",
		instance(get(host("pay-123"), "/api/python/checkout", map[string]string{"baggage": "feature=pay-124"})), "", true)

	fmt.Fprintln(os.Stderr, "mesh parses W3C baggage strictly:")
	for _, c := range []struct{ baggage, want string }{
		{"feature=pay-123", "pay-123"},
		{"other=1, feature=pay-124;prop=x", "pay-124"},
		{" feature = pay-123 ", "pay-123"},
		{"feature=pay-1234", "main"},
		{"featurex=pay-123", "main"},
		{"feature=unknown", "main"},
		{"", "main"},
	} {
		record("mesh", fmt.Sprintf("baggage %q", c.baggage), c.want, meshCall(c.baggage), "", true)
	}

	fmt.Fprintln(os.Stderr, "async: queue + workers:")
	for _, c := range []struct{ env, worker, payments string }{
		{"main", "main", "main"},
		{"pay-123", "pay-123", "pay-123"}, // forks the worker: own queue jobs:pay-123
		{"pay-124", "main", "pay-124"},    // no worker fork: main worker, context from the message
	} {
		job := get(host(c.env), "/api/python/order", nil)
		res := waitJob(host(c.env), str(job["job"]))
		record("queue", fmt.Sprintf("order in %s: worker (queue %s)", c.env, str(job["queue"])), c.worker, str(res["worker"]), "", true)
		p, _ := res["payments"].(map[string]any)
		record("queue", fmt.Sprintf("order in %s: worker → payments", c.env), c.payments, str(p["instance"]), "", true)
	}

	fmt.Fprintln(os.Stderr, "latency of the extra Envoy hop…")
	lat := latency()

	fmt.Print(report(lat))
	for _, c := range checks {
		if !c.OK {
			os.Exit(1)
		}
	}
}

func waitReady() {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		ready := true
		for _, p := range []string{"/api/node/checkout", "/api/python/checkout", "/api/php/checkout"} {
			if r := get("shop.localhost", p, nil); instance(r) != "main" {
				ready = false
			}
		}
		if ready {
			return
		}
		time.Sleep(time.Second)
	}
	fail(fmt.Errorf("stand is not ready after 90s"))
}

func get(host, path string, headers map[string]string) map[string]any {
	req, _ := http.NewRequest("GET", edge+path, nil)
	req.Host = host
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if json.Unmarshal(body, &out) != nil {
		return map[string]any{"error": fmt.Sprintf("%d %s", resp.StatusCode, body)}
	}
	return out
}

func instance(r map[string]any) string {
	if p, ok := r["payments"].(map[string]any); ok {
		return str(p["instance"])
	}
	if e, ok := r["error"]; ok {
		return "error: " + str(e)
	}
	return "?"
}

func str(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

func waitJob(host, id string) map[string]any {
	for i := 0; i < 50; i++ {
		r := get(host, "/api/python/job/"+id, nil)
		if r["pending"] != true {
			return r
		}
		time.Sleep(100 * time.Millisecond)
	}
	return map[string]any{"worker": "timeout"}
}

// meshCall sends a request with the given baggage straight to the logical name, from inside
// the network, the way a main module would.
func meshCall(baggage string) string {
	script := fmt.Sprintf(`import json, urllib.request
req = urllib.request.Request("http://payments/payments/charge", headers={"baggage": %q} if %q else {})
print(json.load(urllib.request.urlopen(req, timeout=5))["instance"])`, baggage, baggage)
	out, err := run("docker", "compose", "exec", "-T", "backend-python", "python", "-c", script)
	if err != nil {
		return "error"
	}
	return strings.TrimSpace(out)
}

type latencyResult struct{ DirectP50, DirectP99, ViaP50, ViaP99 float64 }

// latency compares 1000 calls straight to the fork container with 1000 calls through
// Envoy's logical name (baggage regex evaluated on every call). Plain urllib, keep-alive off.
func latency() latencyResult {
	script := `import json, time, urllib.request
def bench(url, headers, n=1000):
    xs = []
    for _ in range(n):
        t = time.perf_counter()
        urllib.request.urlopen(urllib.request.Request(url, headers=headers), timeout=5).read()
        xs.append((time.perf_counter() - t) * 1000)
    xs.sort()
    return xs[len(xs) // 2], xs[int(len(xs) * 0.99)]
bench("http://payments-pay-123/x", {}, 100)
d = bench("http://payments-pay-123/payments/charge", {})
v = bench("http://payments/payments/charge", {"baggage": "other=1, feature=pay-123"})
print(json.dumps({"DirectP50": d[0], "DirectP99": d[1], "ViaP50": v[0], "ViaP99": v[1]}))`
	out, err := run("docker", "compose", "exec", "-T", "backend-python", "python", "-c", script)
	var r latencyResult
	if err != nil || json.Unmarshal([]byte(strings.TrimSpace(out)), &r) != nil {
		fmt.Fprintln(os.Stderr, "latency:", err, out)
	}
	return r
}

func report(l latencyResult) string {
	var b strings.Builder
	titles := map[string]string{
		"sync":  "Синхронные вызовы через неизменённый бэкенд main",
		"gap":   "Известный пробел: неинструментированный клиент",
		"edge":  "Вход: заголовок клиента не учитывается",
		"mesh":  "Внутренняя маршрутизация: разбор baggage",
		"queue": "Очередь и воркеры",
	}
	for _, g := range []string{"sync", "gap", "edge", "mesh", "queue"} {
		fmt.Fprintf(&b, "\n### %s\n\n| Проверка | Ожидалось | Получено | Итог |\n|---|---|---|---|\n", titles[g])
		for _, c := range checks {
			if c.Group != g {
				continue
			}
			res := "✅"
			if !c.OK {
				res = "❌"
			}
			want := c.Want
			if !c.Expect {
				want = "не " + c.Want + " (потеря контекста)"
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", c.Name, want, c.Got, res)
		}
	}
	fmt.Fprintf(&b, "\n### Задержка лишнего прохода через Envoy (1000 запросов, без keep-alive)\n\n")
	fmt.Fprintf(&b, "| | p50 | p99 |\n|---|---|---|\n")
	fmt.Fprintf(&b, "| Напрямую в контейнер форка | %.2f мс | %.2f мс |\n", l.DirectP50, l.DirectP99)
	fmt.Fprintf(&b, "| Через Envoy по логическому имени + разбор baggage | %.2f мс | %.2f мс |\n", l.ViaP50, l.ViaP99)
	fmt.Fprintf(&b, "| Разница | %.2f мс | %.2f мс |\n", l.ViaP50-l.DirectP50, l.ViaP99-l.DirectP99)
	return b.String()
}

func run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
