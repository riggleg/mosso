package vercel

import (
	"context"
	"encoding/json"
	"errors"
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
	rec := recordedRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), ContentType: r.Header.Get("Content-Type")}
	b, _ := io.ReadAll(r.Body)
	rec.RawBody = string(b)
	if len(b) > 0 {
		_ = json.Unmarshal(b, &rec.Body)
	}
	f.mu.Lock()
	f.requests = append(f.requests, rec)
	f.mu.Unlock()
	// Like the real API, reject POSTs that are not a JSON body.
	if r.Method == http.MethodPost && (rec.ContentType != "application/json" || !json.Valid(b)) {
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
	if f.requests[1].Query["projectId"][0] != "prj_1" {
		t.Fatalf("delete missing projectId: %v", f.requests[1].Query)
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
