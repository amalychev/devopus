package cli

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	agentv2 "github.com/amalychev/devopus/gen/devopus/agent/v2"
	"github.com/amalychev/devopus/internal/agent/client"
	"github.com/amalychev/devopus/internal/agent/paths"
)

// expectChange tells this machine's agent what its user is changing in a server
// project, before the server has it: the version with the change runs here without
// asking them (risk R21). Without an agent, or without the project on this machine,
// there is nothing to tell.
func (a *app) expectChange(project, section string, v any) {
	var data []byte
	if v != nil {
		data = []byte(toYAML(v))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = client.New(paths.Socket()).ExpectChange(ctx, connect.NewRequest(&agentv2.ExpectChangeRequest{Project: project, Section: section, Yaml: data}))
}

type pendingChange struct {
	Project string   `json:"project"`
	Running int32    `json:"running_version"`
	Version int32    `json:"version"`
	Risky   []string `json:"risky"`
	Other   []string `json:"other"`
}

func (a *app) changesCmd() *cobra.Command {
	pending := func(project string) ([]pendingChange, *agentv2.ProjectSpec, error) {
		st, err := a.agentStatus()
		if err != nil {
			return nil, nil, err
		}
		var out []pendingChange
		for _, p := range st.Projects {
			if project != "" && p.Name != project {
				continue
			}
			if project != "" && p.Pending == nil {
				return nil, nil, fail(ExitInvalid, "nothing of %s waits for you", project)
			}
			if p.Pending != nil {
				out = append(out, pendingChange{Project: p.Name, Running: p.ConfigVersion, Version: p.Pending.Version, Risky: p.Pending.Risky, Other: p.Pending.Other})
				if project != "" {
					return out, p, nil
				}
			}
		}
		if project != "" {
			return nil, nil, fail(ExitInvalid, "no project %q on this machine", project)
		}
		return out, nil, nil
	}
	show := func(c pendingChange) {
		fmt.Fprintf(a.stdout, "%s: v%d → v%d\n", c.Project, c.Running, c.Version)
		fmt.Fprintln(a.stdout, "  New code on this machine:")
		for _, x := range c.Risky {
			fmt.Fprintf(a.stdout, "    %s\n", x)
		}
		if len(c.Other) > 0 {
			fmt.Fprintln(a.stdout, "  Also:")
			for _, x := range c.Other {
				fmt.Fprintf(a.stdout, "    %s\n", x)
			}
		}
	}
	cmd := &cobra.Command{
		Use:   "changes",
		Short: "Changes of the projects' settings that wait for you: new images, commands, hooks",
		Long: "A change that makes this machine run new code — an image outside the catalog, a\n" +
			"command, a hook, a build, a folder of this machine, another repository, a new\n" +
			"application — waits for you: the project keeps its version until you accept it.\n" +
			"Your own changes from this machine, versions of catalog services, variables and\n" +
			"routes apply at once. Accept what you expect; ask your teamlead about the rest.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			list, _, err := pending("")
			if err != nil {
				return err
			}
			if a.json {
				if list == nil {
					list = []pendingChange{}
				}
				return a.writeJSON(list)
			}
			if len(list) == 0 {
				fmt.Fprintln(a.stdout, "Nothing waits for you.")
				return nil
			}
			for i, c := range list {
				if i > 0 {
					fmt.Fprintln(a.stdout)
				}
				show(c)
			}
			fmt.Fprintln(a.stdout, "\n'devopus changes accept <project>' applies a version; 'devopus changes decline <project>' keeps yours.")
			return nil
		},
	}
	review := func(accept bool) *cobra.Command {
		use, short := "accept <project>", "Apply the version that waits for you"
		if !accept {
			use, short = "decline <project>", "Keep the version you run; the declined one is not asked about again"
		}
		return &cobra.Command{
			Use:   use,
			Short: short,
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				list, p, err := pending(args[0])
				if err != nil {
					return err
				}
				ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
				defer cancel()
				if _, err := client.New(paths.Socket()).ReviewChanges(ctx, connect.NewRequest(&agentv2.ReviewChangesRequest{Project: p.Name, Hash: p.Pending.Hash, Accept: accept})); err != nil {
					return rpcError(err)
				}
				if a.json {
					return a.writeJSON(map[string]any{"project": p.Name, "version": p.Pending.Version, "accepted": accept})
				}
				if !accept {
					fmt.Fprintf(a.stdout, "%s keeps v%d; v%d is declined. A newer version is asked about again.\n", p.Name, p.ConfigVersion, p.Pending.Version)
					return nil
				}
				show(list[0])
				fmt.Fprintf(a.stdout, "Accepted: %s runs v%d.\n", p.Name, p.Pending.Version)
				return nil
			},
		}
	}
	cmd.AddCommand(review(true), review(false))
	return cmd
}
