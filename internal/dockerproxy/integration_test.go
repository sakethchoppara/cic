package dockerproxy

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/sakethchoppara/cic/internal/config"
	"github.com/sakethchoppara/cic/internal/runner"
)

// TestRealDaemon runs the runner through the proxy against the real Docker
// daemon, then checks the proxy refuses what an attacker holding its socket
// would try.
func TestRealDaemon(t *testing.T) {
	direct, err := client.New(client.FromEnv)
	if err != nil {
		t.Skip("docker unavailable:", err)
	}
	defer direct.Close()
	if _, err := direct.Ping(context.Background(), client.PingOptions{}); err != nil {
		t.Skip("docker unavailable:", err)
	}
	langs, err := config.Load("../../config/languages.yaml")
	if err != nil {
		t.Fatal(err)
	}
	policy := Policy{Images: map[string]bool{}, MaxMemoryMB: 1024, MaxPids: 256}
	for _, l := range langs {
		policy.Images[l.Image] = true
	}

	sock := filepath.Join(t.TempDir(), "docker.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	upstream := strings.TrimPrefix(direct.DaemonHost(), "unix://")
	dial := func(ctx context.Context) (net.Conn, error) { return (&net.Dialer{}).DialContext(ctx, "unix", upstream) }
	srv := &http.Server{Handler: New(dial, policy)}
	go srv.Serve(l)
	defer srv.Close()

	t.Setenv("DOCKER_HOST", "unix://"+sock)
	r, err := runner.New()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()

	// Everything the runner does works through the proxy.
	for _, c := range []struct {
		lang, code, status string
		timeout            time.Duration
		memoryMB           int
	}{
		{"python", "print(1)", "ok", 5 * time.Second, 0},
		{"python", "print(1)", "ok", 5 * time.Second, 64},                              // ContainerUpdate for custom memory
		{"c", "int main(){return 0;}", "ok", 5 * time.Second, 0},                       // compile, then lower limits
		{"python", "while True: pass", "timeout", time.Second, 0},                      // kill
		{"python", "while True: print('x'*1000)", "runtime_error", 5 * time.Second, 0}, // flood kill
	} {
		if err := r.Check(ctx, langs[c.lang]); err != nil {
			t.Skipf("%s unusable: %v", c.lang, err)
		}
		res, err := r.Run(ctx, langs[c.lang], c.code, "", c.timeout, c.memoryMB)
		if err != nil || res.Status != c.status {
			t.Fatalf("%s %q through proxy: %+v %v", c.lang, c.code, res, err)
		}
	}
	if _, err := r.RemoveAll(ctx); err != nil {
		t.Fatalf("RemoveAll through proxy: %v", err)
	}
	if n, err := r.Containers(ctx); err != nil || n != 0 {
		t.Fatalf("%d containers left (err %v)", n, err)
	}

	// What someone holding the proxy socket would try instead.
	attacker, err := client.New(client.WithHost("unix://" + sock))
	if err != nil {
		t.Fatal(err)
	}
	defer attacker.Close()
	pids := int64(64)
	_, err = attacker.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{Image: "python:3.12-slim", User: "nobody", Labels: map[string]string{"cic": "x"}},
		HostConfig: &container.HostConfig{
			Privileged: true, Binds: []string{"/:/host"}, NetworkMode: "none", ReadonlyRootfs: true,
			CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges"},
			Resources: container.Resources{NanoCPUs: 1e9, Memory: 64 << 20, MemorySwap: 64 << 20, PidsLimit: &pids},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "cic proxy") {
		t.Fatalf("privileged create with / mounted: got %v, want refusal", err)
	}
	if _, err := attacker.ContainerList(ctx, client.ContainerListOptions{All: true}); err == nil {
		t.Fatal("unfiltered container list was allowed")
	}
	if _, err := attacker.ImagePull(ctx, "alpine:latest", client.ImagePullOptions{}); err == nil {
		t.Fatal("image pull was allowed")
	}

	// A container that isn't cic's: the proxy must not let the runner touch it.
	other, err := direct.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:     &container.Config{Image: "python:3.12-slim", Cmd: []string{"sleep", "60"}},
		HostConfig: &container.HostConfig{NetworkMode: "none"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer direct.ContainerRemove(context.Background(), other.ID, client.ContainerRemoveOptions{Force: true})
	if _, err := attacker.ContainerRemove(ctx, other.ID, client.ContainerRemoveOptions{Force: true}); err == nil {
		t.Fatal("removing a non-cic container was allowed")
	}
	if _, err := attacker.ExecCreate(ctx, other.ID, client.ExecCreateOptions{User: "nobody", Cmd: []string{"id"}}); err == nil {
		t.Fatal("exec into a non-cic container was allowed")
	}
}
