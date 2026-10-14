// Command p6check is the stage-0 prototype P-6: ports 80/443, *.localhost and the local CA
// on the host and inside containers, checked with real clients (curl, Python, Node, PHP,
// Java, Go). It changes no system setting and needs no root. Throwaway code: see README.md.
package main

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

type result struct {
	Section, Name, Detail string
	OK                    bool
}

var (
	results  []result
	sections []struct{ key, title string }
)

func section(key, title string) {
	sections = append(sections, struct{ key, title string }{key, title})
	fmt.Fprintf(os.Stderr, "\n== %s ==\n", title)
}

// record notes a check. ok means "behaves as the design expects", which for some rows is a
// refusal (e.g. a certificate that must be rejected).
func record(sec, name string, ok bool, detail string) {
	results = append(results, result{sec, name, detail, ok})
	mark := "✓"
	if !ok {
		mark = "•"
	}
	fmt.Fprintf(os.Stderr, "  %s %-72s %s\n", mark, name, detail)
}

func main() {
	// Helper mode used by the Linux tests: try to listen on an address and report.
	if len(os.Args) == 2 && strings.Contains(os.Args[1], ":") {
		l, err := net.Listen("tcp", os.Args[1])
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		l.Close()
		fmt.Println("listening on", os.Args[1])
		return
	}

	dir, err := os.MkdirTemp("", "p6-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	p, err := newPKI(dir)
	if err != nil {
		panic(err)
	}
	only := ""
	if len(os.Args) > 1 {
		only = os.Args[1]
	}
	if only == "" || only == "host" {
		hostTests(p)
	}
	if only == "" || only == "containers" {
		containerTests(p)
	}
	if only == "" || only == "linux" {
		linuxTests(p)
	}
	fmt.Print(report())
}

func report() string {
	var b strings.Builder
	for _, s := range sections {
		fmt.Fprintf(&b, "\n### %s\n\n| Проверка | Результат | Как задумано |\n|---|---|---|\n", s.title)
		for _, r := range results {
			if r.Section != s.key {
				continue
			}
			mark := "✅"
			if !r.OK {
				mark = "⚠️"
			}
			fmt.Fprintf(&b, "| %s | %s | %s |\n", r.Name, strings.ReplaceAll(r.Detail, "|", "\\|"), mark)
		}
	}
	return b.String()
}

func run(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	s := strings.TrimSpace(out.String())
	if err != nil {
		if e := strings.TrimSpace(errb.String()); e != "" {
			s = strings.TrimSpace(s + " " + e)
		}
		return s, fmt.Errorf("%v", err)
	}
	return s, nil
}

func goarch() string { return runtime.GOARCH }
