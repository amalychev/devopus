// Command waker is the agent part of prototype P-7: it puts idle features to sleep and wakes
// them on the first request. Envoy sends a feature's traffic here only while the feature's
// own cluster has no healthy host (aggregate cluster failover), so no route ever changes.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Env struct {
	Name       string   `json:"name"`
	Host       string   `json:"host"`
	Containers []string `json:"containers"` // start order; stopped in reverse
	Clusters   []string `json:"clusters"`   // Envoy clusters of the env's own modules
	Worktree   string   `json:"worktree"`
	Upstreams  []string `json:"upstreams"` // module address(es), pinned IP:port

	healthy  bool          // what the waker reports to Envoy's health check
	stopping chan struct{} // closed when the current sleep finishes

	mu         sync.Mutex
	running    bool
	lastActive time.Time
	lastRq     map[string]int
	lastMtime  time.Time
	waking     chan struct{} // closed when the current wake finishes
	Wakes      int           `json:"wakes"`
	Sleeps     int           `json:"sleeps"`
	LastWakeMs int64         `json:"last_wake_ms"`
	StartMs    int64         `json:"start_ms"` // docker start of all containers, part of LastWakeMs
	SleepCause string        `json:"sleep_cause"`
	WakeErr    string        `json:"wake_error,omitempty"`
}

var (
	envs       []*Env
	sleepAfter time.Duration
	budget     int
	envoyAdmin = "http://envoy:9901"
	envoyEdge  = "http://envoy:8080"
)

