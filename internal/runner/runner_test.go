package runner

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/sakethchoppara/cic/internal/config"
)

// Integration tests: need a Docker daemon and the images in config/languages.yaml pulled.
func newRunner(t *testing.T) (*Runner, map[string]Lang) {
	t.Helper()
	langs, err := config.Load("../../config/languages.yaml")
	if err != nil {
		t.Fatal(err)
	}
	r, err := New()
	if err != nil {
		t.Skip("docker unavailable:", err)
	}
	t.Cleanup(func() { r.Close() })
	return r, langs
}

// lang skips the test if the language's image isn't pulled.
func lang(t *testing.T, r *Runner, langs map[string]Lang, name string) Lang {
	t.Helper()
	l := langs[name]
	if _, err := r.cli.ImageInspect(context.Background(), l.Image); err != nil {
		t.Skipf("%s not pulled: %v", l.Image, err)
	}
	return l
}

func TestRunPython(t *testing.T) {
	r, langs := newRunner(t)
	py := lang(t, r, langs, "python")
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
			res, err := r.Run(context.Background(), py, c.code, c.stdin, c.timeout, 0)
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
	r, langs := newRunner(t)
	if _, err := r.Run(context.Background(), lang(t, r, langs, "python"), "print(1)", "", 5*time.Second, 0); err != nil {
		t.Fatal(err)
	}
	list, err := r.cli.ContainerList(context.Background(), client.ContainerListOptions{
		All: true, Filters: client.Filters{}.Add("label", "cic"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Parallel subtests in earlier tests have finished by now (subtests block their parent).
	if len(list.Items) != 0 {
		t.Fatalf("%d cic containers left behind", len(list.Items))
	}
}

func TestRunLanguages(t *testing.T) {
	r, langs := newRunner(t)
	cases := []struct {
		lang, name, code, stdin string
		status, phase, stdout   string
		stderr                  string // substring
		memoryMB                int
	}{
		{lang: "node", name: "hello", code: "console.log([1,2,3].reduce((a,b)=>a+b))", status: "ok", stdout: "6\n"},
		{lang: "node", name: "throw", code: "throw new Error('boom')", status: "runtime_error", phase: "run", stderr: "boom"},

		{lang: "c", name: "hello", code: "#include <stdio.h>\nint main(){int n;scanf(\"%d\",&n);printf(\"%d\\n\",n*2);}", stdin: "21", status: "ok", stdout: "42\n"},
		{lang: "c", name: "compile error", code: "int main(){ return x; }", status: "compile_error", phase: "compile", stderr: "undeclared"},
		{lang: "c", name: "segfault", code: "int main(){ *(volatile int*)0 = 1; }", status: "runtime_error", phase: "run"},
		// 400 MB fits the 512 MB compile budget but not the 256 MB run budget:
		// proves the limit was lowered between phases. volatile stops -O2 eliding the allocation.
		{lang: "c", name: "limit lowered for run", code: "#include <stdlib.h>\nint main(){volatile char*p=malloc(400<<20);for(long i=0;i<(400<<20);i+=4096)p[i]=1;}", status: "memory_exceeded", phase: "run"},

		{lang: "rust", name: "hello", code: "fn main(){ println!(\"{}\", (1..=10).sum::<i32>()); }", status: "ok", stdout: "55\n"},
		{lang: "rust", name: "compile error", code: "fn main(){ let x: i32 = \"s\"; }", status: "compile_error", phase: "compile", stderr: "mismatched types"},
		{lang: "rust", name: "panic", code: "fn main(){ panic!(\"boom\"); }", status: "runtime_error", phase: "run", stderr: "boom"},

		{lang: "go", name: "hello", code: "package main\nimport \"fmt\"\nfunc main(){ fmt.Println(6*7) }", status: "ok", stdout: "42\n"},
		{lang: "go", name: "compile error", code: "package main\nfunc main(){ x := 1 }", status: "compile_error", phase: "compile", stderr: "declared and not used"},
		{lang: "go", name: "panic", code: "package main\nfunc main(){ panic(\"boom\") }", status: "runtime_error", phase: "run", stderr: "boom"},
	}
	for _, c := range cases {
		t.Run(c.lang+"/"+c.name, func(t *testing.T) {
			l := lang(t, r, langs, c.lang)
			t.Parallel()
			res, err := r.Run(context.Background(), l, c.code, c.stdin, 5*time.Second, c.memoryMB)
			if err != nil {
				t.Fatal(err)
			}
			if res.Status != c.status || res.Phase != c.phase ||
				(c.stdout != "" && res.Stdout != c.stdout) || !strings.Contains(res.Stderr, c.stderr) {
				t.Fatalf("got %+v", res)
			}
		})
	}
}

func TestCompileOOM(t *testing.T) {
	r, langs := newRunner(t)
	l := lang(t, r, langs, "rust")
	l.CompileMemoryMB = 24 // rustc can't start in this
	res, err := r.Run(context.Background(), l, "fn main(){}", "", 5*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "memory_exceeded" || res.Phase != "compile" {
		t.Fatalf("got %+v", res)
	}
}
