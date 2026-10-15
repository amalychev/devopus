package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/devopus/devopus/internal/compose"
	"github.com/devopus/devopus/internal/workspace"
	"github.com/devopus/devopus/schema"
)

// SecretsFile holds local secret values in stage 1 (specs/mvp-prd.md); it stays out of git.
const SecretsFile = ".env.secrets"

var composeNames = []string{"compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml"}

func (a *app) initCmd() *cobra.Command {
	var (
		from    []string
		project string
		noPin   bool
		force   bool
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create workspace.yaml, importing docker-compose if the project has one",
		Long: "Creates workspace.yaml in the current directory (or at --file). Its folder is the\n" +
			"repository root: module paths are relative to it.\n\n" +
			"With --from-compose, or when a compose file is found here, the compose project is\n" +
			"imported and a report lists what to review. Floating image tags are pinned to\n" +
			"today's digest (needs the registry; --no-pin skips it). Values that look like\n" +
			"secrets move to " + SecretsFile + ", which must stay out of git.",
		Example: "  devopus init\n  devopus init --from-compose docker-compose.yml --from-compose docker-compose.dev.yml",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			target := a.file
			if target == "" {
				target = workspace.FileName
			}
			if _, err := os.Stat(target); err == nil && !force {
				return fail(ExitError, "%s already exists; pass --force to overwrite it", target)
			}
			if len(from) == 0 {
				for _, n := range composeNames {
					if _, err := os.Stat(n); err == nil {
						from = []string{n}
						if !a.json {
							fmt.Fprintf(a.stderr, "Importing %s (pass --from-compose to choose another file)\n", n)
						}
						break
					}
				}
			}
			if len(from) == 0 {
				return a.initTemplate(target, project)
			}
			// Paths in workspace.yaml are relative to its folder: that is the repository root.
			abs, err := filepath.Abs(target)
			if err != nil {
				return err
			}
			opt := compose.Options{Root: filepath.Dir(abs), Project: project}
			if !noPin {
				opt.Pinner = compose.RegistryPinner{}
			}
			return a.initFromCompose(cmd.Context(), target, from, opt)
		},
	}
	cmd.Flags().StringArrayVar(&from, "from-compose", nil, "compose file to import; repeat to merge overrides in order")
	cmd.Flags().StringVar(&project, "project", "", "project name (default: directory name)")
	cmd.Flags().BoolVar(&noPin, "no-pin", false, "do not resolve floating image tags to digests")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing workspace file")
	return cmd
}

func (a *app) initFromCompose(ctx context.Context, target string, files []string, opt compose.Options) error {
	if ctx == nil {
		ctx = context.Background()
	}
	res, err := compose.Import(ctx, files, opt)
	if err != nil {
		return fail(ExitInvalid, "cannot load %s: %v", strings.Join(files, ", "), err)
	}
	data, err := marshal(res.Workspace)
	if err != nil {
		return err
	}
	if err := os.WriteFile(target, data, 0o644); err != nil {
		return err
	}
	secretsNote, err := writeSecrets(filepath.Dir(target), res.Secrets)
	if err != nil {
		return err
	}
	loaded, err := workspace.Load(target)
	if err != nil {
		return err
	}
	ds := loaded.Diagnostics
	level := res.Report.Level(ds.HasErrors())

	if a.json {
		if err := a.writeJSON(map[string]any{
			"file":        target,
			"level":       level,
			"report":      res.Report,
			"diagnostics": nonNil(ds),
			"secrets":     secretsNote,
		}); err != nil {
			return err
		}
	} else {
		res.Report.Write(a.stdout, ds.HasErrors())
		if secretsNote != "" {
			fmt.Fprintln(a.stdout, "\n"+secretsNote)
		}
		fmt.Fprintln(a.stdout)
		a.printDiagnostics(target, ds)
		fmt.Fprintf(a.stdout, "Wrote %s.", display(target))
		if ds.HasErrors() {
			fmt.Fprint(a.stdout, " Fix the errors above, then run 'devopus validate'.")
		}
		fmt.Fprintln(a.stdout)
	}
	if level == "C" {
		return silent(ExitInvalid)
	}
	return nil
}

