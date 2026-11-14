package server

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/riggleg/mosso/internal/claude"
	"github.com/riggleg/mosso/internal/vercel"
	"github.com/riggleg/mosso/internal/worker"
)

// fakeVercel is an in-memory Vercel Sandbox API with persistent sandboxes:
// a sandbox can be stopped (its files are kept, its processes end), resumed
// into a new session (the files come back, processes do not) and deleted.
// Each sandbox has a real in-process Mosso worker (internal/worker) whose
// event log lives in a temp directory that survives stops, exactly like the
// snapshotted filesystem; the worker's Claude is scripted (fakeClaudeRunner).
type fakeVercel struct {
	mu      sync.Mutex
	tmp     string   // root of per-sandbox worker state directories
	created []string // sandbox names
	deleted []string
	stopped []string // session ids
	failCmd bool
	// cmds records each command request body; stdoutFor, if set, scripts
	// the stdout of a command from its request body.
	cmds      []map[string]any
	stdoutFor func(cmd map[string]any) string
	// exitFor, if set, scripts a command's exit code.
	exitFor func(cmd map[string]any) int
	// files is what was written with fs/write, by sandbox session and
	// absolute path; failWrite makes writes fail. A resumed session
	// inherits its predecessor's files.
	files     map[string]map[string][]byte
	failWrite bool
	// orphanSnapshots records whether each delete also removed orphan
	// snapshots.
	orphanSnapshots []bool

	nextCmd   int
	running   map[string]*fakeCmd     // by command id
	sandboxes map[string]*fakeSandbox // by name
	sessions  map[string]string       // session id -> sandbox name
	workers   map[string]*fakeWorker  // by sandbox name
	claude    *fakeClaudeRunner
	// snapshotLost makes resuming fail with snapshot_not_found.
	snapshotLost map[string]bool
}

type fakeSandbox struct {
	name       string
	session    string
	state      string // running, stopped, deleted
	sessions   int
	persistent bool
	timeout    int64 // ms
	started    time.Time
}

// fakeCmd is one command run in a sandbox.
type fakeCmd struct {
	body   map[string]any
	exit   int
	stdout string
	stderr string
	// stream, if set, writes the command's log lines until it ends, the
	// reader goes away or killed is closed.
	stream func(w http.ResponseWriter, r *http.Request, killed <-chan struct{})
	killed chan struct{}
	done   chan struct{}
}

func (f *fakeVercel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	p := r.URL.Path
	parts := strings.Split(p, "/")
	switch {
	case r.Method == http.MethodPost && p == "/v2/sandboxes":
		f.createSandbox(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/v2/sandboxes/") && len(parts) == 4:
		f.getSandbox(w, r, parts[3])
	case r.Method == http.MethodDelete && strings.HasPrefix(p, "/v2/sandboxes/") && len(parts) == 4:
		f.deleteSandbox(w, r, parts[3])
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/stop"):
		f.stopSession(w, parts[4])
	case strings.HasPrefix(p, "/v2/sandboxes/sessions/"):
		if !f.sessionRunning(w, parts[4]) {
			return
		}
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/fs/write"):
			f.writeFiles(w, r, parts[4])
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/fs/read"):
			f.readFile(w, r, parts[4])
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/cmd"):
			f.startCommand(w, r, parts[4])
		case r.Method == http.MethodGet && len(parts) == 7 && parts[5] == "cmd":
			f.waitCommand(w, parts[6])
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/logs"):
			f.commandLogs(w, r, parts[6])
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/kill"):
			f.killCommand(w, parts[6])
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeVercel) sandboxJSON(sb *fakeSandbox, resumed bool) string {
	status := "running"
	if sb.state != "running" {
		status = "stopped"
	}
	return fmt.Sprintf(`{"sandbox":{"name":%q,"persistent":%t,"currentSessionId":%q,"status":%q,"timeout":%d,"createdAt":%d},`+
		`"session":{"id":%q,"status":%q,"timeout":%d,"startedAt":%d},"routes":[],"resumed":%t}`,
		sb.name, sb.persistent, sb.session, status, sb.timeout, sb.started.UnixMilli(),
		sb.session, status, sb.timeout, sb.started.UnixMilli(), resumed)
}

func (f *fakeVercel) createSandbox(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name       string `json:"name"`
		Persistent bool   `json:"persistent"`
		Timeout    int64  `json:"timeout"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, body.Name)
	sb := &fakeSandbox{name: body.Name, session: "sbx_" + body.Name, state: "running", sessions: 1,
		persistent: body.Persistent, timeout: body.Timeout, started: time.Now()}
	if f.sandboxes == nil {
		f.sandboxes = map[string]*fakeSandbox{}
		f.sessions = map[string]string{}
	}
	f.sandboxes[sb.name] = sb
	f.sessions[sb.session] = sb.name
	_, _ = io.WriteString(w, f.sandboxJSON(sb, false))
}

func (f *fakeVercel) getSandbox(w http.ResponseWriter, r *http.Request, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sb := f.sandboxes[name]
	if sb == nil || sb.state == "deleted" {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"sandbox not found"}}`)
		return
	}
	resumed := false
	if r.URL.Query().Get("resume") == "true" && sb.state == "stopped" {
		if f.snapshotLost[name] || !sb.persistent {
			w.WriteHeader(http.StatusGone)
			_, _ = io.WriteString(w, `{"error":{"code":"snapshot_not_found","message":"the snapshot is gone"}}`)
			return
		}
		old := sb.session
		sb.sessions++
		sb.session = fmt.Sprintf("sbx_%s_%d", sb.name, sb.sessions)
		sb.state, sb.started, resumed = "running", time.Now(), true
		f.sessions[sb.session] = sb.name
		// The snapshot brings the files back.
		if f.files[old] != nil {
			if f.files[sb.session] == nil {
				f.files[sb.session] = map[string][]byte{}
			}
			for k, v := range f.files[old] {
				f.files[sb.session][k] = v
			}
		}
	}
	_, _ = io.WriteString(w, f.sandboxJSON(sb, resumed))
}

