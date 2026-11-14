package vercel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "test-token-value"

type recordedRequest struct {
	Method      string
	Path        string
	Query       map[string][]string
	ContentType string
	Header      http.Header
	RawBody     string
	Body        map[string]any
}

// fakeAPI is a scripted stand-in for the Vercel Sandbox API.
type fakeAPI struct {
	t        *testing.T
	mu       sync.Mutex
	requests []recordedRequest
	routes   map[string]http.HandlerFunc // key: "METHOD /path"
}

func newFakeAPI(t *testing.T) (*fakeAPI, *Client) {
	t.Helper()
	f := &fakeAPI{t: t, routes: map[string]http.HandlerFunc{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, NewClient(Config{Token: testToken, TeamID: "team_1", ProjectID: "prj_1", BaseURL: srv.URL})
}

func (f *fakeAPI) handle(route string, h http.HandlerFunc) { f.routes[route] = h }

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
		f.t.Errorf("%s %s: Authorization = %q", r.Method, r.URL.Path, got)
	}
	if got := r.URL.Query().Get("teamId"); got != "team_1" {
		f.t.Errorf("%s %s: teamId = %q", r.Method, r.URL.Path, got)
	}
	rec := recordedRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), ContentType: r.Header.Get("Content-Type"), Header: r.Header}
	b, _ := io.ReadAll(r.Body)
	rec.RawBody = string(b)
	if len(b) > 0 {
		_ = json.Unmarshal(b, &rec.Body)
	}
	f.mu.Lock()
	f.requests = append(f.requests, rec)
	f.mu.Unlock()
	// Like the real API, reject POSTs that are not a JSON body.
	if r.Method == http.MethodPost && rec.ContentType != "application/gzip" && (rec.ContentType != "application/json" || !json.Valid(b)) {
		http.Error(w, `{"error":{"message":"Unsupported Media Type"}}`, http.StatusUnsupportedMediaType)
		return
	}
	h, ok := f.routes[r.Method+" "+r.URL.Path]
	if !ok {
		http.Error(w, `{"error":{"message":"not found"}}`, http.StatusNotFound)
		return
	}
	h(w, r)
}

func (f *fakeAPI) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.requests {
		out = append(out, r.Method+" "+r.Path)
	}
	return out
}

func reply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("VERCEL_TOKEN", "secret-value")
	t.Setenv("VERCEL_TEAM_ID", "")
	t.Setenv("VERCEL_PROJECT_ID", "")
	_, err := ConfigFromEnv()
	if !errors.Is(err, ErrMissingConfig) {
		t.Fatalf("err = %v, want ErrMissingConfig", err)
	}
	if strings.Contains(err.Error(), "secret-value") {
		t.Fatal("error message leaks the token")
	}
	if !strings.Contains(err.Error(), "VERCEL_TEAM_ID") || !strings.Contains(err.Error(), "VERCEL_PROJECT_ID") {
		t.Fatalf("error should name missing variables: %v", err)
	}

	t.Setenv("VERCEL_TEAM_ID", "team_1")
	t.Setenv("VERCEL_PROJECT_ID", "prj_1")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "secret-value" || cfg.TeamID != "team_1" || cfg.ProjectID != "prj_1" {
		t.Fatalf("unexpected config: team=%q project=%q", cfg.TeamID, cfg.ProjectID)
	}
}

