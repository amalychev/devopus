package reconcile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	agentv2 "github.com/amalychev/devopus/gen/devopus/agent/v2"
	"github.com/amalychev/devopus/internal/agent/runtime"
	"github.com/amalychev/devopus/internal/agent/stack"
)

// roleCopy is the one-shot container that copies a golden copy into a feature's volume.
const roleCopy = "copy"

// dataMark is what the agent remembers about a copy of a database: in the base
// environment the golden copy, in a feature its own copy.
type dataMark struct {
	// Key identifies the golden copy: the migrations of the applications that use the
	// database. A feature keeps the key of the golden copy it came from.
	Key   string    `json:"key"`
	Reset uint64    `json:"reset"`
	At    time.Time `json:"at"`
	Size  string    `json:"size,omitempty"`
}

func (r *Reconciler) dataFile(envID string) string {
	return filepath.Join(r.opt.EnvsDir, envID, "data.json")
}

func (r *Reconciler) loadData(envID string) map[string]dataMark {
	m := map[string]dataMark{}
	if b, err := os.ReadFile(r.dataFile(envID)); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func (r *Reconciler) saveData(envID string, m map[string]dataMark) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.dataFile(envID)), 0o700); err != nil {
		return err
	}
	return os.WriteFile(r.dataFile(envID), b, 0o600)
}