func (a *app) initTemplate(target, project string) error {
	if project == "" {
		abs, err := filepath.Abs(target)
		if err != nil {
			return err
		}
		project = filepath.Base(filepath.Dir(abs))
	}
	slug := strings.Trim(regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(strings.ToLower(project), "-"), "-")
	if len(slug) > 31 {
		slug = strings.TrimRight(slug[:31], "-")
	}
	if len(slug) < 2 || slug[0] < 'a' || slug[0] > 'z' {
		slug = "app-" + slug
	}
	data := []byte(fmt.Sprintf(template, schema.URL, slug, slug))
	if err := os.WriteFile(target, data, 0o644); err != nil {
		return err
	}
	if a.json {
		return a.writeJSON(map[string]any{"file": target, "template": true})
	}
	fmt.Fprintf(a.stdout, "Wrote %s from a template: describe your modules and services, then run 'devopus validate'.\n", target)
	return nil
}

const template = `# yaml-language-server: $schema=%s
# Devopus workspace: one file describes the project. Spec: specs/workspace-yaml.md
version: 1
project: %s
domain: %s.localhost

services:
  db:
    image: postgres:17.2
    port: 5432
    env:
      POSTGRES_DB: app
      POSTGRES_PASSWORD: ${{ secrets.POSTGRES_PASSWORD }}

modules:
  app:
    path: .
    runtime: node:22
    port: 3000
    routes: ["/"]
    uses: [db]
    env:
      DATABASE_URL: postgres://postgres:${{ secrets.POSTGRES_PASSWORD }}@${{ db.host }}:${{ db.port }}/${{ db.database }}
    hooks:
      install: npm ci
    command: npm run dev
`

// marshal renders the workspace with the schema comment for editors.
func marshal(w *workspace.Workspace) ([]byte, error) {
	return workspace.Marshal(w, fmt.Sprintf("# yaml-language-server: $schema=%s\n", schema.URL)+
		"# Imported by 'devopus init'. Review the report, then run 'devopus validate'.\n")
}

// writeSecrets adds the imported values to .env.secrets without touching keys already
// there, and checks that git ignores the file.
func writeSecrets(dir string, secrets map[string]string) (string, error) {
	if len(secrets) == 0 {
		return "", nil
	}
	path := filepath.Join(dir, SecretsFile)
	have := map[string]bool{}
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if k, _, ok := strings.Cut(sc.Text(), "="); ok {
				have[strings.TrimSpace(k)] = true
			}
		}
		f.Close()
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	var keys []string
	for k := range secrets {
		if !have[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if len(keys) > 0 {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return "", err
		}
		for _, k := range keys {
			fmt.Fprintf(f, "%s=%s\n", k, dotenvQuote(secrets[k]))
		}
		if err := f.Close(); err != nil {
			return "", err
		}
	}
	note := fmt.Sprintf("%d secret %s in %s.", len(secrets), plural(len(secrets), "value is", "values are"), path)
	if !gitIgnores(dir, SecretsFile) {
		note += " Add " + SecretsFile + " to .gitignore: it must not be committed."
	}
	return note, nil
}

func dotenvQuote(v string) string {
	if v != "" && !strings.ContainsAny(v, " \t\n#'\"\\$") {
		return v
	}
	return strconv.Quote(v)
}

// gitIgnores is a plain check of .gitignore lines; git check-ignore needs a repository.
func gitIgnores(dir, name string) bool {
	data, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		return false
	}
	for _, l := range strings.Split(string(data), "\n") {
		l = strings.TrimSpace(l)
		if l == name || l == "/"+name || l == ".env*" || l == "*.secrets" {
			return true
		}
	}
	return false
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
