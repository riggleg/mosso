package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/riggleg/mosso/internal/claude"
	"github.com/riggleg/mosso/internal/vercel"
	"github.com/riggleg/mosso/internal/worker"
)

// The worker's API listens on loopback inside the sandbox, so it is not
// reachable from the internet. Mosso talks to it by running curl through
// the Vercel Sandbox command API: a request is one short command, and
// following the event stream is one long-running command whose output
// Mosso reads as it is produced. Request bodies, which carry the Claude
// token, travel in the command's environment, never in its arguments.

// workerDir is the worker's state directory in the sandbox. It lives in
// the sandbox's persisted filesystem, outside the repository checkout.
const workerDir = worker.DefaultStateDir

var workerBase = "http://127.0.0.1:" + strconv.Itoa(worker.DefaultPort)

// workerCallScript sends one request ($1 method, $2 URL) and prints the
// response body, then the status code on its own line. The body, if any,
// comes from $MOSSO_BODY on stdin.
const workerCallScript = `set -u
method=$1 url=$2
if [ -n "${MOSSO_BODY-}" ]; then
  exec curl -sS --max-time 30 -X "$method" -H 'Content-Type: application/json' --data-binary @- -o - -w '\n%{http_code}' "$url" <<<"$MOSSO_BODY"
fi
exec curl -sS --max-time 30 -X "$method" -o - -w '\n%{http_code}' "$url"`

// workerFollowScript streams events from the worker until the worker
// closes the stream, the command is killed, or followMaxTime passes (then
// Mosso simply follows again from where it was).
const workerFollowScript = `exec curl -sSN --max-time "$2" "$1"`

const followMaxTime = 10 * time.Minute

// workerStartScript starts the worker ($1 binary, $2 state directory, $3
// port) unless one already answers, and waits until it does. The worker is
// detached from the command, so it keeps running after the command ends
// and after Mosso disconnects.
const workerStartScript = `set -euo pipefail
bin=$1 state=$2 port=$3
mkdir -p "$state"
chmod +x "$bin"
if curl -sf --max-time 5 "http://127.0.0.1:$port/status" >/dev/null 2>&1; then echo running; exit 0; fi
setsid nohup "$bin" -state "$state" -listen "127.0.0.1:$port" >>"$state/worker.log" 2>&1 </dev/null &
for _ in $(seq 1 50); do
  if curl -sf --max-time 2 "http://127.0.0.1:$port/status" >/dev/null 2>&1; then echo started; exit 0; fi
  sleep 0.2
done
echo "the worker did not start; its log ends with:" >&2
tail -n 20 "$state/worker.log" >&2 || true
exit 1`

// workerProbeScript creates the state directory and reports whether the
// worker binary ($1) is already there.
const workerProbeScript = `set -u
mkdir -p "$1/bin" && if [ -x "$2" ]; then echo present; else echo missing; fi`

// errWorkerDown means nothing answered on the worker's port.
var errWorkerDown = errors.New("the worker is not running in the sandbox")

// workerResponse is the worker's answer to one request.
type workerResponse struct {
	Status int
	Body   []byte
}

// errorMessage returns the worker's error message, if the body has one.
func (r workerResponse) errorMessage() string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(r.Body, &e) == nil && e.Error != "" {
		return e.Error
	}
	return strings.TrimSpace(string(r.Body))
}

// workerCall sends one request to the worker in the sandbox session.
// Vercel errors come back as they are (see vercel.SessionOver); a worker
// that does not answer is errWorkerDown.
func (s *Server) workerCall(ctx context.Context, sessionID, method, path string, body any) (workerResponse, error) {
	cmd := vercel.Command{Name: "bash", Args: []string{"-c", workerCallScript, "call", method, workerBase + path}}
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return workerResponse{}, err
		}
		// One environment variable is limited to 128 KiB on Linux.
		if len(b) > 120<<10 {
			return workerResponse{}, errors.New("the message is too large to send to the worker")
		}
		cmd.Env = map[string]string{"MOSSO_BODY": string(b)}
	}
	res, err := s.vercel.Run(ctx, sessionID, cmd)
	if err != nil {
		return workerResponse{}, err
	}
	return parseWorkerResponse(res)
}

