package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/riggleg/mosso/internal/vercel"
)

// A workspace's setup script customizes its sandbox: it installs system
// packages, mise, languages and tools. Mosso runs it once, when the
// workspace is created, after the clone and before Claude Code is
// installed, the worker starts or the first message runs. A persistent
// sandbox keeps what it installed across sessions, so resuming does not
// run it again.
//
// What it installs must also be found by later, non-interactive commands
// (the worker, Claude Code and its Bash tool), which do not read shell
// startup files. So Mosso keeps the workspace's environment in a file,
// setupEnvFile: before the script runs, it puts ~/.local/bin and mise's
// shims directory on PATH there; the script can append more ($MOSSO_ENV).
// The worker loads the file whenever it starts, and Claude Code inherits
// the worker's environment. Interactive shells source it from ~/.bashrc.

// setupScriptTimeout bounds the setup script. Creating a workspace with
// one may take this much longer than setupTimeout.
const setupScriptTimeout = 20 * time.Minute

const (
	// setupScriptFile is the setup script in the sandbox, kept for
	// reference and to run again by hand; setupLogFile is its full output.
	setupScriptFile = workerDir + "/setup.sh"
	setupLogFile    = workerDir + "/setup.log"
	// setupEnvFile is the workspace's environment; see above.
	setupEnvFile = workerDir + "/env"
)

// setupRunScript prepares the environment file, then runs the setup script
// ($1) in the checkout ($3) with `bash -e -o pipefail`, so it stops at the
// first failing command, with no stdin. Its stderr is merged into stdout,
// which Mosso streams, and copied to the log ($4). The exit code is the
// script's.
const setupRunScript = `set -eo pipefail
script=$1 env=$2 dir=$3 log=$4
mkdir -p "${env%/*}"
if [ ! -e "$env" ]; then
  cat >"$env" <<'EOF'
# The environment of Claude Code, the Mosso worker and shells in this
# workspace, loaded whenever the worker starts. Mosso wrote it before the
# setup script ran; the script can add to it, for example:
#   echo 'export PATH="$HOME/.cargo/bin:$PATH"' >> "$MOSSO_ENV"
export PATH="$HOME/.local/bin:$HOME/.local/share/mise/shims:$PATH"
EOF
fi
if ! grep -qsF "$env" "$HOME/.bashrc"; then
  printf '\n# Added by Mosso: the workspace environment.\n[ -f %q ] && . %q\n' "$env" "$env" >>"$HOME/.bashrc"
fi
. "$env"
export MOSSO_ENV="$env"
cd "$dir"
bash -e -o pipefail "$script" </dev/null 2>&1 | tee "$log"`

// setupEnv is the setup script's whole environment beyond the sandbox's
// own: settings for unattended installs. It never carries credentials;
// the Claude and GitHub tokens are not in the sandbox at this point (a
// private repository's token is only given to the clone command).
var setupEnv = map[string]string{
	"DEBIAN_FRONTEND": "noninteractive",
	"MISE_YES":        "1",
}

const (
	// setupFlushInterval is how often new setup output reaches the chat.
	setupFlushInterval = time.Second
	// maxSetupChatBytes bounds the setup output kept in the chat; the
	// sandbox's setup.log has all of it.
	maxSetupChatBytes = 256 << 10
	// setupTailBytes is how much of the output an error message quotes.
	setupTailBytes = 2048
)

// errSetupFailed is a setup script that ran and failed.
var errSetupFailed = errors.New("the setup script failed")