func TestCreate(t *testing.T) {
	f, c := newFakeAPI(t)
	f.handle("POST /v2/sandboxes", reply(200, `{
		"sandbox": {"name": "mosso-abc", "persistent": false, "currentSessionId": "sbx_1", "status": "pending"},
		"session": {"id": "sbx_1", "status": "pending"},
		"routes": []
	}`))
	sb, err := c.Create(context.Background(), "mosso-abc", CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if sb.Name != "mosso-abc" || sb.SessionID != "sbx_1" || sb.Status != "pending" {
		t.Fatalf("unexpected sandbox: %+v", sb)
	}
	body := f.requests[0].Body
	if body["projectId"] != "prj_1" || body["name"] != "mosso-abc" || body["runtime"] != DefaultRuntime ||
		body["persistent"] != false || body["timeout"] != float64(DefaultTimeout.Milliseconds()) {
		t.Fatalf("unexpected create body: %v", body)
	}
}

func TestCreateFailureCleansUp(t *testing.T) {
	f, c := newFakeAPI(t)
	f.handle("POST /v2/sandboxes", reply(500, `{"error":{"message":"boom"}}`))
	f.handle("DELETE /v2/sandboxes/mosso-abc", reply(200, `{"sandbox":{}}`))

	_, err := c.Create(context.Background(), "mosso-abc", CreateOptions{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 500 || apiErr.Message != "boom" {
		t.Fatalf("err = %v, want 500 boom", err)
	}
	if got := f.calls(); len(got) != 2 || got[1] != "DELETE /v2/sandboxes/mosso-abc" {
		t.Fatalf("calls = %v, want create then delete", got)
	}
	if f.requests[1].Query["projectId"][0] != "prj_1" || f.requests[1].Query["deleteOrphanSnapshots"][0] != "true" {
		t.Fatalf("delete missing projectId or deleteOrphanSnapshots: %v", f.requests[1].Query)
	}
}

func TestCreatePersistent(t *testing.T) {
	f, c := newFakeAPI(t)
	f.handle("POST /v2/sandboxes", reply(200, `{
		"sandbox": {"name": "mosso-abc", "persistent": true, "currentSessionId": "sbx_1", "status": "running", "timeout": 2700000, "createdAt": 1700000000000},
		"session": {"id": "sbx_1", "status": "running", "timeout": 2700000, "startedAt": 1700000001000},
		"routes": []
	}`))
	sb, err := c.Create(context.Background(), "mosso-abc", CreateOptions{Timeout: 45 * time.Minute, Persistent: true, SnapshotExpiration: 7 * 24 * time.Hour, KeepLastSnapshots: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !sb.Persistent || sb.Timeout != 45*time.Minute || sb.StartedAt.UnixMilli() != 1700000001000 ||
		sb.ExpiresAt() != sb.StartedAt.Add(45*time.Minute) || !sb.Running() || sb.CreatedAt.UnixMilli() != 1700000000000 {
		t.Fatalf("unexpected sandbox: %+v", sb)
	}
	body := f.requests[0].Body
	keep, _ := body["keepLastSnapshots"].(map[string]any)
	if body["persistent"] != true || body["snapshotExpiration"] != float64(7*24*3600*1000) || keep["count"] != float64(1) || keep["deleteEvicted"] != true {
		t.Fatalf("unexpected create body: %v", body)
	}
}

func TestGetResumesAndReportsErrors(t *testing.T) {
	f, c := newFakeAPI(t)
	f.handle("GET /v2/sandboxes/mosso-abc", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("resume") != "true" || r.URL.Query().Get("projectId") != "prj_1" {
			t.Errorf("query = %v", r.URL.Query())
		}
		reply(200, `{"sandbox":{"name":"mosso-abc","persistent":true,"currentSessionId":"sbx_2","status":"running","timeout":60000},
			"session":{"id":"sbx_2","status":"running","timeout":60000,"startedAt":1700000005000},"routes":[],"resumed":true}`)(w, r)
	})
	sb, err := c.Get(context.Background(), "mosso-abc", true)
	if err != nil {
		t.Fatal(err)
	}
	if sb.SessionID != "sbx_2" || sb.Timeout != time.Minute || sb.StartedAt.UnixMilli() != 1700000005000 || !sb.Running() {
		t.Fatalf("unexpected sandbox: %+v", sb)
	}

	f.handle("GET /v2/sandboxes/stopped", reply(200, `{"sandbox":{"name":"stopped","persistent":true,"currentSessionId":"sbx_9","status":"stopped"},
		"session":{"id":"sbx_9","status":"stopped","timeout":60000,"startedAt":1,"stoppedAt":2},"routes":[]}`))
	sb, err = c.Get(context.Background(), "stopped", false)
	if err != nil || sb.Running() || sb.Status != "stopped" {
		t.Fatalf("stopped sandbox = %+v, %v", sb, err)
	}

	f.handle("GET /v2/sandboxes/gone", reply(410, `{"error":{"code":"snapshot_not_found","message":"The snapshot has expired"}}`))
	_, err = c.Get(context.Background(), "gone", true)
	if !SnapshotGone(err) || !SessionStopped(err) || IsNotFound(err) {
		t.Fatalf("err = %v, want SnapshotGone", err)
	}
	f.handle("GET /v2/sandboxes/busy", reply(422, `{"error":{"code":"sandbox_stopping","message":"stopping"}}`))
	if _, err = c.Get(context.Background(), "busy", true); !InTransition(err) {
		t.Fatalf("err = %v, want InTransition", err)
	}
	if _, err = c.Get(context.Background(), "missing", true); !IsNotFound(err) || !SessionOver(err) {
		t.Fatalf("err = %v, want not found", err)
	}
}

func TestStopIgnoresEndedSessions(t *testing.T) {
	f, c := newFakeAPI(t)
	f.handle("POST /v2/sandboxes/sessions/sbx_1/stop", reply(200, `{"session":{"id":"sbx_1","status":"stopping"},"snapshot":{"id":"snap_1"}}`))
	if err := c.Stop(context.Background(), "sbx_1"); err != nil {
		t.Fatal(err)
	}
	f.handle("POST /v2/sandboxes/sessions/sbx_2/stop", reply(410, `{"error":{"code":"sandbox_stopped","message":"gone"}}`))
	if err := c.Stop(context.Background(), "sbx_2"); err != nil {
		t.Fatalf("stopping an ended session = %v, want nil", err)
	}
	if err := c.Stop(context.Background(), "sbx_404"); err != nil {
		t.Fatalf("stopping an unknown session = %v, want nil", err)
	}
	f.handle("POST /v2/sandboxes/sessions/sbx_3/stop", reply(500, `{"error":{"message":"boom"}}`))
	if err := c.Stop(context.Background(), "sbx_3"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestCreateMalformedResponseCleansUp(t *testing.T) {
	f, c := newFakeAPI(t)
	f.handle("POST /v2/sandboxes", reply(200, `{"sandbox":{"name":"mosso-abc"}}`)) // no session id
	f.handle("DELETE /v2/sandboxes/mosso-abc", reply(200, `{}`))
	if _, err := c.Create(context.Background(), "mosso-abc", CreateOptions{}); err == nil {
		t.Fatal("expected error")
	}
	if got := f.calls(); len(got) != 2 || got[1] != "DELETE /v2/sandboxes/mosso-abc" {
		t.Fatalf("calls = %v, want cleanup delete", got)
	}
}

func TestExec(t *testing.T) {
	f, c := newFakeAPI(t)
	f.handle("POST /v2/sandboxes/sessions/sbx_1/cmd", reply(200,
		`{"command":{"id":"cmd_1","name":"bash","args":["-c","echo hello"],"cwd":"/vercel/sandbox","sessionId":"sbx_1","exitCode":null,"startedAt":1}}`))
	f.handle("GET /v2/sandboxes/sessions/sbx_1/cmd/cmd_1", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("wait") != "true" {
			t.Errorf("wait query = %q", r.URL.Query().Get("wait"))
		}
		reply(200, `{"command":{"id":"cmd_1","exitCode":3}}`)(w, r)
	})
	f.handle("GET /v2/sandboxes/sessions/sbx_1/cmd/cmd_1/logs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = io.WriteString(w, `{"stream":"stdout","data":"hel"}`+"\n"+
			`{"stream":"stderr","data":"warn\n"}`+"\n"+
			`{"stream":"stdout","data":"lo\n"}`+"\n")
	})

	res, err := c.Exec(context.Background(), "sbx_1", "echo hello")
	if err != nil {
		t.Fatal(err)
	}
	want := CommandResult{CommandID: "cmd_1", ExitCode: 3, Stdout: "hello\n", Stderr: "warn\n"}
	if *res != want {
		t.Fatalf("result = %+v, want %+v", *res, want)
	}
	args, _ := f.requests[0].Body["args"].([]any)
	if f.requests[0].Body["command"] != "bash" || len(args) != 2 || args[0] != "-c" || args[1] != "echo hello" {
		t.Fatalf("unexpected cmd body: %v", f.requests[0].Body)
	}
}

func TestExecLogStreamError(t *testing.T) {
	f, c := newFakeAPI(t)
	f.handle("POST /v2/sandboxes/sessions/sbx_1/cmd", reply(200, `{"command":{"id":"cmd_1","exitCode":0}}`))
	f.handle("GET /v2/sandboxes/sessions/sbx_1/cmd/cmd_1/logs", reply(200,
		`{"stream":"error","data":{"code":"sandbox_stopped","message":"gone"}}`+"\n"))
	if _, err := c.Exec(context.Background(), "sbx_1", "true"); err == nil || !strings.Contains(err.Error(), "sandbox_stopped") {
		t.Fatalf("err = %v, want sandbox_stopped", err)
	}
}

func TestExecAPIError(t *testing.T) {
	f, c := newFakeAPI(t)
	f.handle("POST /v2/sandboxes/sessions/sbx_1/cmd", reply(410, `{"error":{"message":"session stopped"}}`))
	_, err := c.Exec(context.Background(), "sbx_1", "true")
	if err == nil || !strings.Contains(err.Error(), "session stopped") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatal("error leaks token")
	}
}

func TestDestroy(t *testing.T) {
	f, c := newFakeAPI(t)
	f.handle("POST /v2/sandboxes/sessions/sbx_1/stop", reply(200, `{"session":{"id":"sbx_1","status":"stopping"}}`))
	f.handle("DELETE /v2/sandboxes/mosso-abc", reply(200, `{"sandbox":{}}`))
	if err := c.Destroy(context.Background(), Sandbox{Name: "mosso-abc", SessionID: "sbx_1"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"POST /v2/sandboxes/sessions/sbx_1/stop", "DELETE /v2/sandboxes/mosso-abc"}
	if got := f.calls(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}

func TestDestroyIgnoresNotFound(t *testing.T) {
	_, c := newFakeAPI(t) // every route 404s
	if err := c.Destroy(context.Background(), Sandbox{Name: "mosso-abc", SessionID: "sbx_1"}); err != nil {
		t.Fatalf("Destroy of missing sandbox = %v, want nil", err)
	}
}

func TestDestroyStillDeletesWhenStopFails(t *testing.T) {
	f, c := newFakeAPI(t)
	f.handle("POST /v2/sandboxes/sessions/sbx_1/stop", reply(500, `{"error":{"message":"stop failed"}}`))
	f.handle("DELETE /v2/sandboxes/mosso-abc", reply(200, `{}`))
	err := c.Destroy(context.Background(), Sandbox{Name: "mosso-abc", SessionID: "sbx_1"})
	if err == nil || !strings.Contains(err.Error(), "stop failed") {
		t.Fatalf("err = %v", err)
	}
	if got := f.calls(); len(got) != 2 {
		t.Fatalf("calls = %v, want stop and delete", got)
	}
}

func TestLimitedBuffer(t *testing.T) {
	b := limitedBuffer{max: 4}
	b.WriteString("abc")
	b.WriteString("def")
	if got := b.String(); got != "abcd\n[mosso: output truncated]" {
		t.Fatalf("got %q", got)
	}
}

func TestLimitedBufferKeepsUTF8Valid(t *testing.T) {
	b := limitedBuffer{max: 2}
	b.WriteString("aé") // 'é' is 2 bytes, so only "a" fits
	if got := b.String(); got != "a\n[mosso: output truncated]" {
		t.Fatalf("got %q", got)
	}
}

func TestCreateRejectsTimeoutAboveMax(t *testing.T) {
	f, c := newFakeAPI(t)
	if _, err := c.Create(context.Background(), "mosso-abc", CreateOptions{Timeout: MaxTimeout + time.Second}); err == nil {
		t.Fatal("expected error")
	}
	if got := f.calls(); len(got) != 0 {
		t.Fatalf("calls = %v, want none", got)
	}
}

// Regression test: Vercel answered a body-less POST /stop with 415, which made
// DELETE /sandboxes/{name} fail even though the sandbox was cleaned up.
func TestDestroyStopRequestSendsEmptyJSONBody(t *testing.T) {
	f, c := newFakeAPI(t)
	f.handle("POST /v2/sandboxes/sessions/sbx_1/stop", reply(200, `{"session":{"id":"sbx_1","status":"stopping"}}`))
	f.handle("DELETE /v2/sandboxes/mosso-abc", reply(200, `{"sandbox":{}}`))
	if err := c.Destroy(context.Background(), Sandbox{Name: "mosso-abc", SessionID: "sbx_1"}); err != nil {
		t.Fatalf("Destroy = %v, want nil when stop and delete succeed", err)
	}
	stop := f.requests[0]
	if stop.Method != http.MethodPost || stop.Path != "/v2/sandboxes/sessions/sbx_1/stop" {
		t.Fatalf("first request = %s %s, want POST stop", stop.Method, stop.Path)
	}
	if stop.ContentType != "application/json" {
		t.Errorf("stop Content-Type = %q, want application/json", stop.ContentType)
	}
	if stop.RawBody != "{}" {
		t.Errorf("stop body = %q, want {}", stop.RawBody)
	}
	if del := f.requests[1]; del.Method != http.MethodDelete || del.Query["projectId"][0] != "prj_1" {
		t.Errorf("second request = %s %s %v, want DELETE with projectId", del.Method, del.Path, del.Query)
	}
}

func TestExecHandlesLogLineLargerThanOutputCap(t *testing.T) {
	f, c := newFakeAPI(t)
	f.handle("POST /v2/sandboxes/sessions/sbx_1/cmd", reply(200, `{"command":{"id":"cmd_1","exitCode":0}}`))
	big := strings.Repeat("x", 5*MaxOutputBytes) // one log line well past any line-scanner limit
	f.handle("GET /v2/sandboxes/sessions/sbx_1/cmd/cmd_1/logs", reply(200,
		`{"stream":"stdout","data":"`+big+`"}`+"\n"+`{"stream":"stderr","data":"done"}`+"\n"))
	res, err := c.Exec(context.Background(), "sbx_1", "cat big")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Stdout, strings.Repeat("x", MaxOutputBytes)) || !strings.HasSuffix(res.Stdout, "[mosso: output truncated]") {
		t.Fatalf("stdout not truncated as expected (len %d)", len(res.Stdout))
	}
	if res.Stderr != "done" {
		t.Fatalf("stderr = %q, want later lines still read", res.Stderr)
	}
}

func TestRunSendsEnvAndCwd(t *testing.T) {
	f, c := newFakeAPI(t)
	f.handle("POST /v2/sandboxes/sessions/sbx_1/cmd", reply(200, `{"command":{"id":"cmd_1","exitCode":0}}`))
	f.handle("GET /v2/sandboxes/sessions/sbx_1/cmd/cmd_1/logs", reply(200,
		`{"stream":"stdout","data":"0123456789"}`+"\n"))

	res, err := c.Run(context.Background(), "sbx_1", Command{
		Name: "bash", Args: []string{"-c", `echo "$SECRET"`},
		Env: map[string]string{"SECRET": "s3cr3t"}, Cwd: "/vercel/sandbox", MaxOutputBytes: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Stdout, "0123\n") {
		t.Fatalf("stdout = %q, want truncated to 4 bytes", res.Stdout)
	}
	body := f.requests[0].Body
	env, _ := body["env"].(map[string]any)
	if env["SECRET"] != "s3cr3t" || body["cwd"] != "/vercel/sandbox" {
		t.Fatalf("unexpected cmd body: %v", body)
	}
	if strings.Contains(f.requests[0].Path+fmt.Sprint(f.requests[0].Query), "s3cr3t") {
		t.Fatal("env value leaked into the URL")
	}
}

func TestRunKillsCommandWhenContextEnds(t *testing.T) {
	f, c := newFakeAPI(t)
	f.handle("POST /v2/sandboxes/sessions/sbx_1/cmd", reply(200, `{"command":{"id":"cmd_1","exitCode":null}}`))
	ctx, cancel := context.WithCancel(context.Background())
	f.handle("GET /v2/sandboxes/sessions/sbx_1/cmd/cmd_1", func(w http.ResponseWriter, r *http.Request) {
		cancel() // the caller gives up while the command is still running
		reply(200, `{"command":{"id":"cmd_1","exitCode":null}}`)(w, r)
	})
	killed := make(chan map[string]any, 1)
	f.handle("POST /v2/sandboxes/sessions/sbx_1/cmd/cmd_1/kill", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		killed <- f.requests[len(f.requests)-1].Body
		f.mu.Unlock()
		reply(200, `{"command":{"id":"cmd_1"}}`)(w, r)
	})

	if _, err := c.Run(ctx, "sbx_1", Command{Name: "sleep", Args: []string{"60"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	select {
	case body := <-killed:
		if body["signal"] != float64(15) {
			t.Fatalf("kill body = %v, want SIGTERM (15)", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("command was not killed")
	}
}

func TestRunStreamsStdoutWhileRunning(t *testing.T) {
	f, c := newFakeAPI(t)
	f.handle("POST /v2/sandboxes/sessions/sbx_1/cmd", reply(200, `{"command":{"id":"cmd_1","exitCode":null}}`))
	f.handle("GET /v2/sandboxes/sessions/sbx_1/cmd/cmd_1", reply(200, `{"command":{"id":"cmd_1","exitCode":0}}`))
	more := make(chan struct{})
	f.handle("GET /v2/sandboxes/sessions/sbx_1/cmd/cmd_1/logs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = io.WriteString(w, `{"stream":"stdout","data":"first\n"}`+"\n")
		w.(http.Flusher).Flush()
		<-more // the command is still running
		_, _ = io.WriteString(w, `{"stream":"stderr","data":"oops"}`+"\n"+`{"stream":"stdout","data":"second\n"}`+"\n")
	})

	var mu sync.Mutex
	var got []string
	first := make(chan struct{})
	done := make(chan *CommandResult)
	go func() {
		res, err := c.Run(context.Background(), "sbx_1", Command{Name: "bash", Stdout: func(s string) {
			mu.Lock()
			got = append(got, s)
			mu.Unlock()
			if s == "first\n" {
				close(first)
			}
		}})
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()
	select {
	case <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("stdout was not delivered while the command was running")
	}
	close(more)
	res := <-done
	if res == nil {
		return
	}
	if res.Stdout != "" || res.Stderr != "oops" || res.ExitCode != 0 {
		t.Fatalf("result = %+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(got, "") != "first\nsecond\n" {
		t.Fatalf("stdout = %q", got)
	}
}

func TestRunStreamReconnectsWithoutDuplicates(t *testing.T) {
	f, c := newFakeAPI(t)
	f.handle("POST /v2/sandboxes/sessions/sbx_1/cmd", reply(200, `{"command":{"id":"cmd_1","exitCode":null}}`))
	f.handle("GET /v2/sandboxes/sessions/sbx_1/cmd/cmd_1", reply(200, `{"command":{"id":"cmd_1","exitCode":0}}`))
	attempts := 0
	f.handle("GET /v2/sandboxes/sessions/sbx_1/cmd/cmd_1/logs", func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "application/x-ndjson")
		if attempts == 1 {
			// Deliver part of the output, then break the connection mid-line.
			_, _ = io.WriteString(w, `{"stream":"stdout","data":"abc"}`+"\n"+`{"stream":"std`)
			w.(http.Flusher).Flush()
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
			return
		}
		// The replay is chunked differently but has the same content.
		_, _ = io.WriteString(w, `{"stream":"stdout","data":"ab"}`+"\n"+`{"stream":"stdout","data":"cdef"}`+"\n")
	})
	var out strings.Builder
	if _, err := c.Run(context.Background(), "sbx_1", Command{Name: "bash", Stdout: func(s string) { out.WriteString(s) }}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "abcdef" || attempts != 2 {
		t.Fatalf("stdout = %q after %d attempts, want abcdef after 2", out.String(), attempts)
	}
}

func TestRunStreamKillsCommandWhenContextEnds(t *testing.T) {
	f, c := newFakeAPI(t)
	f.handle("POST /v2/sandboxes/sessions/sbx_1/cmd", reply(200, `{"command":{"id":"cmd_1","exitCode":null}}`))
	ctx, cancel := context.WithCancel(context.Background())
	f.handle("GET /v2/sandboxes/sessions/sbx_1/cmd/cmd_1/logs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = io.WriteString(w, `{"stream":"stdout","data":"working"}`+"\n")
		w.(http.Flusher).Flush()
		cancel()
		<-r.Context().Done()
	})
	killed := make(chan struct{}, 1)
	f.handle("POST /v2/sandboxes/sessions/sbx_1/cmd/cmd_1/kill", func(w http.ResponseWriter, r *http.Request) {
		killed <- struct{}{}
		reply(200, `{"command":{"id":"cmd_1"}}`)(w, r)
	})
	if _, err := c.Run(ctx, "sbx_1", Command{Name: "bash", Stdout: func(string) {}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	select {
	case <-killed:
	case <-time.After(5 * time.Second):
		t.Fatal("command was not killed")
	}
}