func (f *fakeVercel) deleteSandbox(w http.ResponseWriter, r *http.Request, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, name)
	f.orphanSnapshots = append(f.orphanSnapshots, r.URL.Query().Get("deleteOrphanSnapshots") == "true")
	if sb := f.sandboxes[name]; sb != nil {
		sb.state = "deleted"
		if fw := f.workers[name]; fw != nil {
			fw.stop()
		}
	}
	_, _ = io.WriteString(w, `{"sandbox":{}}`)
}

func (f *fakeVercel) stopSession(w http.ResponseWriter, session string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, session)
	if name, ok := f.sessions[session]; ok {
		f.stopLocked(name)
	}
	_, _ = io.WriteString(w, `{"session":{}}`)
}

// stopLocked ends a sandbox's session: its processes (the worker) die, its
// files are kept.
func (f *fakeVercel) stopLocked(name string) {
	sb := f.sandboxes[name]
	if sb == nil || sb.state != "running" {
		return
	}
	sb.state = "stopped"
	if fw := f.workers[name]; fw != nil {
		fw.stop()
	}
}

// timeout simulates Vercel stopping the sandbox's session at its timeout.
func (f *fakeVercel) timeout(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopLocked(name)
}

// lose simulates the sandbox disappearing behind Mosso's back: deleted from
// the dashboard, or removed by Vercel after its snapshot expired.
func (f *fakeVercel) lose(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sb := f.sandboxes[name]; sb != nil {
		sb.state = "deleted"
		if fw := f.workers[name]; fw != nil {
			fw.stop()
		}
	}
}

// session returns the sandbox's current session ID.
func (f *fakeVercel) session(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sb := f.sandboxes[name]; sb != nil {
		return sb.session
	}
	return ""
}

// sessionRunning answers 410 (stopped) or 404 (unknown) for a session that
// cannot run commands, and reports whether it can.
func (f *fakeVercel) sessionRunning(w http.ResponseWriter, session string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	name, ok := f.sessions[session]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"session not found"}}`)
		return false
	}
	if sb := f.sandboxes[name]; sb.state != "running" || sb.session != session {
		w.WriteHeader(http.StatusGone)
		_, _ = io.WriteString(w, `{"error":{"code":"sandbox_stopped","message":"the session has stopped"}}`)
		return false
	}
	return true
}

func (f *fakeVercel) writeFiles(w http.ResponseWriter, r *http.Request, session string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWrite {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"disk full"}}`)
		return
	}
	gz, err := gzip.NewReader(r.Body)
	if err != nil || r.Header.Get("Content-Type") != "application/gzip" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		data, _ := io.ReadAll(tr)
		if f.files == nil {
			f.files = map[string]map[string][]byte{}
		}
		if f.files[session] == nil {
			f.files[session] = map[string][]byte{}
		}
		f.files[session][path.Join(r.Header.Get("x-cwd"), hdr.Name)] = data
	}
	_, _ = io.WriteString(w, `{}`)
}

