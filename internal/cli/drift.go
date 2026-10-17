package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	syncv1 "github.com/amalychev/devopus/gen/devopus/sync/v1"
	workspacev1 "github.com/amalychev/devopus/gen/devopus/workspace/v1"
)

// drift is how the team's machines differ from the project's settings: an older
// version, personal overrides, overrides refused, an environment in error.
type driftRow struct {
	User, Device, Project, Environment string
	Online                             bool
	LastSeen                           time.Time
	Version, Current                   int32
	Phase, Error                       string
	Overrides, Refused                 []string
}

func (d driftRow) drifts() bool {
	return d.Version != d.Current || len(d.Overrides) > 0 || len(d.Refused) > 0 || d.Phase == "error"
}

func (a *app) driftCmd() *cobra.Command {
	var project string
	var all bool
	cmd := &cobra.Command{
		Use:   "drift",
		Short: "Where machines differ from the projects' settings: old versions, personal overrides",
		Long: "Shows, for every machine running the projects (yours, and those of the projects you\n" +
			"lead), which version of the settings it runs and the personal overrides of its\n" +
			"devopus.yaml, applied or refused. --all lists machines that do not drift too.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			s, cur, c, err := a.inWorkspace(ctx)
			if err != nil {
				return err
			}
			projects, err := c.ListProjects(ctx, req(s, &workspacev1.ListProjectsRequest{WorkspaceId: cur.ID}))
			if err != nil {
				return rpcErr(err)
			}
			current := map[string]int32{}
			for _, p := range projects.Msg.Projects {
				current[p.Name] = p.Version
			}
			sc := s.syncServer(cur.ServerURL)
			res, err := sc.ListDevices(ctx, req(s, &syncv1.ListDevicesRequest{WorkspaceId: cur.ID, Project: project}))
			if err != nil {
				return rpcErr(err)
			}
			var rows []driftRow
			for _, d := range res.Msg.Devices {
				for _, e := range d.Environments {
					env := "base"
					if e.Kind == "feature" {
						env = e.Name
					}
					r := driftRow{User: d.UserEmail, Device: d.Device.Name, Project: e.Project, Environment: env, Online: d.Online,
						LastSeen: d.LastSeen.AsTime(), Version: e.ConfigVersion, Current: current[e.Project], Phase: e.Phase, Error: e.Error,
						Overrides: e.Overrides, Refused: e.RejectedOverrides}
					if all || r.drifts() {
						rows = append(rows, r)
					}
				}
			}
			if a.json {
				return a.writeJSON(rows)
			}
			if len(rows) == 0 {
				fmt.Fprintln(a.stdout, "No drift: every machine runs the current settings without personal overrides.")
				return nil
			}
			w := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "WHO\tMACHINE\tPROJECT\tENV\tSETTINGS\tSTATE\tPERSONAL OVERRIDES")
			for _, r := range rows {
				version := fmt.Sprintf("v%d", r.Version)
				if r.Version != r.Current {
					version += fmt.Sprintf(" (current v%d)", r.Current)
				}
				state := r.Phase
				if !r.Online {
					state += ", offline since " + r.LastSeen.Local().Format("2 Jan 15:04")
				}
				var notes []string
				notes = append(notes, r.Overrides...)
				for _, x := range r.Refused {
					notes = append(notes, "refused: "+x)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.User, r.Device, r.Project, r.Environment, version, state, dash(strings.Join(notes, "; ")))
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "only this project")
	cmd.Flags().BoolVar(&all, "all", false, "machines that do not drift too")
	return cmd
}
