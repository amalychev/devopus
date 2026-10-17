package api

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentv2 "github.com/amalychev/devopus/gen/devopus/agent/v2"
	"github.com/amalychev/devopus/internal/agent/runtime"
	"github.com/amalychev/devopus/internal/agent/stack"
	"github.com/amalychev/devopus/internal/agent/store"
	"github.com/amalychev/devopus/internal/spec"
)

// Containers is what the panel's logs and resources need from the runtime.
type Containers interface {
	Logs(ctx context.Context, id string, tail int, follow bool, fn func(runtime.LogLine) error) error
	Stats(ctx context.Context, id string) (runtime.Usage, error)
}

// projectOf is a project the agent has, as it runs it: with the personal overrides of
// devopus.yaml for a project of the workspace server.
func (s *Server) projectOf(project string) (*spec.Project, error) {
	if project == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name the project"))
	}
	for _, p := range s.b.Store().Snapshot().Projects {
		if p.Name == project {
			ws, err := store.ParseProject(p)
			if err != nil {
				return nil, connect.NewError(connect.CodeInternal, err)
			}
			return ws, nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("%w %q: the projects of the current workspace come from its server ('devopus project list')", store.ErrUnknownProject, project))
}

func (s *Server) DescribeProject(_ context.Context, req *connect.Request[agentv2.DescribeProjectRequest]) (*connect.Response[agentv2.DescribeProjectResponse], error) {
	ws, err := s.projectOf(req.Msg.Project)
	if err != nil {
		return nil, err
	}
	path := ""
	res := &agentv2.DescribeProjectResponse{
		Project: ws.Name, WorkspacePath: path, Domain: ws.Domain,
		Services: containerServices(ws), BranchPrefix: ws.Policy().BranchPrefix,
	}
	if res.Spec, err = ws.YAML(); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	for _, p := range s.b.Store().Snapshot().Projects {
		if p.Name == ws.Name && p.WorkspaceId != "" {
			_, r, _ := store.EffectiveProject(p)
			res.OverrideFile, res.OverridesApplied, res.OverridesRejected = store.OverrideFile(p), r.Applied, r.Rejected
		}
	}
	for _, n := range ws.AppNames() {
		a := ws.Apps[n]
		res.Apps = append(res.Apps, &agentv2.AppInfo{Name: n, Routes: a.Routes, Optional: a.Optional, Uses: ws.Needs(n)})
	}
	if len(req.Msg.Forks) > 0 {
		iso, err := stack.Feature(ws, req.Msg.Forks)
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		res.Plan = plan(ws, iso)
	}
	return connect.NewResponse(res), nil
}

// plan is what `devopus feature create` prints before it creates a feature.
func plan(ws *spec.Project, iso *stack.Isolation) *agentv2.FeaturePlan {
	p := &agentv2.FeaturePlan{Forked: iso.Forked, Dependents: iso.Dependents()}
	for _, svc := range iso.Services {
		if stack.Engine(ws.Services[svc].ImageRef()) != "" {
			p.Copies = append(p.Copies, svc)
		} else {
			p.Own = append(p.Own, svc)
		}
	}
	p.Shared = append(append([]string{}, iso.SharedApps...), iso.SharedServices...)
	for _, m := range iso.NoMigrate {
		p.Warnings = append(p.Warnings, fmt.Sprintf("%s writes to data of the base environment; its migrations are skipped (features.database: shared)", m))
	}
	for _, m := range iso.SharedApps {
		var data []string
		for _, u := range ws.Needs(m) {
			if ws.Services[u] != nil {
				data = append(data, u)
			}
		}
		if len(data) > 0 {
			p.Warnings = append(p.Warnings, fmt.Sprintf("the feature calls %s of the base environment, which writes to %s there; fork %s too to keep the feature's data apart",
				m, strings.Join(data, ", "), m))
		}
	}
	return p
}

