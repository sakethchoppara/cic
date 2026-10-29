package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sakethchoppara/cic/internal/api"
	"github.com/sakethchoppara/cic/internal/config"
	"github.com/sakethchoppara/cic/internal/runner"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8088", "listen address (no auth in v1: keep it on localhost)")
	cfgPath := flag.String("config", "config/languages.yaml", "language definitions")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	run, err := runner.New()
	if err != nil {
		log.Fatal(err)
	}
	defer run.Close()

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

	srv := &http.Server{Addr: *addr, Handler: api.Routes(run, langs), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	log.Printf("cic listening on %s", *addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	log.Print("cic stopped")
}
