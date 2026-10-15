package runtime

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"
)

// ExecResult is how a command in a container ended.
type ExecResult struct {
	Code   int
	Stdout string
	Stderr string
}

// Err is an error with the end of the output when the command failed.
func (r ExecResult) Err(what string) error {
	if r.Code == 0 {
		return nil
	}
	out := strings.TrimSpace(r.Stderr)
	if out == "" {
		out = strings.TrimSpace(r.Stdout)
	}
	return fmt.Errorf("%s exited with %d: %s", what, r.Code, LastLines(out, 5))
}

// Exec runs a command in a running container as user (empty: the image's user) and
// waits for it to end.
func (d *Docker) Exec(ctx context.Context, id, user string, cmd []string) (ExecResult, error) {
	created, err := d.cli.ExecCreate(ctx, id, client.ExecCreateOptions{User: user, Cmd: cmd, AttachStdout: true, AttachStderr: true})
	if err != nil {
		return ExecResult{}, err
	}
	att, err := d.cli.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return ExecResult{}, err
	}
	defer att.Close()
	var stdout, stderr bytes.Buffer
	done := make(chan error, 1)
	go func() {
		_, err := stdcopy.StdCopy(&stdout, &stderr, att.Reader)
		done <- err
	}()
	select {
	case err = <-done:
	case <-ctx.Done():
		return ExecResult{}, ctx.Err()
	}
	if err != nil {
		return ExecResult{}, err
	}
	insp, err := d.cli.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
	if err != nil {
		return ExecResult{}, err
	}
	return ExecResult{Code: insp.ExitCode, Stdout: stdout.String(), Stderr: stderr.String()}, nil
}
