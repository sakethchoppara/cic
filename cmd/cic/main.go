package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sakethchoppara/cic/internal/api"
	"github.com/sakethchoppara/cic/internal/config"
	"github.com/sakethchoppara/cic/internal/pool"
	"github.com/sakethchoppara/cic/internal/runner"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8088", "listen address (no auth in v1: keep it on localhost)")
	cfgPath := flag.String("config", "config/languages.yaml", "language definitions")
	poolSize := flag.Int("pool-size", 2, "idle containers kept per language")
	queueTimeout := flag.Duration("queue-timeout", time.Second, "wait for a warm container before cold-starting one")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	run, err := runner.New()
	if err != nil {
		log.Fatal(err)
	}
	defer run.Close()

	if n, err := run.RemoveAll(ctx); err != nil {
		log.Fatal(err)
	} else if n > 0 {
		log.Printf("removed %d leftover containers", n)
	}

	langs, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}
	// Drop languages whose image is missing or lacks the configured user.
	for name, lang := range langs {
		if err := run.Check(ctx, lang); err != nil {
			log.Printf("disabling %s: %v", name, err)
			delete(langs, name)
		}
	}
	if len(langs) == 0 {
		log.Fatal("no usable languages")
	}

	pools := make(map[string]*pool.Pool, len(langs))
	for name, lang := range langs {
		pools[name] = pool.New(run, name, lang, *poolSize, *queueTimeout)
	}

	srv := &http.Server{Addr: *addr, Handler: api.Routes(pools), ReadHeaderTimeout: 5 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()
	log.Printf("cic listening on %s", *addr)

	var serveFailed bool
	select {
	case err := <-serveErr:
		log.Print(err)
		serveFailed = true
	case <-ctx.Done():
	}

	// FR9: finish in-flight requests, then remove the idle containers.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	for _, p := range pools {
		p.Close()
	}
	log.Print("cic stopped")
	if serveFailed {
		os.Exit(1)
	}
}