func (s *Server) env(id string) (*agentv2.EnvironmentStatus, error) {
	for _, e := range s.b.Statuses() {
		if e.Id == id {
			return e, nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("no environment %q", id))
}

func (s *Server) logDir(envID string) string { return filepath.Join(s.b.EnvsDir(), envID, "logs") }

func (s *Server) LogSources(_ context.Context, req *connect.Request[agentv2.LogSourcesRequest]) (*connect.Response[agentv2.LogSourcesResponse], error) {
	e, err := s.env(req.Msg.EnvironmentId)
	if err != nil {
		return nil, err
	}
	res := &agentv2.LogSourcesResponse{}
	for _, u := range e.Units {
		if u.Container != "" && u.State != "missing" {
			res.Sources = append(res.Sources, &agentv2.LogSource{Unit: u.Name})
		}
	}
	entries, _ := os.ReadDir(s.logDir(e.Id))
	var files []*agentv2.LogSource
	for _, de := range entries {
		name, ok := strings.CutSuffix(de.Name(), ".log")
		if !ok || de.IsDir() {
			continue
		}
		src := &agentv2.LogSource{File: name}
		if fi, err := de.Info(); err == nil {
			src.UpdatedAt = timestamppb.New(fi.ModTime())
		}
		files = append(files, src)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].File < files[j].File })
	res.Sources = append(res.Sources, files...)
	return connect.NewResponse(res), nil
}

const (
	logBatch     = 500
	logFlush     = 100 * time.Millisecond
	logFilePoll  = 500 * time.Millisecond
	maxFileLines = 10000
)

func (s *Server) Logs(ctx context.Context, req *connect.Request[agentv2.LogsRequest], stream *connect.ServerStream[agentv2.LogsResponse]) error {
	m := req.Msg
	e, err := s.env(m.EnvironmentId)
	if err != nil {
		return err
	}
	src := m.GetSource()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := newLogBatcher(stream)
	defer out.close()
	switch {
	case src.GetUnit() != "":
		var container string
		for _, u := range e.Units {
			if u.Name == src.Unit {
				container = u.Container
			}
		}
		if container == "" {
			return connect.NewError(connect.CodeNotFound, fmt.Errorf("%s has no container for %q", e.Id, src.Unit))
		}
		err = s.b.Containers().Logs(ctx, container, int(m.Tail), m.Follow, func(l runtime.LogLine) error {
			line := &agentv2.LogLine{Text: l.Text, Stderr: l.Stderr}
			if !l.Time.IsZero() {
				line.Time = timestamppb.New(l.Time)
			}
			return out.add(line)
		})
	case src.GetFile() != "":
		name := src.File
		if name != filepath.Base(name) || strings.HasPrefix(name, ".") {
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("bad log name %q", name))
		}
		err = tailFile(ctx, filepath.Join(s.logDir(e.Id), name+".log"), int(m.Tail), m.Follow, out.add)
	default:
		return connect.NewError(connect.CodeInvalidArgument, errors.New("name a unit or a log file"))
	}
	if err != nil && ctx.Err() == nil {
		if errors.Is(err, os.ErrNotExist) {
			return connect.NewError(connect.CodeNotFound, err)
		}
		var ce *connect.Error
		if errors.As(err, &ce) {
			return ce
		}
		return connect.NewError(connect.CodeUnavailable, err)
	}
	return out.close()
}

// logBatcher sends lines in batches: a log of thousands of lines as thousands of
// messages would keep the browser busy rendering.
type logBatcher struct {
	stream *connect.ServerStream[agentv2.LogsResponse]
	mu     sync.Mutex
	buf    []*agentv2.LogLine
	err    error
	stop   chan struct{}
	done   chan struct{}
}

func newLogBatcher(stream *connect.ServerStream[agentv2.LogsResponse]) *logBatcher {
	b := &logBatcher{stream: stream, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(b.done)
		t := time.NewTicker(logFlush)
		defer t.Stop()
		for {
			select {
			case <-b.stop:
				return
			case <-t.C:
				b.mu.Lock()
				b.flushLocked()
				b.mu.Unlock()
			}
		}
	}()
	return b
}

func (b *logBatcher) flushLocked() {
	if len(b.buf) == 0 || b.err != nil {
		return
	}
	b.err = b.stream.Send(&agentv2.LogsResponse{Lines: b.buf})
	b.buf = nil
}