func parseWorkerResponse(res *vercel.CommandResult) (workerResponse, error) {
	switch res.ExitCode {
	case 0:
	case 7:
		return workerResponse{}, errWorkerDown
	default:
		return workerResponse{}, fmt.Errorf("call the worker: curl exit code %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	out := strings.TrimRight(res.Stdout, "\n")
	i := strings.LastIndex(out, "\n")
	codeText := out[i+1:]
	code, err := strconv.Atoi(strings.TrimSpace(codeText))
	if err != nil {
		return workerResponse{}, fmt.Errorf("call the worker: unexpected output %q", codeText)
	}
	var body []byte
	if i >= 0 {
		body = []byte(out[:i])
	}
	return workerResponse{Status: code, Body: body}, nil
}

// workerStatus asks the worker how it is doing.
func (s *Server) workerStatus(ctx context.Context, sessionID string) (worker.Status, error) {
	res, err := s.workerCall(ctx, sessionID, http.MethodGet, "/status", nil)
	if err != nil {
		return worker.Status{}, err
	}
	var st worker.Status
	if res.Status != http.StatusOK || json.Unmarshal(res.Body, &st) != nil {
		return st, fmt.Errorf("worker status: HTTP %d: %s", res.Status, res.errorMessage())
	}
	return st, nil
}

// ensureWorker makes sure the worker runs in the sandbox session and
// returns its status. If nothing answers, it uploads the worker binary
// (unless this build's is already there) and starts it detached. A worker
// started after a sandbox resume recovers its event log from the
// filesystem.
func (s *Server) ensureWorker(ctx context.Context, name, sessionID string) (worker.Status, error) {
	st, err := s.workerStatus(ctx, sessionID)
	if !errors.Is(err, errWorkerDown) {
		return st, err
	}
	bin, err := s.workerBinary(ctx)
	if err != nil {
		return st, err
	}
	rel := "bin/mosso-worker-" + worker.BinaryID(bin)
	path := workerDir + "/" + rel
	res, err := s.vercel.Run(ctx, sessionID, vercel.Command{Name: "bash", Args: []string{"-c", workerProbeScript, "probe", workerDir, path}})
	if err != nil {
		return st, err
	}
	if !strings.Contains(res.Stdout, "present") {
		if err := s.vercel.WriteFiles(ctx, sessionID, workerDir, []vercel.File{{Name: rel, Data: bin, Mode: 0o755}}); err != nil {
			return st, fmt.Errorf("upload the worker: %w", err)
		}
		log.Printf("workspace %s: uploaded the worker (%d bytes)", name, len(bin))
	}
	res, err = s.vercel.Run(ctx, sessionID, vercel.Command{Name: "bash",
		Args: []string{"-c", workerStartScript, "start", path, workerDir, strconv.Itoa(worker.DefaultPort)}})
	if err != nil {
		return st, err
	}
	if res.ExitCode != 0 {
		return st, fmt.Errorf("start the worker: exit code %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	log.Printf("workspace %s: worker %s", name, strings.TrimSpace(res.Stdout))
	return s.workerStatus(ctx, sessionID)
}

// workerFollow streams the worker's events after seq to fn until the
// stream ends: the worker closed it, followMaxTime passed (not an error:
// follow again), ctx ended, or the sandbox or worker went away.
func (s *Server) workerFollow(ctx context.Context, sessionID string, after int64, fn func(worker.Event)) error {
	url := fmt.Sprintf("%s/events?after=%d&follow=1", workerBase, after)
	lines := claude.NewLineSplitter(func(line string) {
		var e worker.Event
		if json.Unmarshal([]byte(line), &e) != nil || e.Type == worker.EventPing || e.Seq == 0 {
			return
		}
		fn(e)
	})
	res, err := s.vercel.Run(ctx, sessionID, vercel.Command{
		Name:   "bash",
		Args:   []string{"-c", workerFollowScript, "follow", url, strconv.Itoa(int(followMaxTime.Seconds()))},
		Stdout: lines.Write,
	})
	if err != nil {
		return err
	}
	lines.Flush()
	switch res.ExitCode {
	case 0, 28: // 28: --max-time passed
		return nil
	case 7:
		return errWorkerDown
	default:
		return fmt.Errorf("follow the worker's events: curl exit code %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
}

// workerSubmit sends a message to the worker, which starts Claude Code and
// answers at once.
func (s *Server) workerSubmit(ctx context.Context, sessionID string, msg worker.Message) (worker.Accepted, error) {
	res, err := s.workerCall(ctx, sessionID, http.MethodPost, "/messages", msg)
	if err != nil {
		return worker.Accepted{}, err
	}
	var acc worker.Accepted
	switch {
	case res.Status == http.StatusConflict:
		return acc, errors.New("Claude is already running in the sandbox; wait for it to finish")
	case res.Status != http.StatusAccepted:
		return acc, fmt.Errorf("the worker refused the message: HTTP %d: %s", res.Status, res.errorMessage())
	}
	if err := json.Unmarshal(res.Body, &acc); err != nil || acc.Seq == 0 {
		return acc, errors.New("the worker's answer was not understood")
	}
	return acc, nil
}

// workerStop asks the worker to interrupt the running task.
func (s *Server) workerStop(ctx context.Context, sessionID string) error {
	res, err := s.workerCall(ctx, sessionID, http.MethodPost, "/stop", nil)
	if err != nil {
		return err
	}
	if res.Status != http.StatusAccepted && res.Status != http.StatusConflict {
		return fmt.Errorf("stop: HTTP %d: %s", res.Status, res.errorMessage())
	}
	return nil
}