func (f *fakeVercel) readFile(w http.ResponseWriter, r *http.Request, session string) {
	var body struct {
		Path string `json:"path"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	data, ok := f.files[session][body.Path]
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"message":"no such file"}}`)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(data)
}

// startCommand runs a command. Worker calls are served by the sandbox's
// in-process worker; everything else is scripted by stdoutFor/exitFor.
func (f *fakeVercel) startCommand(w http.ResponseWriter, r *http.Request, session string) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	if f.failCmd {
		f.mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"cmd failed"}}`)
		return
	}
	f.cmds = append(f.cmds, body)
	f.nextCmd++
	id := "cmd_" + strconv.Itoa(f.nextCmd)
	cmd := &fakeCmd{body: body, killed: make(chan struct{}), done: make(chan struct{})}
	if f.running == nil {
		f.running = map[string]*fakeCmd{}
	}
	f.running[id] = cmd
	name := f.sessions[session]
	fw := f.workerFor(name)
	f.mu.Unlock()

	args := argsOf(body)
	script := ""
	if len(args) > 1 {
		script = args[1]
	}
	switch script {
	case workerCallScript:
		cmd.exit, cmd.stdout, cmd.stderr = fw.call(args[3], args[4], envOf(body)["MOSSO_BODY"])
		close(cmd.done)
	case workerProbeScript:
		f.mu.Lock()
		_, present := f.files[session][args[4]]
		f.mu.Unlock()
		cmd.stdout = "missing\n"
		if present {
			cmd.stdout = "present\n"
		}
		close(cmd.done)
	case workerStartScript:
		f.mu.Lock()
		_, present := f.files[session][args[3]]
		f.mu.Unlock()
		if !present {
			cmd.exit, cmd.stderr = 127, "no such binary: "+args[3]
		} else if fw.start() {
			cmd.stdout = "started\n"
		} else {
			cmd.stdout = "running\n"
		}
		close(cmd.done)
	case workerFollowScript:
		cmd.stream = func(w http.ResponseWriter, r *http.Request, killed <-chan struct{}) {
			cmd.exit = fw.follow(w, r, args[3], killed)
		}
	default:
		f.mu.Lock()
		if f.stdoutFor != nil {
			cmd.stdout = f.stdoutFor(body)
		} else {
			cmd.stdout = "hello\n"
		}
		if f.exitFor != nil {
			cmd.exit = f.exitFor(body)
		}
		f.mu.Unlock()
		close(cmd.done)
	}
	_, _ = fmt.Fprintf(w, `{"command":{"id":%q,"exitCode":null}}`, id)
}

func (f *fakeVercel) command(id string) *fakeCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running[id]
}

func (f *fakeVercel) waitCommand(w http.ResponseWriter, id string) {
	cmd := f.command(id)
	if cmd == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	<-cmd.done
	_, _ = fmt.Fprintf(w, `{"command":{"id":%q,"exitCode":%d}}`, id, cmd.exit)
}

func (f *fakeVercel) commandLogs(w http.ResponseWriter, r *http.Request, id string) {
	cmd := f.command(id)
	if cmd == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if cmd.stream != nil {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		cmd.stream(w, r, cmd.killed)
		close(cmd.done)
		return
	}
	enc := json.NewEncoder(w)
	_ = enc.Encode(map[string]string{"stream": "stdout", "data": cmd.stdout})
	if cmd.stderr != "" {
		_ = enc.Encode(map[string]string{"stream": "stderr", "data": cmd.stderr})
	}
}

func (f *fakeVercel) killCommand(w http.ResponseWriter, id string) {
	if cmd := f.command(id); cmd != nil {
		select {
		case <-cmd.killed:
		default:
			close(cmd.killed)
		}
	}
	_, _ = io.WriteString(w, `{"command":{}}`)
}

func argsOf(body map[string]any) []string {
	raw, _ := body["args"].([]any)
	out := make([]string, len(raw))
	for i, a := range raw {
		out[i], _ = a.(string)
	}
	return out
}

func envOf(body map[string]any) map[string]string {
	raw, _ := body["env"].(map[string]any)
	out := map[string]string{}
	for k, v := range raw {
		out[k], _ = v.(string)
	}
	return out
}

// workerFor returns the sandbox's worker, creating its state directory
// (which, like the sandbox's filesystem, outlives sessions). Call with
// f.mu held.
func (f *fakeVercel) workerFor(name string) *fakeWorker {
	if f.workers == nil {
		f.workers = map[string]*fakeWorker{}
	}
	fw := f.workers[name]
	if fw == nil {
		fw = &fakeWorker{dir: filepath.Join(f.tmp, name), claude: f.claude}
		f.workers[name] = fw
	}
	return fw
}

// worker returns the sandbox's worker for inspection.
func (f *fakeVercel) worker(name string) *fakeWorker {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.workerFor(name)
}

// fakeWorker hosts a real worker.Server for one sandbox. It is "running"
// while it has an HTTP server; a session stop ends it, a start after a
// resume brings it back over the same event log.
type fakeWorker struct {
	mu     sync.Mutex
	dir    string
	claude *fakeClaudeRunner
	srv    *httptest.Server
	w      *worker.Server
	log    *worker.Log
	starts int
}

func (fw *fakeWorker) start() bool {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if fw.srv != nil {
		return false
	}
	l, err := worker.OpenLog(filepath.Join(fw.dir, "events.jsonl"))
	if err != nil {
		panic(err)
	}
	fw.log = l
	fw.w = worker.New(l, "/nonexistent/claude")
	fw.w.SetRunner(fw.claude.run)
	fw.srv = httptest.NewServer(fw.w.Handler())
	fw.starts++
	return true
}

func (fw *fakeWorker) stop() {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if fw.srv == nil {
		return
	}
	fw.srv.CloseClientConnections()
	fw.srv.Close()
	fw.log.Close()
	fw.srv, fw.w, fw.log = nil, nil, nil
}

func (fw *fakeWorker) running() bool {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	return fw.srv != nil
}

func (fw *fakeWorker) url() string {
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if fw.srv == nil {
		return ""
	}
	return fw.srv.URL
}

// call performs one request like workerCallScript does: body, then the
// status code; exit 7 when the worker is not running.
func (fw *fakeWorker) call(method, url, body string) (int, string, string) {
	base := fw.url()
	if base == "" {
		return 7, "", "curl: (7) Failed to connect"
	}
	url = strings.Replace(url, workerBase, base, 1)
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body + "\n")
	}
	req, _ := http.NewRequest(method, url, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 7, "", err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return 0, strings.TrimRight(string(b), "\n") + "\n" + strconv.Itoa(resp.StatusCode) + "\n", ""
}

// follow relays the worker's event stream as command log lines until the
// worker closes it or the command is killed.
func (fw *fakeWorker) follow(w http.ResponseWriter, r *http.Request, url string, killed <-chan struct{}) int {
	base := fw.url()
	if base == "" {
		return 7
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		select {
		case <-killed:
			cancel()
		case <-ctx.Done():
		}
	}()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.Replace(url, workerBase, base, 1), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 7
	}
	defer resp.Body.Close()
	enc := json.NewEncoder(w)
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(nil, 16<<20)
	for sc.Scan() {
		_ = enc.Encode(map[string]string{"stream": "stdout", "data": sc.Text() + "\n"})
		w.(http.Flusher).Flush()
	}
	select {
	case <-killed:
		return 143
	default:
		return 0
	}
}

// fakeClaudeRunner scripts Claude inside the fake workers: it reports the
// session ID (the one being resumed, or testSessionID), runs one tool, and
// answers "you said <text>". With block set, it waits after the tool until
// the channel is closed or it is stopped.
type fakeClaudeRunner struct {
	mu      sync.Mutex
	tasks   []worker.Message
	block   chan struct{}
	started chan struct{} // receives once per run that starts
	fail    bool          // report an error result, like an expired token
}

func (fc *fakeClaudeRunner) run(ctx context.Context, msg worker.Message, emit func(worker.Event)) (*claude.Turn, error) {
	fc.mu.Lock()
	fc.tasks = append(fc.tasks, msg)
	block, started, fail := fc.block, fc.started, fc.fail
	fc.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	sid := msg.SessionID
	if sid == "" {
		sid = testSessionID
	}
	emit(worker.Event{Type: worker.EventSession, SessionID: sid})
	emit(worker.Event{Type: worker.EventClaude, Event: &claude.Event{Type: "tool_use", ToolUseID: "toolu_1", Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`)}})
	emit(worker.Event{Type: worker.EventClaude, Event: &claude.Event{Type: "tool_result", ToolUseID: "toolu_1", Content: "README.md"}})
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if fail {
		return &claude.Turn{SessionID: sid, Result: "Invalid API key · Please run /login", IsError: true, Subtype: "success"}, nil
	}
	return &claude.Turn{SessionID: sid, Model: "claude-test", Result: "you said " + msg.Text, Subtype: "success", NumTurns: 2}, nil
}

