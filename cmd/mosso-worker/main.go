// Command mosso-worker runs inside a Vercel Sandbox. It runs Claude Code on
// behalf of the Mosso server, records everything to a durable event log in
// the sandbox, and answers the server's polls on a loopback HTTP API. See
// docs/worker.md.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/riggleg/mosso/internal/worker"
)

func main() {
	state := flag.String("state", worker.DefaultStateDir, "directory for the event log")
	listen := flag.String("listen", "127.0.0.1:"+strconv.Itoa(worker.DefaultPort), "loopback address of the worker API")
	claudePath := flag.String("claude", "", "path of the claude binary (default $HOME/.local/bin/claude)")
	flag.Parse()
	if *claudePath == "" {
		*claudePath = worker.DefaultClaudePath()
	}
	log.SetFlags(log.LstdFlags | log.LUTC)

	l, err := worker.OpenLog(filepath.Join(*state, "events.jsonl"))
	if err != nil {
		log.Fatal(err)
	}
	defer l.Close()
	srv := worker.New(l, *claudePath)
	log.Printf("mosso-worker %s: %d events recorded, listening on %s", worker.Version, l.Seq(), *listen)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.ListenAndServe(ctx, *listen); err != nil {
		log.Fatal(err)
	}
	log.Print("mosso-worker: shutting down")
}