func main() {
	data, err := os.ReadFile(getenv("ENVS_FILE", "/etc/waker/envs.json"))
	if err != nil {
		log.Fatal(err)
	}
	if err := json.Unmarshal(data, &envs); err != nil {
		log.Fatal(err)
	}
	sleepAfter, _ = time.ParseDuration(getenv("SLEEP_AFTER", "2h"))
	budget, _ = strconv.Atoi(getenv("MAX_RUNNING", "0"))
	for _, e := range envs {
		e.running = containersRunning(e)
		e.healthy = e.running
		e.lastActive = time.Now()
		e.lastRq = map[string]int{}
	}
	go monitor()

	http.HandleFunc("/_waker/status", status)
	http.HandleFunc("/_waker/health/", func(w http.ResponseWriter, r *http.Request) {
		e := byName(strings.TrimPrefix(r.URL.Path, "/_waker/health/"))
		if e == nil {
			http.NotFound(w, r)
			return
		}
		e.mu.Lock()
		ok := e.healthy
		e.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	http.HandleFunc("/_waker/sleep", func(w http.ResponseWriter, r *http.Request) {
		if e := byName(r.URL.Query().Get("env")); e != nil {
			sleep(e, "manual")
		}
		status(w, r)
	})
	http.HandleFunc("/", wakeAndServe)
	log.Printf("waker: %d envs, sleep after %s, max running %d", len(envs), sleepAfter, budget)
	log.Fatal(http.ListenAndServe(":8000", nil))
}

// wakeAndServe handles a request Envoy could not route because the feature sleeps.
// Browsers get a "starting" page that refreshes itself; everything else is held until the
// feature is healthy in Envoy and then replayed through Envoy, body included.
func wakeAndServe(w http.ResponseWriter, r *http.Request) {
	e := byHost(r.Host)
	if e == nil {
		http.Error(w, "unknown environment "+r.Host, http.StatusNotFound)
		return
	}
	w.Header().Set("X-Devopus-Waker", "1")
	if r.Header.Get("X-Devopus-Probe") != "" {
		http.Error(w, "probe reached the waker", http.StatusServiceUnavailable)
		return
	}
	done := wake(e)

	browser := r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html")
	if browser {
		select {
		case <-done:
		case <-time.After(300 * time.Millisecond):
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Retry-After", "1")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, startingPage, e.Name, e.Name)
			return
		}
	} else {
		select {
		case <-done:
		case <-time.After(60 * time.Second):
			http.Error(w, e.Name+" did not start within 60s", http.StatusGatewayTimeout)
			return
		case <-r.Context().Done():
			return
		}
	}
	if e.WakeErr != "" {
		http.Error(w, "could not start "+e.Name+": "+e.WakeErr, http.StatusBadGateway)
		return
	}
	// Replay straight to the module Envoy named in the route, not back through Envoy:
	// Envoy's workers switch back to the module at slightly different moments, and a replay
	// through the edge can land on the waker again or on the container's old IP.
	upstream := r.Header.Get("X-Devopus-Upstream")
	if upstream == "" {
		http.Error(w, "route has no x-devopus-upstream", http.StatusBadGateway)
		return
	}
	target, _ := url.Parse("http://" + upstream)
	proxy := httputil.NewSingleHostReverseProxy(target)
	director := proxy.Director
	proxy.Director = func(req *http.Request) {
		director(req)
		req.Host = r.Host
		req.Header.Del("X-Devopus-Upstream")
	}
	proxy.ServeHTTP(w, r)
}

const startingPage = `<!doctype html><meta charset="utf-8"><meta http-equiv="refresh" content="1">
<title>Запускаем %s…</title>
<body style="font-family:system-ui;display:grid;place-items:center;height:90vh">
<div><h1>Запускаем %s…</h1><p>Окружение спало. Страница обновится сама, когда оно будет готово.</p></div>`

// wake starts the env once, however many requests arrive while it starts.
func wake(e *Env) <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastActive = time.Now()
	stopping := e.stopping
	if e.waking != nil {
		return e.waking
	}
	ch := make(chan struct{})
	if e.running && e.healthy && stopping == nil {
		close(ch)
		return ch
	}
	e.waking = ch
	go func() {
		if stopping != nil {
			<-stopping // a request arrived while the env was going to sleep: let it finish, then start again
		}
		t0 := time.Now()
		err := startEnv(e)
		e.mu.Lock()
		e.waking = nil
		e.WakeErr = ""
		if err != nil {
			e.WakeErr = err.Error()
		} else {
			e.running = true
			e.healthy = true
			e.Wakes++
			e.LastWakeMs = time.Since(t0).Milliseconds()
			e.lastActive = time.Now()
		}
		e.mu.Unlock()
		log.Printf("wake %s: %s err=%v", e.Name, time.Since(t0).Round(time.Millisecond), err)
		close(ch)
		enforceBudget(e)
	}()
	return ch
}

func startEnv(e *Env) error {
	t0 := time.Now()
	for _, c := range e.Containers {
		if out, err := exec.Command("docker", "start", c).CombinedOutput(); err != nil {
			return fmt.Errorf("docker start %s: %s", c, out)
		}
	}
	e.mu.Lock()
	e.StartMs = time.Since(t0).Milliseconds()
	e.mu.Unlock()
	// Ready when every module accepts connections. Held requests go straight to the
	// module; Envoy picks the env up again on its next health check.
	deadline := time.Now().Add(55 * time.Second)
	for _, u := range e.Upstreams {
		for {
			if c, err := net.DialTimeout("tcp", u, 200*time.Millisecond); err == nil {
				c.Close()
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%s does not accept connections", u)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}
	return nil
}

// sleep first tells Envoy the env is unhealthy and waits until its traffic goes to the
// waker, and only then stops the containers: no request ever reaches a stopped container.
func sleep(e *Env, cause string) {
	e.mu.Lock()
	if !e.running || e.waking != nil || e.stopping != nil {
		e.mu.Unlock()
		return
	}
	done := make(chan struct{})
	e.stopping = done
	e.healthy = false
	e.Sleeps++
	e.SleepCause = cause
	e.mu.Unlock()

	t0 := time.Now()
	for time.Since(t0) < 5*time.Second && routedToEnv(e) {
		time.Sleep(50 * time.Millisecond)
	}
	drained := time.Since(t0)
	for i := len(e.Containers) - 1; i >= 0; i-- {
		_ = exec.Command("docker", "stop", "-t", "2", e.Containers[i]).Run()
	}
	e.mu.Lock()
	e.running = false
	e.stopping = nil
	e.mu.Unlock()
	close(done)
	log.Printf("sleep %s: %s (traffic switched to waker in %s, stopped in %s)", e.Name, cause,
		drained.Round(time.Millisecond), time.Since(t0).Round(time.Millisecond))
}

// enforceBudget puts the least recently active features to sleep when too many run.
// The base environment is not managed by the waker at all, so it never sleeps.
func enforceBudget(woken *Env) {
	if budget <= 0 {
		return
	}
	var running []*Env
	for _, e := range envs {
		e.mu.Lock()
		if e.running && e != woken {
			running = append(running, e)
		}
		e.mu.Unlock()
	}
	sort.Slice(running, func(i, j int) bool { return running[i].lastActive.Before(running[j].lastActive) })
	for len(running)+1 > budget {
		sleep(running[0], "budget")
		running = running[1:]
	}
}

// monitor marks activity from Envoy request counters and worktree changes, and sleeps
// envs idle for longer than SLEEP_AFTER.
func monitor() {
	for range time.Tick(time.Second) {
		stats := envoyStats()
		for _, e := range envs {
			actual := containersRunning(e) // observe, don't trust the cached flag
			e.mu.Lock()
			if e.waking == nil && e.stopping == nil {
				e.running = actual
				e.healthy = actual // a crashed module turns unhealthy, and the next request restarts it
			}
			for _, c := range e.Clusters {
				n := stats["cluster."+c+".upstream_rq_total"]
				if n != e.lastRq[c] {
					e.lastRq[c] = n
					e.lastActive = time.Now()
				}
			}
			if m := newestMtime(e.Worktree); m.After(e.lastMtime) {
				if !e.lastMtime.IsZero() {
					e.lastActive = time.Now()
				}
				e.lastMtime = m
			}
			idle := e.running && e.waking == nil && e.stopping == nil && time.Since(e.lastActive) > sleepAfter
			e.mu.Unlock()
			if idle {
				sleep(e, "idle")
			}
		}
	}
}

var statRe = regexp.MustCompile(`^(cluster\.[^:]+\.upstream_rq_total): (\d+)$`)

func envoyStats() map[string]int {
	out := map[string]int{}
	resp, err := http.Get(envoyAdmin + "/stats?filter=upstream_rq_total")
	if err != nil {
		return out
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	for _, line := range strings.Split(string(body), "\n") {
		if m := statRe.FindStringSubmatch(line); m != nil {
			out[m[1]], _ = strconv.Atoi(m[2])
		}
	}
	return out
}

// routedToEnv checks end to end that Envoy sends the env's traffic to the env again, not
// to the waker. Reading health flags from /clusters is not enough: the aggregate cluster
// switches back a moment after the host turns healthy, and a replay sent in that gap
// would loop back here.
func routedToEnv(e *Env) bool {
	req, _ := http.NewRequest(http.MethodHead, envoyEdge+"/", nil)
	req.Host = e.Host
	req.Header.Set("X-Devopus-Probe", "1")
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	// Any answer from the module counts, even 501 for HEAD: only who answered matters.
	return resp.Header.Get("X-Devopus-Waker") == ""
}

var skipDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true}

func newestMtime(root string) time.Time {
	var newest time.Time
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && skipDirs[d.Name()] {
			return filepath.SkipDir
		}
		if info, err := d.Info(); err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	return newest
}

func containersRunning(e *Env) bool {
	for _, c := range e.Containers {
		out, err := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", c).Output()
		if err != nil || strings.TrimSpace(string(out)) != "true" {
			return false
		}
	}
	return true
}

func status(w http.ResponseWriter, _ *http.Request) {
	type view struct {
		Name       string `json:"name"`
		Running    bool   `json:"running"`
		IdleMs     int64  `json:"idle_ms"`
		Wakes      int    `json:"wakes"`
		Sleeps     int    `json:"sleeps"`
		LastWakeMs int64  `json:"last_wake_ms"`
		StartMs    int64  `json:"start_ms"`
		SleepCause string `json:"sleep_cause"`
	}
	var out []view
	for _, e := range envs {
		e.mu.Lock()
		out = append(out, view{e.Name, e.running, time.Since(e.lastActive).Milliseconds(), e.Wakes, e.Sleeps, e.LastWakeMs, e.StartMs, e.SleepCause})
		e.mu.Unlock()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func byHost(host string) *Env {
	host, _, _ = strings.Cut(host, ":")
	for _, e := range envs {
		if e.Host == host {
			return e
		}
	}
	return nil
}

func byName(name string) *Env {
	for _, e := range envs {
		if e.Name == name {
			return e
		}
	}
	return nil
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