// goldenKey changes when the base environment's migrations of a database change: the
// golden copy must follow them (docs/architecture/data.md).
func goldenKey(d stack.Data, hooks hookMarks) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%s\n", d.Engine, d.Image)
	uses := append([]string{}, d.Uses...)
	sort.Strings(uses)
	for _, u := range uses {
		fmt.Fprintf(h, "%s=%s\n", u, hooks[u+".migrate"])
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// ensureGolden keeps the golden copies of the base environment's databases: made when a
// feature first needs one, made again after the base's migrations change and on
// 'devopus db golden refresh'. The copy is taken from the running database without
// stopping it: pg_basebackup for Postgres, CLONE LOCAL for MySQL.
func (r *Reconciler) ensureGolden(ctx context.Context, env *agentv2.EnvironmentSpec, p *prepared) (string, error) {
	if len(p.plan.Data) == 0 {
		r.update(env.Id, func(s *agentv2.EnvironmentStatus) { s.Data = nil })
		return "no databases", nil
	}
	marks := r.loadData(env.Id)
	hooks := r.loadMarks(env.Id)
	needed := r.needGolden[env.Project]
	var made, kept, waiting []string
	for _, d := range p.plan.Data {
		key, reset := goldenKey(d, hooks), env.DataResets[d.Service]
		m, ok := marks[d.Service]
		switch {
		case ok && m.Key == key && m.Reset == reset:
			kept = append(kept, d.Service)
			continue
		case !needed[d.Service] && m.Reset == reset:
			// Nothing copies it yet: no reason to make the base wait.
			waiting = append(waiting, d.Service)
			continue
		}
		ctr, err := r.container(ctx, d.Container)
		if err != nil {
			return "", err
		}
		if ctr == nil || !ctr.Running {
			return "", fmt.Errorf("%s is not running", d.Service)
		}
		r.stepMessage(env.Id, "golden", "copying "+d.Service)
		size, err := r.goldenCopy(ctx, d, ctr.ID)
		if err != nil {
			return "", fmt.Errorf("%s: %w", d.Service, err)
		}
		marks[d.Service] = dataMark{Key: key, Reset: reset, At: r.now(), Size: size}
		if err := r.saveData(env.Id, marks); err != nil {
			return "", err
		}
		made = append(made, fmt.Sprintf("%s (%s)", d.Service, size))
	}
	r.setDataStatus(env.Id, p.plan.Data, marks, nil)
	var parts []string
	if len(made) > 0 {
		parts = append(parts, "copied "+strings.Join(made, ", "))
	}
	if len(kept) > 0 {
		parts = append(parts, "up to date: "+strings.Join(kept, ", "))
	}
	if len(waiting) > 0 {
		parts = append(parts, "not needed yet: "+strings.Join(waiting, ", "))
	}
	return strings.Join(parts, "; "), nil
}

// Scripts run in the database's own image: no extra pulls, and the engine's tools at
// hand. $1 is the data folder.
const (
	goldenPostgres = `set -e
g=` + stack.GoldenMount + `
rm -rf "$g/next"
pg_basebackup -U "${POSTGRES_USER:-postgres}" -D "$g/next" -X stream -c fast
chown -R "$(stat -c %u:%g "$1")" "$g/next"
rm -rf "$g/data"
mv "$g/next" "$g/data"
du -sh "$g/data" | cut -f1`
	goldenMySQL = `set -e
g=` + stack.GoldenMount + `
export MYSQL_PWD="${MYSQL_ROOT_PASSWORD:-}"
mkdir -p "$g"
chown mysql:mysql "$g"
rm -rf "$g/next"
mysql -uroot -e "INSTALL PLUGIN clone SONAME 'mysql_clone.so'" 2>/dev/null || true
mysql -uroot -e "CLONE LOCAL DATA DIRECTORY = '$g/next'"
rm -rf "$g/data"
mv "$g/next" "$g/data"
du -sh "$g/data" | cut -f1`
	// copyGolden checks the free space first: Postgres that runs out of it in the middle
	// of a copy fails with an obscure error (P-3). With reflink the copy is free, and the
	// check is pessimistic.
	copyGolden = `set -e
src=/golden/data
dst="/data/$1"
[ -d "$src" ] || { echo "there is no golden copy" >&2; exit 3; }
need=$(du -sk "$src" | cut -f1)
avail=$(df -Pk /data | awk 'NR==2 {print $4}')
if [ "$avail" -lt $((need + need / 10 + 65536)) ]; then
  echo "not enough disk space for a copy of $((need / 1024)) MB: $((avail / 1024)) MB free" >&2
  exit 28
fi
mkdir -p "$dst"
cp -a --reflink=auto "$src/." "$dst/" 2>/dev/null || cp -a "$src/." "$dst/"`
)

func (r *Reconciler) goldenCopy(ctx context.Context, d stack.Data, containerID string) (string, error) {
	script := goldenPostgres
	if d.Engine == stack.EngineMySQL {
		script = goldenMySQL
	}
	res, err := r.rt.Exec(ctx, containerID, "0", []string{"sh", "-c", script, "sh", d.DataDir})
	if err != nil {
		return "", err
	}
	if err := res.Err("the golden copy"); err != nil {
		return "", err
	}
	return strings.TrimSpace(runtime.LastLines(res.Stdout, 1)), nil
}

// ensureData gives a feature's databases their data: a copy of the base environment's
// golden copy, made once and again on 'devopus db reset'. The feature's migrations then
// run on its copy only.
func (r *Reconciler) ensureData(ctx context.Context, env *agentv2.EnvironmentSpec, p *prepared, o *observed) (string, error) {
	if len(p.plan.Data) == 0 {
		r.update(env.Id, func(s *agentv2.EnvironmentStatus) { s.Data = nil })
		return "no databases of its own", nil
	}
	marks := r.loadData(env.Id)
	golden := r.loadData(p.base.env.Id)
	var copied, kept []string
	for _, d := range p.plan.Data {
		reset := env.DataResets[d.Service]
		if m, ok := marks[d.Service]; ok && m.Reset == reset {
			kept = append(kept, d.Service)
			continue
		}
		g, ok := golden[d.Service]
		if !ok {
			return "", fmt.Errorf("the base environment has no golden copy of %s yet", d.Service)
		}
		r.stepMessage(env.Id, "data", fmt.Sprintf("copying %s (%s)", d.Service, g.Size))
		start := r.now()
		if err := r.copyData(ctx, env, d, o); err != nil {
			return "", fmt.Errorf("%s: %w", d.Service, err)
		}
		marks[d.Service] = dataMark{Key: g.Key, Reset: reset, At: r.now(), Size: g.Size}
		if err := r.saveData(env.Id, marks); err != nil {
			return "", err
		}
		// The feature's migrations run again on the new copy.
		hooks := r.loadMarks(env.Id)
		for _, u := range d.Uses {
			delete(hooks, u+".migrate")
		}
		if err := r.saveMarks(env.Id, hooks); err != nil {
			return "", err
		}
		copied = append(copied, fmt.Sprintf("%s (%s, %.1fs)", d.Service, g.Size, r.now().Sub(start).Seconds()))
	}
	r.setDataStatus(env.Id, p.plan.Data, marks, golden)
	var parts []string
	if len(copied) > 0 {
		parts = append(parts, "copied "+strings.Join(copied, ", "))
	}
	if len(kept) > 0 {
		parts = append(parts, "kept "+strings.Join(kept, ", "))
	}
	return strings.Join(parts, "; "), nil
}

// copyData replaces the feature's database and its volume with a copy of the golden one.
func (r *Reconciler) copyData(ctx context.Context, env *agentv2.EnvironmentSpec, d stack.Data, o *observed) error {
	ctrs := o.byEnv[env.Id][:0:0]
	for _, c := range o.byEnv[env.Id] {
		if c.Name == d.Container {
			if err := r.rt.RemoveContainer(ctx, c.ID); err != nil {
				return err
			}
			continue
		}
		ctrs = append(ctrs, c)
	}
	o.byEnv[env.Id] = ctrs
	if err := r.rt.RemoveVolume(ctx, d.Volume); err != nil {
		return err
	}
	if err := r.rt.CreateVolume(ctx, d.Volume, r.labels(env)); err != nil {
		return err
	}
	labels := r.labels(env)
	labels[runtime.LabelRole] = roleCopy
	spec := runtime.ContainerSpec{
		Name:   d.Container + "-copy",
		Image:  d.Image,
		User:   "0",
		Labels: labels,
		// The image's entrypoint would start the database.
		Entrypoint: []string{"sh", "-c", copyGolden, "sh", d.Path},
		Volumes:    []runtime.VolumeMount{{Name: d.Golden, Target: "/golden"}, {Name: d.Volume, Target: "/data"}},
	}
	if old, err := r.container(ctx, spec.Name); err != nil {
		return err
	} else if old != nil {
		if err := r.rt.RemoveContainer(ctx, old.ID); err != nil {
			return err
		}
	}
	id, err := r.rt.CreateContainer(ctx, spec)
	if err != nil {
		return err
	}
	defer func() { _ = r.rt.RemoveContainer(context.WithoutCancel(ctx), id) }()
	if err := r.rt.StartContainer(ctx, id); err != nil {
		return r.explain(ctx, id, err)
	}
	code, err := r.rt.WaitContainer(ctx, id)
	if err != nil {
		return err
	}
	if code != 0 {
		return r.explain(ctx, id, fmt.Errorf("the copy exited with %d", code))
	}
	return nil
}

// setDataStatus publishes the databases of an environment. golden is the base's marks,
// for a feature: its copy is behind when the golden copy changed since.
func (r *Reconciler) setDataStatus(envID string, data []stack.Data, marks, golden map[string]dataMark) {
	var out []*agentv2.DataStatus
	for _, d := range data {
		st := &agentv2.DataStatus{Service: d.Service, Engine: d.Engine}
		if m, ok := marks[d.Service]; ok {
			st.CopiedAt, st.Size = timestamppb.New(m.At), m.Size
			if g, ok := golden[d.Service]; ok && g.Key != m.Key {
				st.Behind = true
				st.Note = "the base environment migrated since: 'devopus db reset' makes a fresh copy"
			}
		} else if golden == nil {
			st.Note = "no golden copy yet: made when a feature needs one"
		}
		out = append(out, st)
	}
	r.update(envID, func(s *agentv2.EnvironmentStatus) { s.Data = out })
}
