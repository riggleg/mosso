package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/riggleg/mosso/internal/claude"
	"github.com/riggleg/mosso/internal/github"
	"github.com/riggleg/mosso/internal/server"
	"github.com/riggleg/mosso/internal/vercel"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "auth" {
		if err := runAuth(context.Background(), os.Args[2:]); err != nil {
			log.Fatal(err)
		}
		return
	}

	defaultAddr := "127.0.0.1:8080"
	if v := os.Getenv("MOSSO_LISTEN"); v != "" {
		defaultAddr = v
	}
	addr := flag.String("listen", defaultAddr, "HTTP listen address")
	flag.Parse()
	if err := server.CheckListenAddr(*addr); err != nil {
		log.Fatal(err)
	}

	var client *vercel.Client
	if cfg, err := vercel.ConfigFromEnv(); err != nil {
		log.Printf("sandbox endpoints disabled: %v", err)
	} else {
		client = vercel.NewClient(cfg)
		log.Print("Vercel Sandbox credentials loaded")
	}
	if _, source, err := claude.LoadToken(context.Background()); err != nil {
		log.Printf("Claude Code messages disabled until a token is configured: %v", err)
	} else {
		log.Printf("Claude Code token found (%s)", source)
	}
	if _, source, err := github.LoadToken(context.Background()); err != nil {
		log.Printf("GitHub workspaces disabled until a token is configured: %v", err)
	} else {
		log.Printf("GitHub token found (%s)", source)
	}
	// Fetches and pushes for workspaces run here, not in the sandbox.
	if _, err := exec.LookPath("git"); err != nil {
		log.Print("git is not installed on this machine: workspaces cannot fetch from or push to GitHub until it is (on macOS: xcode-select --install)")
	}
	srv := server.New(client)
	// Workspaces are saved on disk and reconnected to after a restart.
	if dataDir, err := server.DataDir(); err != nil {
		log.Printf("workspaces are not saved between runs: %v", err)
	} else if client == nil {
		log.Printf("workspaces in %s are not restored: Vercel is not configured", dataDir)
	} else if err := srv.Restore(context.Background(), dataDir); err != nil {
		log.Printf("restore workspaces from %s: %v", dataDir, err)
	} else {
		log.Printf("workspaces saved in %s", dataDir)
	}
	httpSrv := &http.Server{Addr: *addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	// Browsers keep event streams open; end them so shutdown need not wait.
	httpSrv.RegisterOnShutdown(srv.CloseStreams)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("mosso listening on %s; open http://%s in your browser", *addr, *addr)
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

	// Give in-flight requests a short grace period, then cut them off so a
	// long-running /exec cannot eat into the time needed for cleanup.
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 10*time.Second)
	if err := httpSrv.Shutdown(drainCtx); err != nil {
		_ = httpSrv.Close()
	}
	cancelDrain()

	// Workspace sandboxes keep running (Claude finishes its task in the
	// sandbox and Mosso catches up on the next start; Vercel stops the
	// session at its timeout). Plain sandboxes are destroyed so they do not
	// keep accruing charges. This uses its own deadline so a slow drain
	// cannot leave it pre-expired.
	destroyCtx, cancelDestroy := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelDestroy()
	if err := srv.Shutdown(destroyCtx); err != nil {
		log.Printf("some sandboxes could not be destroyed; check the Vercel dashboard: %v", err)
	}
	if exitErr != nil {
		log.Fatal(exitErr)
	}
}