// runSetup runs the workspace's setup script in its sandbox session,
// showing its output in the chat as it arrives. It returns an error (with
// the end of the output) if the script cannot run or fails.
func (s *Server) runSetup(ctx context.Context, ws *workspace, session string) error {
	start := time.Now()
	s.addEntry(ws.chat, chatEntry{Type: "notice", Text: "Running the setup script…"})
	// Written from the parent directory, which the clone made sure
	// exists: workerDir itself is only created later.
	parent := path.Dir(workerDir)
	if err := s.vercel.WriteFiles(ctx, session, parent, []vercel.File{
		{Name: strings.TrimPrefix(setupScriptFile, parent+"/"), Data: []byte(ws.SetupScript), Mode: 0o755},
	}); err != nil {
		return fmt.Errorf("could not copy the setup script into the sandbox: %w", err)
	}

	out := newSetupOutput(func(text string) { s.addEntry(ws.chat, chatEntry{Type: "setup", Text: text}) })
	ctx, cancel := context.WithTimeout(ctx, setupScriptTimeout)
	defer cancel()
	res, err := s.vercel.Run(ctx, session, vercel.Command{
		Name:   "bash",
		Args:   []string{"-c", setupRunScript, "setup", setupScriptFile, setupEnvFile, ws.Dir, setupLogFile},
		Env:    setupEnv,
		Stdout: out.write,
	})
	out.close()
	took := time.Since(start).Round(time.Second)
	switch {
	case errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil:
		err = fmt.Errorf("%w: it did not finish within %s", errSetupFailed, setupScriptTimeout)
	case err != nil:
		err = fmt.Errorf("could not run the setup script: %w", err)
	case res.ExitCode != 0:
		err = fmt.Errorf("%w with exit code %d after %s", errSetupFailed, res.ExitCode, took)
	}
	if err != nil {
		if tail := out.tail(); tail != "" {
			err = fmt.Errorf("%w. Its output ends with:\n%s", err, tail)
		}
		return err
	}
	log.Printf("workspace %s: setup script finished in %s", ws.Name, took)
	s.addEntry(ws.chat, chatEntry{Type: "notice", Text: "The setup script finished in " + took.String() + "."})
	return nil
}

// setupOutput passes a running setup script's output to the chat in
// batches of whole lines, at most every setupFlushInterval, and keeps its
// end for error messages.
type setupOutput struct {
	emit func(string)

	mu      sync.Mutex
	pending strings.Builder
	shown   int    // bytes passed to emit
	cut     bool   // reached maxSetupChatBytes
	last    []byte // the end of the output
	lastCut bool   // last is not all of it
	done    chan struct{}
	stopped chan struct{}
}

func newSetupOutput(emit func(string)) *setupOutput {
	o := &setupOutput{emit: emit, done: make(chan struct{}), stopped: make(chan struct{})}
	go func() {
		defer close(o.stopped)
		t := time.NewTicker(setupFlushInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				o.flush(false)
			case <-o.done:
				o.flush(true)
				return
			}
		}
	}()
	return o
}

func (o *setupOutput) write(chunk string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.pending.WriteString(chunk)
	o.last = append(o.last, chunk...)
	if extra := len(o.last) - setupTailBytes; extra > 0 {
		o.last = o.last[extra:]
		o.lastCut = true
	}
}

// flush emits the complete lines received so far, or everything if all.
func (o *setupOutput) flush(all bool) {
	o.mu.Lock()
	text := o.pending.String()
	n := len(text)
	if !all {
		n = strings.LastIndexByte(text, '\n') + 1
	}
	o.pending.Reset()
	o.pending.WriteString(text[n:])
	text = text[:n]
	if o.cut {
		text = ""
	} else if room := maxSetupChatBytes - o.shown; len(text) > room {
		text = text[:runeFloor(text, room)] + "\n[mosso: the rest of the output is in " + setupLogFile + " in the sandbox]\n"
		o.cut = true
	}
	o.shown += len(text)
	o.mu.Unlock()
	if text != "" {
		o.emit(text)
	}
}

// close emits what is left and waits until it has been.
func (o *setupOutput) close() {
	close(o.done)
	<-o.stopped
}

// tail returns the end of the output, starting at a line if it can.
func (o *setupOutput) tail() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	s := string(o.last)
	if o.lastCut {
		// Drop the partial line (and partial rune) it starts with.
		if i := strings.IndexByte(s, '\n'); i >= 0 && i < len(s)-1 {
			s = s[i+1:]
		}
		for s != "" && !utf8.RuneStart(s[0]) {
			s = s[1:]
		}
		s = "…\n" + s
	}
	return strings.TrimRight(s, "\n")
}

// runeFloor moves i down to a rune boundary in s.
func runeFloor(s string, i int) int {
	for i > 0 && i < len(s) && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}