// last returns the most recent task Claude ran.
func (fc *fakeClaudeRunner) last() worker.Message {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(fc.tasks) == 0 {
		return worker.Message{}
	}
	return fc.tasks[len(fc.tasks)-1]
}

func (fc *fakeClaudeRunner) count() int {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return len(fc.tasks)
}

// blockRuns makes the next Claude runs wait; the returned channels report
// each start and release them all.
func (fc *fakeClaudeRunner) blockRuns() (started chan struct{}, release chan struct{}) {
	started, release = make(chan struct{}, 10), make(chan struct{})
	fc.mu.Lock()
	fc.block, fc.started = release, started
	fc.mu.Unlock()
	return started, release
}

// fakeWorkerBinary stands in for the real mosso-worker build.
var fakeWorkerBinary = []byte("#!/bin/sh\necho fake mosso-worker\n")

// newFakeVercel returns a fake with its own state directory.
func newFakeVercel(t *testing.T) (*fakeVercel, *httptest.Server) {
	t.Helper()
	fake := &fakeVercel{tmp: t.TempDir(), claude: &fakeClaudeRunner{}}
	api := httptest.NewServer(fake)
	t.Cleanup(func() {
		// Ending the workers ends any follow stream the API is serving.
		fake.mu.Lock()
		for _, fw := range fake.workers {
			fw.stop()
		}
		fake.mu.Unlock()
		api.CloseClientConnections()
		api.Close()
	})
	return fake, api
}

