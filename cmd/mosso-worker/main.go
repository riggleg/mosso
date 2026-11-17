// Command mosso-worker runs inside a Vercel Sandbox. It runs Claude Code on
// behalf of the Mosso server, records everything to a durable event log in
// the sandbox, and answers the server's polls on a loopback HTTP API. With
// the "mcp" argument, it is instead the MCP server through which Claude
// Code uses Mosso's tools. See docs/worker.md and docs/agent-github.md.
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

	"github.com/riggleg/mosso/internal/claude"
	"github.com/riggleg/mosso/internal/worker"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		mcp(os.Args[2:])
		return
	}
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
	// Images Claude shows are kept in the state directory; files are read
	// from the workspace (and attachments) and the temporary directory.
	tmp := os.TempDir()
	srv.SetImages(filepath.Join(*state, "images"), *state, tmp, claude.WorkDir, tmp)
	// Claude Code starts this binary as its MCP server for Mosso's tools.
	if exe, err := os.Executable(); err == nil {
		srv.SetMCP(exe, "mcp", "-worker", "http://"+*listen)
	} else {
		log.Printf("mosso-worker: Mosso's tools are not offered to Claude: %v", err)
	}
	log.Printf("mosso-worker %s: %d events recorded, listening on %s", worker.Version, l.Seq(), *listen)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.ListenAndServe(ctx, *listen); err != nil {
		log.Fatal(err)
	}
	log.Print("mosso-worker: shutting down")
}

// mcp serves Mosso's tools to Claude Code over stdin and stdout: `mosso-worker
// mcp -worker http://127.0.0.1:7777`. Claude Code starts it; logs go to
// stderr, which Claude Code keeps apart from the protocol.
func mcp(args []string) {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	workerURL := fs.String("worker", "http://127.0.0.1:"+strconv.Itoa(worker.DefaultPort), "the worker API to relay tool calls to")
	_ = fs.Parse(args)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := worker.ServeMCP(ctx, os.Stdin, os.Stdout, *workerURL); err != nil {
		log.Fatal(err)
	}
}
