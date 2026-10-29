// Package runner executes one program in a fresh, locked-down container and destroys it.
package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/sakethchoppara/cic/internal/config"
)

const (
	streamCap      = 64 << 10  // per-stream bytes kept (FR8)
	floodCap       = 256 << 10 // combined bytes seen before the container is killed (FR8)
	pidsLimit      = 64
	compileTimeout = 10 * time.Second
)

type Lang = config.Lang

type Result struct {
	Status          string // ok, compile_error, runtime_error, timeout, memory_exceeded
	Phase           string // compile or run; empty when ok
	Stdout, Stderr  string
	ExitCode        int
	StdoutTruncated bool
	StderrTruncated bool
	CompileMS       int64
	RunMS           int64
}

type Runner struct{ cli *client.Client }

func New() (*Runner, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return nil, err
	}
	return &Runner{cli: cli}, nil
}

func (r *Runner) Close() error { return r.cli.Close() }

// Check starts a container for lang and removes it. It fails if the image is
// missing or lacks lang.User (Docker refuses to start as an unknown user).
func (r *Runner) Check(ctx context.Context, lang Lang) error {
	id, err := r.start(ctx, lang, lang.RunMemoryMB, pidsLimit)
	if id != "" {
		r.cli.ContainerRemove(context.Background(), id, client.ContainerRemoveOptions{Force: true})
	}
	return err
}

// Run executes code under lang with the given run timeout and memory limit (memoryMB 0 = language default).
// A non-nil error means the run could not be carried out (internal_error), not that the program failed.
func (r *Runner) Run(ctx context.Context, lang Lang, code, stdin string, timeout time.Duration, memoryMB int) (Result, error) {
	if memoryMB == 0 {
		memoryMB = lang.RunMemoryMB
	}
	compiled := len(lang.CompileCmd) > 0
	// One cgroup per container, so it starts at the compile budget and is lowered after compiling (FR4).
	mem, pids := memoryMB, pidsLimit
	if compiled {
		mem, pids = lang.CompileMemoryMB, lang.CompilePids
	}
	id, err := r.start(ctx, lang, mem, pids)
	if id != "" {
		// Fresh context: the container must go even if ctx is done (FR2).
		defer r.cli.ContainerRemove(context.Background(), id, client.ContainerRemoveOptions{Force: true})
	}
	if err != nil {
		return Result{}, err
	}

	// FR7.2: stream source over exec stdin; the archive API can't write into tmpfs.
	src, err := r.exec(ctx, id, lang, []string{"sh", "-c", "cat > /work/" + lang.SourceFile}, code)
	if err != nil || src.exitCode != 0 {
		return Result{}, fmt.Errorf("write source: %v %s", err, src.stderr.buf.String())
	}

	var compileMS int64
	ooms := 0
	if compiled {
		res, err := r.phase(ctx, id, lang, "compile", lang.CompileCmd, "", compileTimeout, &ooms)
		if err != nil || res.Status != "ok" {
			if res.Status == "runtime_error" {
				res.Status = "compile_error"
			}
			return res, err
		}
		compileMS = res.CompileMS

		// Build caches in /tmp are tmpfs pages charged to this cgroup; drop them before
		// shrinking the limit so the run budget is the program's alone.
		if c, err := r.exec(ctx, id, lang, []string{"find", "/tmp", "-mindepth", "1", "-delete"}, ""); err != nil || c.exitCode != 0 {
			return Result{}, fmt.Errorf("clear /tmp: %v %s", err, c.stderr.buf.String())
		}
		m, p := int64(memoryMB)<<20, int64(pidsLimit)
		if _, err := r.cli.ContainerUpdate(ctx, id, client.ContainerUpdateOptions{
			Resources: &container.Resources{Memory: m, MemorySwap: m, PidsLimit: &p},
		}); err != nil {
			return Result{}, fmt.Errorf("lower limits: %w", err)
		}
	}

	res, err := r.phase(ctx, id, lang, "run", lang.RunCmd, stdin, timeout, &ooms)
	res.CompileMS = compileMS
	return res, err
}

// phase runs one exec under timeout and classifies it. ooms is the cgroup's
// oom_kill count before the phase and is updated to the count after it.
// Timing goes in RunMS or CompileMS by phase name.
func (r *Runner) phase(ctx context.Context, id string, lang Lang, name string, cmd []string, stdin string, timeout time.Duration, ooms *int) (Result, error) {
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	ex, err := r.exec(pctx, id, lang, cmd, stdin)
	res := Result{
		Phase:           name,
		Stdout:          ex.stdout.buf.String(),
		Stderr:          ex.stderr.buf.String(),
		ExitCode:        ex.exitCode,
		StdoutTruncated: ex.stdout.truncated,
		StderrTruncated: ex.stderr.truncated,
	}
	if ms := time.Since(start).Milliseconds(); name == "compile" {
		res.CompileMS = ms
	} else {
		res.RunMS = ms
	}
	switch {
	case errors.Is(err, errFlood):
		res.Status, res.StdoutTruncated = "runtime_error", true
		return res, nil
	case pctx.Err() != nil && ctx.Err() == nil:
		res.Status = "timeout"
		return res, nil
	case err != nil:
		return res, err
	}

	// FR7.5: an exec'd process can be OOM-killed while `sleep infinity` lives on,
	// so State.OOMKilled never fires. The cgroup's own counter does.
	n, err := r.oomKills(ctx, id, lang)
	if err != nil {
		return res, err
	}
	before := *ooms
	*ooms = n
	switch {
	case n > before:
		res.Status = "memory_exceeded"
	case res.ExitCode != 0:
		res.Status = "runtime_error"
	default:
		res.Status, res.Phase = "ok", ""
	}
	return res, nil
}

