package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	agentv2 "github.com/amalychev/devopus/gen/devopus/agent/v2"
	"github.com/amalychev/devopus/internal/agent/client"
	"github.com/amalychev/devopus/internal/agent/paths"
)

func (a *app) dbCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "db",
		Short: "Databases of features: golden copies, resets, lag behind main",
	}
	golden := &cobra.Command{Use: "golden", Short: "Golden copies of the base environment's databases"}
	golden.AddCommand(a.dbGoldenRefreshCmd())
	cmd.AddCommand(a.dbResetCmd(), golden, a.dbLagCmd())
	return cmd
}

// resetData asks for new copies and waits until the environment runs with them.
func (a *app) resetData(project, env string, services []string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c := client.New(paths.Socket())
	if err := client.Ping(ctx, c); err != nil {
		return fail(ExitError, "%v", err)
	}
	w, err := watch(ctx, c)
	if err != nil {
		return err
	}
	defer w.Close()
	res, err := c.ResetData(ctx, connect.NewRequest(&agentv2.ResetDataRequest{Project: project, Environment: env, Services: services}))
	if err != nil {
		return rpcError(err)
	}
	id := res.Msg.EnvironmentId
	final, err := a.follow(w, []string{id}, res.Msg.Revision, true, func(s *agentv2.EnvironmentStatus) bool { return s.Phase == agentv2.Phase_PHASE_RUNNING })
	if err != nil {
		return err
	}
	if a.json {
		return a.writeProto(final[id])
	}
	what := "Golden copies"
	if env != "" {
		what = "Feature " + env + " has fresh copies of"
	} else {
		what += " of"
	}
	fmt.Fprintf(a.stdout, "%s %s are ready.\n", what, strings.Join(res.Msg.Services, ", "))
	return nil
}

func (a *app) dbResetCmd() *cobra.Command {
	var project string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "reset <feature> [service]...",
		Short: "Recreate a feature's databases from the golden copies of main",
		Long: "Throws away the feature's data and copies the golden copy of main again; the feature's\n" +
			"migrations then run on the new copy. Without services: every database of the feature.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			project, err := a.project(project)
			if err != nil {
				return err
			}
			return a.resetData(project, args[0], args[1:], timeout)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name (default: from workspace.yaml)")
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Minute, "how long to wait")
	return cmd
}

func (a *app) dbGoldenRefreshCmd() *cobra.Command {
	var project string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "refresh [service]...",
		Short: "Take new golden copies of main's databases",
		Long: "The agent copies main's databases after their migrations change; refresh takes a new\n" +
			"copy now, with the data main has. Features keep their copies until 'devopus db reset'.",
		RunE: func(cmd *cobra.Command, args []string) error {
			project, err := a.project(project)
			if err != nil {
				return err
			}
			return a.resetData(project, "", args, timeout)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name (default: from workspace.yaml)")
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Minute, "how long to wait")
	return cmd
}

func (a *app) dbLagCmd() *cobra.Command {
	var project string
	cmd := &cobra.Command{
		Use:   "lag",
		Short: "Show which features' databases are behind main's golden copies",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			project, err := a.project(project)
			if err != nil {
				return err
			}
			st, err := a.agentStatus()
			if err != nil {
				return err
			}
			var envs []*agentv2.EnvironmentStatus
			for _, e := range st.Environments {
				if e.Project == project && len(e.Data) > 0 {
					envs = append(envs, e)
				}
			}
			// main first: the features' copies come from it.
			sort.SliceStable(envs, func(i, j int) bool { return envs[i].Kind < envs[j].Kind })
			if a.json {
				return a.writeProto(&agentv2.StatusResponse{Environments: envs})
			}
			if len(envs) == 0 {
				fmt.Fprintf(a.stdout, "%s has no copied databases.\n", project)
				return nil
			}
			w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ENV\tDATABASE\tENGINE\tCOPIED\tSIZE\tSTATE")
			behind := 0
			for _, e := range envs {
				name := "main (golden)"
				if e.Kind == agentv2.EnvironmentKind_ENVIRONMENT_KIND_FEATURE {
					name = e.Name
				}
				for _, d := range e.Data {
					copied, state := "-", "current"
					if d.CopiedAt != nil {
						copied = d.CopiedAt.AsTime().Local().Format("2006-01-02 15:04")
					}
					switch {
					case d.Behind:
						state = "behind main"
						behind++
					case d.Note != "":
						state = d.Note
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", name, d.Service, d.Engine, copied, dash(d.Size), state)
				}
			}
			w.Flush()
			if behind > 0 {
				fmt.Fprintln(a.stdout, "\nmain migrated since those copies were made: 'devopus db reset <feature>' copies again.")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project name (default: from workspace.yaml)")
	return cmd
}
