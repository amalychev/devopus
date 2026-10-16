package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// LogLine is one line of a container's output.
type LogLine struct {
	Time   time.Time
	Text   string
	Stderr bool
}

// Logs sends the output of a container to fn line by line: the last tail lines (0: all),
// then, with follow, new ones until ctx ends or the container stops.
func (d *Docker) Logs(ctx context.Context, id string, tail int, follow bool, fn func(LogLine) error) error {
	t := "all"
	if tail > 0 {
		t = strconv.Itoa(tail)
	}
	rc, err := d.cli.ContainerLogs(ctx, id, client.ContainerLogsOptions{
		ShowStdout: true, ShowStderr: true, Timestamps: true, Follow: follow, Tail: t,
	})
	if err != nil {
		return err
	}
	defer rc.Close()
	stdout, stdoutW := io.Pipe()
	stderr, stderrW := io.Pipe()
	go func() {
		_, err := stdcopy.StdCopy(stdoutW, stderrW, rc)
		stdoutW.CloseWithError(err)
		stderrW.CloseWithError(err)
	}()
	lines := make(chan LogLine)
	done := make(chan struct{})
	defer close(done)
	scan := func(r io.Reader, isErr bool, end chan<- error) {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			l := parseLogLine(sc.Text())
			l.Stderr = isErr
			select {
			case lines <- l:
			case <-done:
				end <- nil
				return
			}
		}
		end <- sc.Err()
	}
	ends := make(chan error, 2)
	go scan(stdout, false, ends)
	go scan(stderr, true, ends)
	for open := 2; open > 0; {
		select {
		case l := <-lines:
			if err := fn(l); err != nil {
				rc.Close()
				return err
			}
		case err := <-ends:
			open--
			if err != nil && ctx.Err() == nil {
				return err
			}
		case <-ctx.Done():
			rc.Close()
			return nil
		}
	}
	return nil
}

// parseLogLine splits the timestamp Docker puts in front of each line.
func parseLogLine(s string) LogLine {
	ts, text, ok := strings.Cut(s, " ")
	if ok {
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			return LogLine{Time: t, Text: text}
		}
	}
	return LogLine{Text: s}
}

// Usage is what a container uses right now.
type Usage struct {
	CPUPercent  float64 // of one CPU, as `docker stats` shows it
	MemoryBytes uint64
	MemoryLimit uint64
}

// Stats samples a container's usage; it takes about a second (two samples for CPU).
func (d *Docker) Stats(ctx context.Context, id string) (Usage, error) {
	res, err := d.cli.ContainerStats(ctx, id, client.ContainerStatsOptions{IncludePreviousSample: true})
	if err != nil {
		return Usage{}, err
	}
	defer res.Body.Close()
	var s container.StatsResponse
	if err := json.NewDecoder(res.Body).Decode(&s); err != nil {
		return Usage{}, err
	}
	return usage(s), nil
}

func usage(s container.StatsResponse) Usage {
	u := Usage{MemoryLimit: s.MemoryStats.Limit}
	// Like the docker CLI: the page cache the kernel can drop does not count.
	mem := s.MemoryStats.Usage
	cache := s.MemoryStats.Stats["inactive_file"]
	if cache == 0 {
		cache = s.MemoryStats.Stats["total_inactive_file"]
	}
	if cache < mem {
		mem -= cache
	}
	u.MemoryBytes = mem
	cpu := float64(s.CPUStats.CPUUsage.TotalUsage) - float64(s.PreCPUStats.CPUUsage.TotalUsage)
	sys := float64(s.CPUStats.SystemUsage) - float64(s.PreCPUStats.SystemUsage)
	cpus := float64(s.CPUStats.OnlineCPUs)
	if cpus == 0 {
		cpus = float64(len(s.CPUStats.CPUUsage.PercpuUsage))
	}
	if cpu > 0 && sys > 0 {
		u.CPUPercent = cpu / sys * cpus * 100
	}
	return u
}