func (b *logBatcher) add(l *agentv2.LogLine) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, l)
	if len(b.buf) >= logBatch {
		b.flushLocked()
	}
	return b.err
}

// close sends what is left; it is safe to call twice.
func (b *logBatcher) close() error {
	select {
	case <-b.stop:
	default:
		close(b.stop)
	}
	<-b.done
	b.mu.Lock()
	defer b.mu.Unlock()
	b.flushLocked()
	return b.err
}

// tailFile sends the last lines of a hook or build log and, with follow, what is
// appended to it. The agent recreates the file when the hook runs again: then it starts
// over from the top.
func tailFile(ctx context.Context, path string, tail int, follow bool, fn func(*agentv2.LogLine) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { f.Close() }()
	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	offset := int64(len(data))
	head := headOf(data)
	// A line still being written stays for the next read.
	complete := data
	if i := bytes.LastIndexByte(data, '\n'); i >= 0 && follow {
		complete, offset = data[:i+1], int64(i+1)
	} else if follow {
		complete, offset = nil, 0
	}
	lines := strings.Split(strings.TrimSuffix(string(complete), "\n"), "\n")
	if len(complete) == 0 {
		lines = nil
	}
	limit := tail
	if limit <= 0 || limit > maxFileLines {
		limit = maxFileLines
	}
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	for _, l := range lines {
		if err := fn(&agentv2.LogLine{Text: l}); err != nil {
			return err
		}
	}
	if !follow {
		return nil
	}
	t := time.NewTicker(logFilePoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		fi, err := os.Stat(path)
		if err != nil {
			continue // between the agent removing and recreating it
		}
		cur, _ := f.Stat()
		if !os.SameFile(fi, cur) || fi.Size() < offset || !sameHead(path, head) {
			nf, err := os.Open(path)
			if err != nil {
				continue
			}
			f.Close()
			f, offset, head = nf, 0, nil
			if err := fn(&agentv2.LogLine{Text: "--- the log started over ---"}); err != nil {
				return err
			}
		}
		if fi.Size() == offset {
			continue
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return err
		}
		r := bufio.NewReader(f)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				break // an incomplete line waits for the rest
			}
			offset += int64(len(line))
			if len(head) < headSize {
				head = headOf(append(head, line...))
			}
			if err := fn(&agentv2.LogLine{Text: strings.TrimSuffix(line, "\n")}); err != nil {
				return err
			}
		}
	}
}

func (s *Server) Resources(ctx context.Context, req *connect.Request[agentv2.ResourcesRequest]) (*connect.Response[agentv2.ResourcesResponse], error) {
	e, err := s.env(req.Msg.EnvironmentId)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res := &agentv2.ResourcesResponse{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, u := range e.Units {
		if u.Container == "" || (u.State != "running" && u.State != "starting" && u.State != "unhealthy") {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			use, err := s.b.Containers().Stats(ctx, u.Container)
			if err != nil {
				return // stopped in between
			}
			mu.Lock()
			res.Units = append(res.Units, &agentv2.UnitResources{Name: u.Name, CpuPercent: use.CPUPercent, MemoryBytes: use.MemoryBytes, MemoryLimit: use.MemoryLimit})
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Slice(res.Units, func(i, j int) bool { return res.Units[i].Name < res.Units[j].Name })
	return connect.NewResponse(res), nil
}

// headSize bytes from the top of a log tell a log written over from one appended to.
const headSize = 64

func headOf(b []byte) []byte {
	return bytes.Clone(b[:min(len(b), headSize)])
}

func sameHead(path string, head []byte) bool {
	if len(head) == 0 {
		return true
	}
	f, err := os.Open(path)
	if err != nil {
		return true
	}
	defer f.Close()
	buf := make([]byte, len(head))
	if _, err := io.ReadFull(f, buf); err != nil {
		return false
	}
	return bytes.Equal(buf, head)
}

// containerServices are the services that run as their own containers: what the panel
// lists next to the applications.
func containerServices(p *spec.Project) []string {
	var out []string
	for _, n := range p.ServiceNames() {
		if p.Services[n].IsContainer() {
			out = append(out, n)
		}
	}
	return out
}
