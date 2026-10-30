// Package security runs hostile programs from testdata/ through the full HTTP
// stack (warm pools included) and checks each gets a controlled response.
// Needs a Docker daemon and the images from scripts/images.sh.
package security

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sakethchoppara/cic/internal/api"
	"github.com/sakethchoppara/cic/internal/config"
	"github.com/sakethchoppara/cic/internal/pool"
	"github.com/sakethchoppara/cic/internal/runner"
)

var extLang = map[string]string{".py": "python", ".js": "node", ".c": "c", ".rs": "rust", ".go": "go"}

func TestHostilePrograms(t *testing.T) {
	hostname, _ := os.Hostname()
	cases := []struct {
		file, stdin     string
		timeoutMS       int
		status, phase   string
		stdout          string // exact, if set
		stdoutTruncated bool
		stderrTruncated bool
	}{
		{file: "infinite_loop.rs", timeoutMS: 1000, status: "timeout", phase: "run"},
		{file: "fork_bomb.c", timeoutMS: 2000, status: "timeout", phase: "run"},
		{file: "memory_bomb.py", status: "memory_exceeded", phase: "run"},
		{file: "memory_bomb.js", status: "memory_exceeded", phase: "run"},
		{file: "memory_bomb.go", status: "memory_exceeded", phase: "run"},
		{file: "compile_bomb.c", status: "memory_exceeded", phase: "compile"},
		{file: "stdout_flood.c", status: "runtime_error", phase: "run", stdoutTruncated: true},
		{file: "stderr_flood.py", status: "runtime_error", phase: "run", stdoutTruncated: true, stderrTruncated: true},
		{file: "timeout_keeps_output.py", timeoutMS: 1000, status: "timeout", phase: "run", stdout: "before\n"},
		{file: "host_paths.py", stdin: hostname, status: "ok", stdout: "SAFE\n"},
		{file: "write_outside.py", status: "ok", stdout: "SAFE\n"},
		{file: "tmpfs_fill.py", status: "ok", stdout: "SAFE\n"},
		{file: "network.js", status: "ok", stdout: "SAFE\n"},
		{file: "privileges.c", status: "ok", stdout: "SAFE\n"},
	}

	langs, err := config.Load("../../config/languages.yaml")
	if err != nil {
		t.Fatal(err)
	}
	r, err := runner.New()
	if err != nil {
		t.Skip("docker unavailable:", err)
	}
	defer r.Close()
	pools := map[string]*pool.Pool{}
	for name, lang := range langs {
		if err := r.Check(context.Background(), lang); err != nil {
			t.Skipf("%s unusable (run scripts/images.sh): %v", name, err)
		}
		pools[name] = pool.New(r, name, lang, 2, 5*time.Second)
	}
	srv := httptest.NewServer(api.Routes(pools))

	t.Run("fixtures", func(t *testing.T) {
		for _, c := range cases {
			t.Run(c.file, func(t *testing.T) {
				t.Parallel()
				code, err := os.ReadFile(filepath.Join("testdata", c.file))
				if err != nil {
					t.Fatal(err)
				}
				res := execute(t, srv.URL, map[string]any{
					"language": extLang[filepath.Ext(c.file)],
					"code":     string(code),
					"stdin":    c.stdin,
					"limits":   map[string]int{"timeout_ms": c.timeoutMS},
				})
				if res.Status != c.status || res.Phase != c.phase ||
					(c.stdout != "" && res.Stdout != c.stdout) ||
					res.StdoutTruncated != c.stdoutTruncated || res.StderrTruncated != c.stderrTruncated {
					t.Fatalf("got status=%s phase=%s truncated=%v/%v stdout=%.300q stderr=%.300q",
						res.Status, res.Phase, res.StdoutTruncated, res.StderrTruncated, res.Stdout, res.Stderr)
				}
			})
		}
	})

	// The host and the service are unharmed: a normal program still runs in every language.
	hello := map[string]string{
		"python": `print("ok")`,
		"node":   `console.log("ok")`,
		"c":      "#include <stdio.h>\nint main(){puts(\"ok\");}",
		"rust":   `fn main(){println!("ok");}`,
		"go":     "package main\nimport \"fmt\"\nfunc main(){fmt.Println(\"ok\")}",
	}
	for lang, code := range hello {
		if res := execute(t, srv.URL, map[string]any{"language": lang, "code": code}); res.Status != "ok" || res.Stdout != "ok\n" {
			t.Errorf("%s after hostile runs: %+v", lang, res)
		}
	}

	srv.Close()
	for _, p := range pools {
		p.Close()
	}
	if n, err := r.Containers(context.Background()); err != nil || n != 0 {
		t.Fatalf("%d containers left behind (err %v)", n, err)
	}
}

func execute(t *testing.T, url string, req map[string]any) api.ExecuteResponse {
	t.Helper()
	body, _ := json.Marshal(req)
	resp, err := http.Post(url+"/v1/execute", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var res api.ExecuteResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP %d: %+v", resp.StatusCode, res)
	}
	return res
}
