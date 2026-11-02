// cic-dockerproxy listens on a unix socket and forwards only the Docker API
// calls the cic runner needs, with sandbox settings enforced. Run it as a user
// that can reach the Docker socket, and run cic as a user that can only reach
// this one: DOCKER_HOST=unix:///run/cic/docker.sock.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/sakethchoppara/cic/internal/config"
	"github.com/sakethchoppara/cic/internal/dockerproxy"
)

// The API's own bounds (internal/api): user memory_mb and the run pids limit.
const maxUserMemoryMB, runPids = 512, 64

func main() {
	listen := flag.String("listen", "/run/cic/docker.sock", "unix socket to serve")
	upstream := flag.String("upstream", "/var/run/docker.sock", "Docker daemon socket")
	cfgPath := flag.String("config", "config/languages.yaml", "language definitions (images and budgets allowed)")
	mode := flag.String("mode", "0660", "permissions for the listening socket")
	flag.Parse()

	langs, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}
	policy := dockerproxy.Policy{Images: map[string]bool{}, MaxMemoryMB: maxUserMemoryMB, MaxPids: runPids}
	for _, l := range langs {
		policy.Images[l.Image] = true
		policy.MaxMemoryMB = max(policy.MaxMemoryMB, l.CompileMemoryMB, l.RunMemoryMB)
		policy.MaxPids = max(policy.MaxPids, l.CompilePids)
	}
	perm, err := strconv.ParseUint(*mode, 8, 32)
	if err != nil {
		log.Fatal(err)
	}

	os.Remove(*listen) // stale socket from a previous run
	l, err := net.Listen("unix", *listen)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.Chmod(*listen, os.FileMode(perm)); err != nil {
		log.Fatal(err)
	}

	dial := func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", *upstream)
	}
	srv := &http.Server{Handler: dockerproxy.New(dial, policy), ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		srv.Close() // exec streams are hijacked, so Shutdown would wait on them forever
	}()
	log.Printf("cic-dockerproxy: %s -> %s, %d images, memory <= %d MB, pids <= %d",
		*listen, *upstream, len(policy.Images), policy.MaxMemoryMB, policy.MaxPids)
	if err := srv.Serve(l); err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
