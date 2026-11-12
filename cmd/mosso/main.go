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

	"github.com/riggleg/mosso/internal/server"
	"github.com/riggleg/mosso/internal/vercel"
)

func main() {
	defaultAddr := "127.0.0.1:8080"
	if v := os.Getenv("MOSSO_LISTEN"); v != "" {
		defaultAddr = v
	}
	addr := flag.String("listen", defaultAddr, "HTTP listen address")
	flag.Parse()

	var client *vercel.Client
	if cfg, err := vercel.ConfigFromEnv(); err != nil {
		log.Printf("sandbox endpoints disabled: %v", err)
	} else {
		client = vercel.NewClient(cfg)
		log.Print("Vercel Sandbox credentials loaded")
	}
	srv := server.New(client)
	httpSrv := &http.Server{Addr: *addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("mosso listening on %s", *addr)
		errCh <- httpSrv.ListenAndServe()
	}()

	var exitErr error
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			exitErr = err
		}
	case <-ctx.Done():
		log.Print("shutting down")
	}
	stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	// Destroy any sandboxes still running so they do not keep accruing charges.
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("some sandboxes could not be destroyed; check the Vercel dashboard: %v", err)
	}
	if exitErr != nil {
		log.Fatal(exitErr)
	}
}