// start creates and starts an idle container with every FR3-FR5 control applied.
func (r *Runner) start(ctx context.Context, lang Lang, memoryMB, pidsMax int) (string, error) {
	mem := int64(memoryMB) << 20
	pids := int64(pidsMax)
	tmpfs := fmt.Sprintf("rw,nosuid,nodev,size=%dm,mode=1777", lang.TmpfsMB)
	created, err := r.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:      lang.Image,
			Cmd:        []string{"sleep", "infinity"},
			User:       lang.User,
			Env:        lang.Env,
			WorkingDir: "/work",
			Labels:     map[string]string{"cic": "1"},
		},
		HostConfig: &container.HostConfig{
			NetworkMode:    "none",
			ReadonlyRootfs: true,
			Tmpfs: map[string]string{
				"/tmp":  tmpfs + ",noexec",
				"/work": tmpfs + ",exec", // compiled binaries run from here
			},
			CapDrop:     []string{"ALL"},
			SecurityOpt: []string{"no-new-privileges"},
			Resources: container.Resources{
				NanoCPUs:   1e9,
				Memory:     mem,
				MemorySwap: mem,
				PidsLimit:  &pids,
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("create: %w", err)
	}
	if _, err := r.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return created.ID, fmt.Errorf("start: %w", err)
	}
	return created.ID, nil
}

type execResult struct {
	stdout, stderr *capWriter
	exitCode       int
}

var errFlood = errors.New("output flood")

// exec runs cmd in the container, feeding stdin and capturing capped output.
// If ctx ends first it kills the container (FR4: kill, don't wait) and returns ctx.Err().
func (r *Runner) exec(ctx context.Context, id string, lang Lang, cmd []string, stdin string) (execResult, error) {
	var seen int
	res := execResult{stdout: &capWriter{seen: &seen}, stderr: &capWriter{seen: &seen}}

	created, err := r.cli.ExecCreate(ctx, id, client.ExecCreateOptions{
		User: lang.User, Env: lang.Env, WorkingDir: "/work", Cmd: cmd,
		AttachStdin: true, AttachStdout: true, AttachStderr: true,
	})
	if err != nil {
		return res, fmt.Errorf("exec create: %w", err)
	}
	att, err := r.cli.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return res, fmt.Errorf("exec attach: %w", err)
	}
	defer att.Close()

	done := make(chan error, 1)
	go func() {
		// Write stdin concurrently with reading output: a program that prints before
		// reading would otherwise deadlock against a full pipe.
		go func() {
			io.Copy(att.Conn, strings.NewReader(stdin))
			att.CloseWrite()
		}()
		_, err := stdcopy.StdCopy(res.stdout, res.stderr, att.Reader)
		done <- err
	}()

	select {
	case err = <-done:
	case <-ctx.Done():
		// Close before kill: the daemon won't finish the kill while it's blocked
		// writing output we no longer read (a flood takes 10s to die otherwise).
		att.Close()
		r.kill(id)
		<-done // wait so the writers aren't touched after return
		return res, ctx.Err()
	}
	if errors.Is(err, errFlood) {
		att.Close() // see above
		r.kill(id)
		return res, errFlood
	}
	if err != nil {
		return res, fmt.Errorf("exec stream: %w", err)
	}

	ins, err := r.cli.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
	if err != nil {
		return res, fmt.Errorf("exec inspect: %w", err)
	}
	res.exitCode = ins.ExitCode
	return res, nil
}

func (r *Runner) kill(id string) {
	r.cli.ContainerKill(context.Background(), id, client.ContainerKillOptions{Signal: "KILL"})
}

// oomKills reads oom_kill from the container's cgroup v2 memory.events (private cgroupns).
func (r *Runner) oomKills(ctx context.Context, id string, lang Lang) (int, error) {
	ev, err := r.exec(ctx, id, lang, []string{"cat", "/sys/fs/cgroup/memory.events"}, "")
	if err != nil || ev.exitCode != 0 {
		return 0, fmt.Errorf("read memory.events: %v %s", err, ev.stderr.buf.String())
	}
	for _, line := range strings.Split(ev.stdout.buf.String(), "\n") {
		if v, ok := strings.CutPrefix(line, "oom_kill "); ok {
			return strconv.Atoi(v)
		}
	}
	return 0, errors.New("oom_kill missing from memory.events")
}

// capWriter keeps the first streamCap bytes and errors once both streams together pass floodCap.
// StdCopy writes both streams from one goroutine, so the shared counter needs no lock.
type capWriter struct {
	buf       bytes.Buffer
	truncated bool
	seen      *int
}

func (w *capWriter) Write(p []byte) (int, error) {
	*w.seen += len(p)
	if room := streamCap - w.buf.Len(); room < len(p) {
		w.buf.Write(p[:max(room, 0)])
		w.truncated = true
	} else {
		w.buf.Write(p)
	}
	if *w.seen > floodCap {
		return len(p), errFlood
	}
	return len(p), nil
}
