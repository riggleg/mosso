package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/riggleg/mosso/internal/vercel"
)

// fakeVercel is a minimal in-memory Vercel Sandbox API.
type fakeVercel struct {
	mu      sync.Mutex
	created []string // sandbox names
	deleted []string
	stopped []string // session ids
	failCmd bool
}

func (f *fakeVercel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	p := r.URL.Path
	switch {
	case r.Method == http.MethodPost && p == "/v2/sandboxes":
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.created = append(f.created, body.Name)
		_, _ = io.WriteString(w, `{"sandbox":{"name":"`+body.Name+`","currentSessionId":"sbx_`+body.Name+`"},"session":{"id":"sbx_`+body.Name+`","status":"running"},"routes":[]}`)
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/cmd"):
		if f.failCmd {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"cmd failed"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"command":{"id":"cmd_1","exitCode":null}}`)
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/cmd/cmd_1"):
		_, _ = io.WriteString(w, `{"command":{"id":"cmd_1","exitCode":0}}`)
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/cmd/cmd_1/logs"):
		_, _ = io.WriteString(w, `{"stream":"stdout","data":"hello\n"}`+"\n")
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/stop"):
		f.stopped = append(f.stopped, strings.Split(p, "/")[4])
		_, _ = io.WriteString(w, `{"session":{}}`)
	case r.Method == http.MethodDelete && strings.HasPrefix(p, "/v2/sandboxes/"):
		f.deleted = append(f.deleted, strings.TrimPrefix(p, "/v2/sandboxes/"))
		_, _ = io.WriteString(w, `{"sandbox":{}}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func newTestServer(t *testing.T) (*Server, *fakeVercel) {
	t.Helper()
	fake := &fakeVercel{}
	api := httptest.NewServer(fake)
	t.Cleanup(api.Close)
	client := vercel.NewClient(vercel.Config{Token: "tok", TeamID: "team", ProjectID: "prj", BaseURL: api.URL})
	return New(client), fake
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
	fake.failCmd = true
	if rec := do(t, h, http.MethodPost, "/sandboxes/"+sb.Name+"/exec", `{"command":"true"}`); rec.Code != http.StatusBadGateway {
		t.Fatalf("upstream failure status = %d", rec.Code)
	}
}

func TestCreateRejectsBadTimeout(t *testing.T) {
	srv, fake := newTestServer(t)
	if rec := do(t, srv.Handler(), http.MethodPost, "/sandboxes", `{"timeout_seconds":-1}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(fake.created) != 0 {
		t.Fatal("sandbox should not be created")
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
