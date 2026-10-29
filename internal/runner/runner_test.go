package runner

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"
)

// Integration tests: need a Docker daemon and python:3.12-slim pulled.
func newRunner(t *testing.T) *Runner {
	t.Helper()
	r, err := New()
	if err != nil {
		t.Skip("docker unavailable:", err)
	}
	if _, err := r.cli.ImageInspect(context.Background(), Python.Image); err != nil {
		t.Skipf("%s not pulled: %v", Python.Image, err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func TestRunPython(t *testing.T) {
	r := newRunner(t)
	cases := []struct {
		name, code, stdin string
		timeout           time.Duration
		status, phase     string
		stdout            string // substring
		stderr            string // substring
		truncated         bool
	}{
		{name: "hello", code: "print(sum(range(10)))", status: "ok", stdout: "45\n"},
		{name: "stdin", code: "print(input()[::-1])", stdin: "abc\n", status: "ok", stdout: "cba"},
		{name: "exception", code: "1/0", status: "runtime_error", phase: "run", stderr: "ZeroDivisionError"},
		{name: "exit code", code: "import sys; sys.exit(3)", status: "runtime_error", phase: "run"},
		{name: "infinite loop", code: "while True: pass", timeout: time.Second, status: "timeout", phase: "run"},
		{name: "memory bomb", code: "x = []\nwhile True: x.append(bytearray(10<<20))", status: "memory_exceeded", phase: "run"},
		{name: "fork bomb", code: "import os\nwhile True:\n  try: os.fork()\n  except OSError: pass", timeout: 2 * time.Second, status: "timeout", phase: "run"},
		{name: "noisy timeout", code: "import time\nwhile True: print(1); time.sleep(0.001)", timeout: time.Second, status: "timeout", phase: "run"},
		{name: "stdout flood", code: "while True: print('x'*1000)", status: "runtime_error", phase: "run", truncated: true},
		{name: "non-root", code: "import os; print(os.getuid())", status: "ok", stdout: "65534"},
		{name: "no network", code: "import socket; socket.create_connection(('1.1.1.1', 53), timeout=2)", status: "runtime_error", stderr: "OSError"},
		{name: "read-only rootfs", code: "open('/etc/x','w')", status: "runtime_error", stderr: "Read-only file system"},
		{name: "work writable", code: "open('/work/f','w').write('ok'); print(open('/work/f').read())", status: "ok", stdout: "ok"},
		{name: "no docker socket", code: "import os; print(os.path.exists('/var/run/docker.sock'))", status: "ok", stdout: "False"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if c.timeout == 0 {
				c.timeout = 5 * time.Second
			}
			res, err := r.Run(context.Background(), Python, c.code, c.stdin, c.timeout, 0)
			if err != nil {
				t.Fatal(err)
			}
			if res.Status != c.status || (c.phase != "" && res.Phase != c.phase) ||
				!strings.Contains(res.Stdout, c.stdout) || !strings.Contains(res.Stderr, c.stderr) ||
				res.StdoutTruncated != c.truncated {
				t.Fatalf("got %+v", res)
			}
		})
	}
}

func TestNoLeakedContainers(t *testing.T) {
	r := newRunner(t)
	if _, err := r.Run(context.Background(), Python, "print(1)", "", 5*time.Second, 0); err != nil {
		t.Fatal(err)
	}
	list, err := r.cli.ContainerList(context.Background(), client.ContainerListOptions{
		All: true, Filters: client.Filters{}.Add("label", "cic"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Parallel tests in TestRunPython have finished by now (subtests block their parent).
	if len(list.Items) != 0 {
		t.Fatalf("%d cic containers left behind", len(list.Items))
	}
}
