package compose

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// Report lists what the import did and what a human still has to look at.
type Report struct {
	Modules         int                 `json:"modules"`
	Services        int                 `json:"services"`
	Renamed         []string            `json:"renamed,omitempty"`
	Routes          []string            `json:"routes,omitempty"`       // guessed entry points
	DroppedPorts    int                 `json:"dropped_ports"`          // published ports: only the proxy is reachable
	AssumedPort     []string            `json:"assumed_port,omitempty"` // modules without a known port: 8080 assumed
	Optional        []string            `json:"optional,omitempty"`     // services behind non-default profiles
	Skipped         []string            `json:"skipped,omitempty"`      // profile-only templates without image or build
	Secrets         []string            `json:"secrets,omitempty"`      // moved to ${{ secrets.* }}
	HostVars        []string            `json:"host_vars,omitempty"`    // the project expects them from the host
	MissingEnvFiles []string            `json:"missing_env_files,omitempty"`
	CodeMounts      int                 `json:"code_mounts"`           // replaced by the worktree
	RepoMounts      []string            `json:"repo_mounts,omitempty"` // bind mounts of repository files
	HostMounts      []string            `json:"host_mounts,omitempty"` // outside the repository: dropped
	ServiceOnModule []string            `json:"service_on_module,omitempty"`
	Unsupported     map[string][]string `json:"unsupported,omitempty"`
	Pinned          []string            `json:"pinned,omitempty"`    // floating tags pinned by digest
	PinLater        []string            `json:"pin_later,omitempty"` // registry rate-limited the lookup
	Unpinned        []string            `json:"unpinned,omitempty"`
	Errors          []string            `json:"errors,omitempty"` // blocking
	NoCode          bool                `json:"no_code,omitempty"`
}

// Level grades the result as prototype P-8 did:
// A — nothing to look at; B — valid, items to review; C — blocked, needs an edit;
// N — not a target: only ready-made images, nothing to develop or fork.
func (r *Report) Level(validationErrors bool) string {
	switch {
	case r.NoCode:
		return "N"
	case len(r.Errors) > 0 || validationErrors:
		return "C"
	case len(r.reviewItems()) > 0:
		return "B"
	default:
		return "A"
	}
}

type item struct {
	title string
	lines []string
}

func (r *Report) reviewItems() []item {
	var out []item
	add := func(title string, lines []string) {
		if len(lines) > 0 {
			out = append(out, item{title, lines})
		}
	}
	add("Guessed entry points — check the routes", r.Routes)
	add("Moved to secrets — values are in .env.secrets", r.Secrets)
	add("Renamed to DNS-friendly names", r.Renamed)
	add("Optional (non-default compose profiles)", r.Optional)
	add("Skipped: templates for extends without image or build", r.Skipped)
	add("Pinned by digest", r.Pinned)
	add("Not pinned yet (registry rate limit) — run the import again later", r.PinLater)
	add("Host variables the project expects", r.HostVars)
	add("env_file not found — values not imported", r.MissingEnvFiles)
	add("Port unknown, 8080 assumed — set port", r.AssumedPort)
	add("Bind mounts of repository files", r.RepoMounts)
	add("Bind mounts outside the repository — dropped", r.HostMounts)
	add("A service depended on a module — dropped, the proxy routes HTTP", r.ServiceOnModule)
	var unsupported []string
	for _, k := range sortedKeys(r.Unsupported) {
		unsupported = append(unsupported, k+": "+strings.Join(r.Unsupported[k], ", "))
	}
	add("Not in workspace.yaml v1 — dropped", unsupported)
	return out
}

// Write prints the report for a terminal.
func (r *Report) Write(out io.Writer, validationErrors bool) {
	level := r.Level(validationErrors)
	fmt.Fprintf(out, "Imported %d modules and %d services · level %s (%s)\n", r.Modules, r.Services, level, levelText[level])
	if r.DroppedPorts > 0 || r.CodeMounts > 0 {
		fmt.Fprintf(out, "  %d published ports dropped (only the proxy is reachable), %d code mounts replaced by worktrees\n", r.DroppedPorts, r.CodeMounts)
	}
	for _, e := range r.Errors {
		fmt.Fprintf(out, "  ✗ %s\n", e)
	}
	for _, it := range r.reviewItems() {
		fmt.Fprintf(out, "\n%s:\n", it.title)
		lines := append([]string{}, it.lines...)
		sort.Strings(lines)
		for _, l := range lines {
			fmt.Fprintf(out, "  • %s\n", l)
		}
	}
}

var levelText = map[string]string{
	"A": "ready",
	"B": "ready, review the items below",
	"C": "needs a manual edit",
	"N": "only ready-made images: nothing to develop or fork",
}
