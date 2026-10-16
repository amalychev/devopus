package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	goruntime "runtime"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	agentv1 "github.com/devopus/devopus/gen/devopus/agent/v1"
	"github.com/devopus/devopus/internal/agent/paths"
)

func (a *app) uiCmd() *cobra.Command {
	var noOpen bool
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Open the local panel: environments, progress, logs, features",
		Long: "Opens the local panel in the browser, signed in. The link carries a token that\n" +
			"only works on this device; the panel listens on 127.0.0.1 only.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			c, err := a.connect(ctx)
			if err != nil {
				return err
			}
			res, err := c.Status(ctx, connect.NewRequest(&agentv1.StatusRequest{}))
			if err != nil {
				return rpcError(err)
			}
			base := res.Msg.PanelUrl
			if base == "" {
				return fail(ExitError, "this agent (%s) has no panel; restart it: pkill -f 'devopus agent run'", res.Msg.AgentVersion)
			}
			token, err := os.ReadFile(paths.PanelToken())
			if err != nil {
				return fail(ExitError, "read the panel token: %v", err)
			}
			link := base + "/auth?token=" + strings.TrimSpace(string(token))
			if a.json {
				return a.writeJSON(map[string]string{"url": base, "link": link})
			}
			fmt.Fprintln(a.stdout, base)
			if noOpen {
				fmt.Fprintf(a.stdout, "Sign in with: %s\n", link)
				return nil
			}
			return openBrowser(link)
		},
	}
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "print the sign-in link instead of opening the browser")
	return cmd
}

func openBrowser(url string) error {
	opener := "xdg-open"
	if goruntime.GOOS == "darwin" {
		opener = "open"
	}
	if err := exec.Command(opener, url).Run(); err != nil {
		return fail(ExitError, "%s: %v; open %s yourself", opener, err, url)
	}
	return nil
}
