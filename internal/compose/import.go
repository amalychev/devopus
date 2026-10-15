// Package compose converts between docker-compose files and workspace.yaml v1.
// Import follows the rules of specs/workspace-yaml.md checked by prototype P-8.
package compose

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"

	"github.com/compose-spec/compose-go/v2/dotenv"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/sirupsen/logrus"

	"github.com/devopus/devopus/internal/workspace"
)

// Options of an import.
type Options struct {
	// Root is the repository root; module paths and bind mounts are relative to it.
	// Defaults to the directory of the first compose file.
	Root string
	// Project overrides the project name derived from the directory.
	Project string
	// Pinner resolves floating image tags to digests; nil leaves them unpinned.
	Pinner Pinner
}

// Result of an import: a draft that may still need review, and what to review.
type Result struct {
	Workspace *workspace.Workspace
	// Secrets are values moved out of env into ${{ secrets.NAME }}, for .env.secrets.
	Secrets map[string]string
	Report  *Report
}

var requiredVar = regexp.MustCompile(`required variable (\S+) is missing a value`)

// Import reads compose files (later files override earlier ones, as with docker compose -f)
// and maps them to a workspace draft. The error is set only when the files cannot be loaded.
func Import(ctx context.Context, files []string, opt Options) (*Result, error) {
	if len(files) == 0 {
		return nil, errors.New("no compose files")
	}
	files = slices.Clone(files)
	for i, f := range files {
		abs, err := filepath.Abs(f)
		if err != nil {
			return nil, err
		}
		files[i] = abs
	}
	dir := filepath.Dir(files[0])
	if opt.Root == "" {
		opt.Root = dir
	}
	root, err := filepath.Abs(opt.Root)
	if err != nil {
		return nil, err
	}
	name := opt.Project
	if name == "" {
		name = filepath.Base(dir)
	}
	rep := &Report{Unsupported: map[string][]string{}}
	defer captureUnsetVars(rep)()

	// docker compose reads the project's .env for interpolation; the loader alone does not.
	env := map[string]string{}
	if dot, err := dotenv.GetEnvFromFile(nil, []string{filepath.Join(dir, ".env")}); err == nil {
		for k, v := range dot {
			env[k] = v
		}
	}
	details := types.ConfigDetails{WorkingDir: dir, Environment: env}
	for _, f := range files {
		details.ConfigFiles = append(details.ConfigFiles, types.ConfigFile{Filename: f})
	}
	load := func(profiles []string) (*types.Project, error) {
		return loader.LoadWithContext(ctx, details, func(o *loader.Options) {
			o.SetProjectName(projectSlug(name), true)
			o.ResolvePaths = true
			o.Profiles = profiles
			o.SkipConsistencyCheck = len(profiles) > 0
			// env_file is read below: repositories ship .env examples rather than .env
			// files, and a missing one must not stop the import.
			o.SkipResolveEnvironment = true
		})
	}

	var active *types.Project
	for range 100 {
		if active, err = load(nil); err == nil {
			break
		}
		// ${VAR:?} the project expects from the host: keep it as a host variable.
		m := requiredVar.FindStringSubmatch(err.Error())
		if m == nil {
			return nil, err
		}
		env[m[1]] = "${" + m[1] + "}"
		rep.HostVars = appendUnique(rep.HostVars, m[1])
	}
	if err != nil {
		return nil, err
	}
	all := active
	if p, err := load([]string{"*"}); err == nil {
		all = p
	}
	return convert(ctx, active, all, root, name, opt, rep), nil
}

// environment merges env_file entries that exist with the service's environment.
func environment(svc types.ServiceConfig, root string, rep *Report) map[string]*string {
	out := map[string]*string{}
	for _, f := range svc.EnvFiles {
		vals, err := dotenv.GetEnvFromFile(nil, []string{f.Path})
		if err != nil {
			if _, statErr := os.Stat(f.Path); statErr != nil {
				rep.MissingEnvFiles = appendUnique(rep.MissingEnvFiles, rel(root, f.Path))
			}
			continue
		}
		for k, v := range vals {
			out[k] = &v
		}
	}
	for k, v := range svc.Environment {
		out[k] = v
	}
	return out
}

var unsetVar = regexp.MustCompile(`The "([^"]+)" variable is not set`)

// captureUnsetVars turns compose-go's warnings about variables the host does not set
// into report items instead of log lines; the returned func restores the logger.
func captureUnsetVars(rep *Report) func() {
	log := logrus.StandardLogger()
	out, hooks := log.Out, log.ReplaceHooks(logrus.LevelHooks{})
	log.SetOutput(io.Discard)
	log.AddHook(hookFunc(func(e *logrus.Entry) {
		if m := unsetVar.FindStringSubmatch(e.Message); m != nil {
			rep.HostVars = appendUnique(rep.HostVars, m[1])
		}
	}))
	return func() {
		log.ReplaceHooks(hooks)
		log.SetOutput(out)
	}
}

type hookFunc func(*logrus.Entry)

func (hookFunc) Levels() []logrus.Level       { return []logrus.Level{logrus.WarnLevel} }
func (f hookFunc) Fire(e *logrus.Entry) error { f(e); return nil }