// newServerOver returns a server for the fake, saving workspaces in dataDir.
func newServerOver(t *testing.T, fake *fakeVercel, api *httptest.Server, dataDir string) *Server {
	t.Helper()
	client := vercel.NewClient(vercel.Config{Token: "tok", TeamID: "team", ProjectID: "prj", BaseURL: api.URL})
	srv := New(client)
	srv.workerBinary = func(context.Context) ([]byte, error) { return fakeWorkerBinary, nil }
	if err := srv.Restore(context.Background(), dataDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.cancel() })
	return srv
}

func newTestServer(t *testing.T) (*Server, *fakeVercel) {
	t.Helper()
	fake, api := newFakeVercel(t)
	return newServerOver(t, fake, api, t.TempDir()), fake
}

func do(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "http://127.0.0.1:8080"+path, strings.NewReader(body))
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestSandboxLifecycle(t *testing.T) {
	srv, fake := newTestServer(t)
	h := srv.Handler()

	rec := do(t, h, http.MethodPost, "/sandboxes", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", rec.Code, rec.Body)
	}
	var sb vercel.Sandbox
	if err := json.Unmarshal(rec.Body.Bytes(), &sb); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sb.Name, "mosso-") || sb.SessionID != "sbx_"+sb.Name {
		t.Fatalf("unexpected sandbox: %+v", sb)
	}

	rec = do(t, h, http.MethodGet, "/sandboxes", "")
	if !strings.Contains(rec.Body.String(), sb.Name) {
		t.Fatalf("list missing sandbox: %s", rec.Body)
	}

	rec = do(t, h, http.MethodPost, "/sandboxes/"+sb.Name+"/exec", `{"command":"echo hello"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("exec status = %d: %s", rec.Code, rec.Body)
	}
	var res vercel.CommandResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "hello\n" || res.Stderr != "" || res.ExitCode != 0 {
		t.Fatalf("unexpected exec result: %+v", res)
	}

	rec = do(t, h, http.MethodDelete, "/sandboxes/"+sb.Name, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("destroy status = %d: %s", rec.Code, rec.Body)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != sb.Name || len(fake.stopped) != 1 {
		t.Fatalf("deleted=%v stopped=%v", fake.deleted, fake.stopped)
	}
	if rec = do(t, h, http.MethodPost, "/sandboxes/"+sb.Name+"/exec", `{"command":"true"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("exec after destroy status = %d", rec.Code)
	}
}

func TestExecValidationAndErrors(t *testing.T) {
	srv, fake := newTestServer(t)
	h := srv.Handler()
	rec := do(t, h, http.MethodPost, "/sandboxes", `{"timeout_seconds":60}`)
	var sb vercel.Sandbox
	_ = json.Unmarshal(rec.Body.Bytes(), &sb)

	if rec := do(t, h, http.MethodPost, "/sandboxes/"+sb.Name+"/exec", `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty command status = %d", rec.Code)
	}
	if rec := do(t, h, http.MethodPost, "/sandboxes/nope/exec", `{"command":"true"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown sandbox status = %d", rec.Code)
	}
	fake.mu.Lock()
	fake.failCmd = true
	fake.mu.Unlock()
	if rec := do(t, h, http.MethodPost, "/sandboxes/"+sb.Name+"/exec", `{"command":"true"}`); rec.Code != http.StatusBadGateway {
		t.Fatalf("upstream failure status = %d", rec.Code)
	}
}

func TestCreateRejectsBadTimeout(t *testing.T) {
	srv, fake := newTestServer(t)
	for _, body := range []string{`{"timeout_seconds":-1}`, `{"timeout_seconds":2701}`} {
		if rec := do(t, srv.Handler(), http.MethodPost, "/sandboxes", body); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d", body, rec.Code)
		}
	}
	if rec := do(t, srv.Handler(), http.MethodPost, "/sandboxes", `{"timeout_seconds":2700}`); rec.Code != http.StatusCreated {
		t.Fatalf("45 minutes: status = %d", rec.Code)
	}
	if len(fake.created) != 1 {
		t.Fatalf("created %d sandboxes, want only the valid request to create one", len(fake.created))
	}
}

func TestShutdownDestroysTrackedSandboxes(t *testing.T) {
	srv, fake := newTestServer(t)
	h := srv.Handler()
	do(t, h, http.MethodPost, "/sandboxes", "")
	do(t, h, http.MethodPost, "/sandboxes", "")
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 2 {
		t.Fatalf("deleted = %v, want 2 sandboxes", fake.deleted)
	}
	if rec := do(t, h, http.MethodGet, "/sandboxes", ""); !strings.Contains(rec.Body.String(), `"sandboxes":[]`) {
		t.Fatalf("registry not cleared: %s", rec.Body)
	}
}

func TestSandboxEndpointsWithoutCredentials(t *testing.T) {
	rec := do(t, NewHandler(), http.MethodPost, "/sandboxes", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestCreateAfterShutdownIsDestroyed(t *testing.T) {
	srv, fake := newTestServer(t)
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Simulates a create request that was in flight when Shutdown ran.
	rec := do(t, srv.Handler(), http.MethodPost, "/sandboxes", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body)
	}
	if len(fake.created) != 1 || len(fake.deleted) != 1 || fake.deleted[0] != fake.created[0] {
		t.Fatalf("created=%v deleted=%v, want the late sandbox destroyed", fake.created, fake.deleted)
	}
}

func TestSandboxLimit(t *testing.T) {
	srv, fake := newTestServer(t)
	h := srv.Handler()
	var last vercel.Sandbox
	for i := 0; i < maxSandboxes; i++ {
		rec := do(t, h, http.MethodPost, "/sandboxes", "")
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %d: status = %d", i, rec.Code)
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &last)
	}
	if rec := do(t, h, http.MethodPost, "/sandboxes", ""); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("create over limit: status = %d, want 429", rec.Code)
	}
	if len(fake.created) != maxSandboxes {
		t.Fatalf("Vercel saw %d creates, want %d", len(fake.created), maxSandboxes)
	}
	// Destroying one frees a slot.
	if rec := do(t, h, http.MethodDelete, "/sandboxes/"+last.Name, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("destroy status = %d", rec.Code)
	}
	if rec := do(t, h, http.MethodPost, "/sandboxes", ""); rec.Code != http.StatusCreated {
		t.Fatalf("create after destroy: status = %d", rec.Code)
	}
}

func TestSandboxLimitConcurrent(t *testing.T) {
	srv, fake := newTestServer(t)
	h := srv.Handler()
	var wg sync.WaitGroup
	for i := 0; i < 3*maxSandboxes; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			do(t, h, http.MethodPost, "/sandboxes", "")
		}()
	}
	wg.Wait()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.created) > maxSandboxes {
		t.Fatalf("Vercel saw %d creates, want at most %d", len(fake.created), maxSandboxes)
	}
}

// readDir lists a directory's entries, for store assertions.
func readDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
