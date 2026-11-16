package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/riggleg/mosso/internal/vercel"
)

const (
	testImage       = "my-team/my-project/dev-image:v1"
	testSetupScript = "curl https://mise.run | sh\nmise use -g node@24\n"
	testSetupOutput = "installing mise\nmise: installed node@24\n"
)

// isSetup reports whether a sandbox command is the setup script.
func isSetup(cmd map[string]any) bool {
	args := argsOf(cmd)
	return len(args) > 1 && args[1] == setupRunScript
}

// setupSandbox scripts a workspace sandbox whose setup script prints
// testSetupOutput.
func setupSandbox() func(cmd map[string]any) string {
	repo := fakeRepoSandbox()
	return func(cmd map[string]any) string {
		if isSetup(cmd) {
			return testSetupOutput
		}
		return repo(cmd)
	}
}

// commandIndex returns the position of the first command matching match,
// or -1.
func commandIndex(cmds []map[string]any, match func(args []string) bool) int {
	for i, c := range cmds {
		if match(argsOf(c)) {
			return i
		}
	}
	return -1
}

func isClone(args []string) bool {
	return len(args) > 1 && strings.Contains(args[1], "clone --quiet")
}
func isClaudeInstall(args []string) bool {
	return len(args) > 1 && strings.Contains(args[1], "claude.ai/install.sh")
}
func isWorkerStart(args []string) bool { return len(args) > 1 && args[1] == workerStartScript }
func isSetupArgs(args []string) bool   { return len(args) > 1 && args[1] == setupRunScript }

// countSetups counts the setup script runs so far.
func countSetups(f *fakeVercel) int {
	n := 0
	for _, c := range f.commands() {
		if isSetup(c) {
			n++
		}
	}
	return n
}

// chatOf returns a copy of the workspace's chat.
func chatOf(srv *Server, name string) []chatEntry {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return append([]chatEntry(nil), srv.workspaces[name].chat.entries...)
}

type testEnvWorkspace struct {
	testWorkspace
	Image       string `json:"image"`
	SetupScript string `json:"setup_script"`
}

func createEnvWorkspace(t *testing.T, h http.Handler, body string) testEnvWorkspace {
	t.Helper()
	rec := do(t, h, http.MethodPost, "/workspaces", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create %s: status = %d: %s", body, rec.Code, rec.Body)
	}
	var ws testEnvWorkspace
	_ = json.Unmarshal(rec.Body.Bytes(), &ws)
	return ws
}

// TestSandboxEnvironmentSettings: the default image and setup script start
// empty, are changed one at a time without touching the other settings,
// are checked, and survive a restart.
func TestSandboxEnvironmentSettings(t *testing.T) {
	fake, api := newFakeVercel(t)
	dir := t.TempDir()
	srv := newServerOver(t, fake, api, dir)
	h := srv.Handler()

	s := getSettings(t, h)
	if s.DefaultImage != "" || s.DefaultSetupScript != "" || s.BuiltInRuntime != vercel.DefaultRuntime {
		t.Fatalf("initial settings = %+v", s)
	}
	model := s.DefaultModel

	body, _ := json.Marshal(map[string]string{"default_image": "  " + testImage + " ", "default_setup_script": "echo one\r\necho two\r\n"})
	if rec := do(t, h, http.MethodPost, "/settings", string(body)); rec.Code != http.StatusOK {
		t.Fatalf("set: status = %d: %s", rec.Code, rec.Body)
	}
	if s := getSettings(t, h); s.DefaultImage != testImage || s.DefaultSetupScript != "echo one\necho two\n" || s.DefaultModel != model {
		t.Fatalf("settings = %+v", s)
	}

	// Rejected changes change nothing, not even the valid ones next to them.
	for name, body := range map[string]string{
		"bad image":       `{"default_image":"not an image"}`,
		"flag image":      `{"default_image":"--privileged"}`,
		"binary script":   `{"default_setup_script":"echo \u0000"}`,
		"long script":     `{"default_setup_script":"` + strings.Repeat("x", maxSetupScriptBytes+1) + `"}`,
		"mixed":           `{"default_setup_script":"echo ok","default_image":"Not Valid"}`,
		"nothing to save": `{}`,
	} {
		if rec := do(t, h, http.MethodPost, "/settings", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d: %s", name, rec.Code, rec.Body)
		}
	}
	if s := getSettings(t, h); s.DefaultImage != testImage || s.DefaultSetupScript != "echo one\necho two\n" {
		t.Fatalf("a rejected update changed the settings: %+v", s)
	}

	// The model alone; the rest stays.
	if rec := do(t, h, http.MethodPost, "/settings", `{"default_model":"claude-haiku-5-5"}`); rec.Code != http.StatusOK {
		t.Fatalf("set model: status = %d: %s", rec.Code, rec.Body)
	}
	if s := getSettings(t, h); s.DefaultImage != testImage || s.DefaultModel != "claude-haiku-5-5" {
		t.Fatalf("settings = %+v", s)
	}

	// After a restart.
	srv.cancel()
	srv.store.close()
	srv = newServerOver(t, fake, api, dir)
	if s := getSettings(t, srv.Handler()); s.DefaultImage != testImage || s.DefaultSetupScript != "echo one\necho two\n" || s.DefaultModel != "claude-haiku-5-5" {
		t.Fatalf("settings after a restart = %+v", s)
	}

	// Blank values clear them.
	if rec := do(t, srv.Handler(), http.MethodPost, "/settings", `{"default_image":" ","default_setup_script":"\n  \n"}`); rec.Code != http.StatusOK {
		t.Fatalf("clear: status = %d: %s", rec.Code, rec.Body)
	}
	if s := getSettings(t, srv.Handler()); s.DefaultImage != "" || s.DefaultSetupScript != "" {
		t.Fatalf("cleared settings = %+v", s)
	}
}

// TestWorkspaceWithoutSandboxEnvironment: with nothing configured, a
// workspace is created exactly as before: the built-in runtime and no
// setup step.
func TestWorkspaceWithoutSandboxEnvironment(t *testing.T) {
	srv, fake := newTestServer(t)
	withGitHub(t, srv, &fakeGitHubAPI{})
	fake.stdoutFor = setupSandbox()
	ws := createEnvWorkspace(t, srv.Handler(), `{"repo":"octo/widgets"}`)
	if fake.images[0] != "" || fake.runtimes[0] != vercel.DefaultRuntime {
		t.Fatalf("created from image %q, runtime %q", fake.images[0], fake.runtimes[0])
	}
	if n := countSetups(fake); n != 0 || ws.Image != "" || ws.SetupScript != "" {
		t.Fatalf("setups = %d, workspace = %+v", n, ws)
	}
	for _, e := range chatOf(srv, ws.Name) {
		if e.Type == "setup" || strings.Contains(e.Text, "setup script") {
			t.Fatalf("chat mentions a setup: %+v", e)
		}
	}
	fake.mu.Lock()
	_, wroteScript := fake.files["sbx_"+ws.Name][setupScriptFile]
	fake.mu.Unlock()
	if wroteScript {
		t.Fatal("a setup script was written without one configured")
	}
}

// TestWorkspaceRunsTheDefaultSetupScript: the default image boots the
// sandbox and the default setup script runs once, in the checkout, after
// the clone and before Claude Code, the worker and the first message. Its
// output is in the chat, it gets no credentials, and the workspace saves
// which image and script it got.
func TestWorkspaceRunsTheDefaultSetupScript(t *testing.T) {
	srv, fake := newTestServer(t)
	withToken(srv, testClaudeToken, nil)
	api := &fakeGitHubAPI{private: true}
	withGitHub(t, srv, api)
	fake.stdoutFor = setupSandbox()
	h := srv.Handler()
	body, _ := json.Marshal(map[string]string{"default_image": testImage, "default_setup_script": testSetupScript})
	if rec := do(t, h, http.MethodPost, "/settings", string(body)); rec.Code != http.StatusOK {
		t.Fatalf("settings: %d %s", rec.Code, rec.Body)
	}

	ws := createEnvWorkspace(t, h, `{"repo":"octo/widgets","prompt":"run the tests"}`)
	waitFor(t, func() bool { return fake.claude.count() == 1 && getWorkspace(t, h, ws.Name).Status == "ready" })

	if fake.images[0] != testImage || fake.runtimes[0] != "" {
		t.Fatalf("created from image %q, runtime %q", fake.images[0], fake.runtimes[0])
	}
	if ws.Image != testImage || ws.SetupScript != testSetupScript {
		t.Fatalf("workspace = %+v", ws)
	}

	cmds := fake.commands()
	clone, setup := commandIndex(cmds, isClone), commandIndex(cmds, isSetupArgs)
	install, start := commandIndex(cmds, isClaudeInstall), commandIndex(cmds, isWorkerStart)
	if !(clone >= 0 && clone < setup && setup < install && setup < start) || countSetups(fake) != 1 {
		t.Fatalf("order: clone %d, setup %d, install %d, worker %d (setups: %d)", clone, setup, install, start, countSetups(fake))
	}
	// The script is a file in the sandbox, run in the checkout, with
	// nothing but install settings in its environment.
	args := argsOf(cmds[setup])
	if args[3] != setupScriptFile || args[4] != setupEnvFile || args[5] != ws.Dir || args[6] != setupLogFile {
		t.Fatalf("setup args = %q", args)
	}
	env := envOf(cmds[setup])
	if len(env) != len(setupEnv) || env["MISE_YES"] != "1" || env["DEBIAN_FRONTEND"] != "noninteractive" {
		t.Fatalf("setup env = %v", env)
	}
	raw, _ := json.Marshal(cmds[setup])
	if strings.Contains(string(raw), testGitHubToken) || strings.Contains(string(raw), testClaudeToken) {
		t.Fatal("the setup command carries a credential")
	}
	// The clone of the private repository got the token, and only it.
	if env := envOf(cmds[clone]); env["MOSSO_GITHUB_TOKEN"] != testGitHubToken {
		t.Fatalf("clone env = %v", env)
	}
	fake.mu.Lock()
	script := string(fake.files["sbx_"+ws.Name][setupScriptFile])
	fake.mu.Unlock()
	if script != testSetupScript {
		t.Fatalf("script in the sandbox = %q", script)
	}

	// The chat: the first message, the setup and its output, then Claude.
	entries := chatOf(srv, ws.Name)
	var setupText strings.Builder
	order := []string{}
	for _, e := range entries {
		switch {
		case e.Type == "setup":
			setupText.WriteString(e.Text)
			if order[len(order)-1] != "output" {
				order = append(order, "output")
			}
		case e.Type == "notice" && strings.Contains(e.Text, "setup script"):
			order = append(order, e.Text)
		case e.Type == "user" || e.Type == "result":
			order = append(order, e.Type)
		}
	}
	if setupText.String() != testSetupOutput || len(order) != 5 || order[0] != "user" || order[1] != "Running the setup script…" ||
		order[2] != "output" || !strings.HasPrefix(order[3], "The setup script finished in ") || order[4] != "result" {
		t.Fatalf("chat = %q, setup output = %q", order, setupText.String())
	}

	// Saved with the workspace, setup output included.
	records, err := srv.store.load()
	if err != nil || len(records) != 1 || records[0].Image != testImage || records[0].SetupScript != testSetupScript {
		t.Fatalf("records = %+v, %v", records, err)
	}
	saved, err := srv.store.loadEntries(ws.Name)
	if err != nil || countTypes(saved)["setup"] == 0 {
		t.Fatalf("saved chat = %s, %v", entryTypes(saved), err)
	}
	if got := getSettings(t, h); got.DefaultSetupScript != testSetupScript {
		t.Fatalf("settings changed: %+v", got)
	}
}

// TestWorkspaceOverridesSandboxEnvironment: a workspace can ask for
// another image and script than the defaults, or for none.
func TestWorkspaceOverridesSandboxEnvironment(t *testing.T) {
	srv, fake := newTestServer(t)
	withGitHub(t, srv, &fakeGitHubAPI{})
	fake.stdoutFor = setupSandbox()
	h := srv.Handler()
	body, _ := json.Marshal(map[string]string{"default_image": testImage, "default_setup_script": testSetupScript})
	do(t, h, http.MethodPost, "/settings", string(body))

	// Another image and script.
	ws := createEnvWorkspace(t, h, `{"repo":"octo/widgets","image":"vercel/sandbox/universal","setup_script":"make deps"}`)
	if fake.images[0] != "vercel/sandbox/universal" || ws.Image != "vercel/sandbox/universal" || ws.SetupScript != "make deps" {
		t.Fatalf("image %q, workspace %+v", fake.images[0], ws)
	}
	fake.mu.Lock()
	script := string(fake.files["sbx_"+ws.Name][setupScriptFile])
	fake.mu.Unlock()
	if script != "make deps" || countSetups(fake) != 1 {
		t.Fatalf("script = %q, setups = %d", script, countSetups(fake))
	}

	// Neither: the built-in runtime and no setup.
	ws = createEnvWorkspace(t, h, `{"repo":"octo/widgets","image":"","setup_script":"  "}`)
	if fake.images[1] != "" || fake.runtimes[1] != vercel.DefaultRuntime || ws.Image != "" || ws.SetupScript != "" || countSetups(fake) != 1 {
		t.Fatalf("image %q runtime %q, workspace %+v, setups %d", fake.images[1], fake.runtimes[1], ws, countSetups(fake))
	}

	// Omitted: the defaults.
	ws = createEnvWorkspace(t, h, `{"repo":"octo/widgets"}`)
	if fake.images[2] != testImage || ws.SetupScript != testSetupScript || countSetups(fake) != 2 {
		t.Fatalf("image %q, workspace %+v", fake.images[2], ws)
	}

	// Invalid overrides are refused before a sandbox is created.
	created := len(fake.created)
	for _, body := range []string{
		`{"repo":"octo/widgets","image":"Bad Image"}`,
		`{"repo":"octo/widgets","setup_script":"` + strings.Repeat("x", maxSetupScriptBytes+1) + `"}`,
	} {
		if rec := do(t, h, http.MethodPost, "/workspaces", body); rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d: %s", rec.Code, rec.Body)
		}
	}
	if len(fake.created) != created {
		t.Fatal("an invalid override created a sandbox")
	}
}

// TestWorkspaceSetupFailure: a failing setup script is reported, with the
// end of its output, and the workspace is not created: its sandbox is
// destroyed, nothing is saved, and neither Claude Code nor the first
// message runs.
func TestWorkspaceSetupFailure(t *testing.T) {
	srv, fake := newTestServer(t)
	withToken(srv, testClaudeToken, nil)
	withGitHub(t, srv, &fakeGitHubAPI{})
	fake.stdoutFor = func(cmd map[string]any) string {
		if isSetup(cmd) {
			return "step one\n" + strings.Repeat("noise\n", 1000) + "E: Unable to locate package nodejs-99\n"
		}
		return fakeRepoSandbox()(cmd)
	}
	fake.exitFor = func(cmd map[string]any) int {
		if isSetup(cmd) {
			return 100
		}
		return 0
	}
	h := srv.Handler()
	rec := do(t, h, http.MethodPost, "/workspaces", `{"repo":"octo/widgets","setup_script":"sudo apt-get install -y nodejs-99","prompt":"hi"}`)
	var resp struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(resp.Error, "exit code 100") ||
		!strings.HasSuffix(strings.TrimSpace(strings.Split(resp.Error, "\nThe workspace")[0]), "E: Unable to locate package nodejs-99") ||
		strings.Contains(resp.Error, "step one") || !strings.Contains(resp.Error, "sandbox is destroyed") {
		t.Fatalf("status = %d: %s", rec.Code, resp.Error)
	}
	if len(resp.Error) > setupTailBytes+300 {
		t.Fatalf("error is %d bytes long", len(resp.Error))
	}
	if len(fake.created) != 1 || len(fake.deleted) != 1 || fake.deleted[0] != fake.created[0] {
		t.Fatalf("created=%v deleted=%v, want the sandbox destroyed", fake.created, fake.deleted)
	}
	cmds := fake.commands()
	if commandIndex(cmds, isClaudeInstall) >= 0 || commandIndex(cmds, isWorkerStart) >= 0 || fake.claude.count() != 0 {
		t.Fatal("Claude Code or the worker ran after a failed setup")
	}
	if records, err := srv.store.load(); err != nil || len(records) != 0 {
		t.Fatalf("saved after a failed setup: %+v, %v", records, err)
	}
	if rec := do(t, h, http.MethodGet, "/workspaces", ""); !strings.Contains(rec.Body.String(), `"workspaces":[]`) {
		t.Fatalf("workspaces = %s", rec.Body)
	}
}

// TestSetupRunsOncePerWorkspace: the setup script runs when the workspace
// is created, not when its persistent sandbox resumes or Mosso restarts;
// each worker start loads the environment the setup made.
func TestSetupRunsOncePerWorkspace(t *testing.T) {
	fake, api := newFakeVercel(t)
	fake.stdoutFor = setupSandbox()
	dir := t.TempDir()
	start := func() (*Server, *httptest.Server) {
		srv := newServerOver(t, fake, api, dir, func(srv *Server) {
			withToken(srv, testClaudeToken, nil)
			withGitHub(t, srv, &fakeGitHubAPI{})
		})
		ts := httptest.NewServer(srv.Handler())
		t.Cleanup(ts.Close)
		t.Cleanup(srv.CloseStreams)
		return srv, ts
	}
	srv, ts := start()
	ws := createEnvWorkspace(t, srv.Handler(), `{"repo":"octo/widgets","image":"`+testImage+`","setup_script":"echo set up"}`)
	if countSetups(fake) != 1 {
		t.Fatalf("setups = %d", countSetups(fake))
	}

	// The session ends; the next message resumes the sandbox.
	fake.timeout(ws.Name)
	waitFor(t, func() bool { return getWorkspace(t, srv.Handler(), ws.Name).Status == "stopped" })
	if resp := postJSON(t, ts.URL+"/workspaces/"+ws.Name+"/chat", `{"prompt":"after resume"}`); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("chat status = %d", resp.StatusCode)
	}
	waitFor(t, func() bool {
		return fake.claude.count() == 1 && getWorkspace(t, srv.Handler(), ws.Name).Status == "ready"
	})

	// Mosso restarts and reconnects.
	srv.CloseStreams()
	srv.cancel()
	srv.store.close()
	ts.Close()
	fake.timeout(ws.Name)
	srv, _ = start()
	if got := getWorkspace(t, srv.Handler(), ws.Name); got.Name != ws.Name {
		t.Fatalf("workspace after a restart = %+v", got)
	}
	var restored testEnvWorkspace
	_ = json.Unmarshal(do(t, srv.Handler(), http.MethodGet, "/workspaces/"+ws.Name, "").Body.Bytes(), &restored)
	if restored.Image != testImage || restored.SetupScript != "echo set up" {
		t.Fatalf("restored workspace = %+v", restored)
	}
	// Reconnecting leaves the stopped sandbox alone (or resumes it, if it
	// missed the end of the turn); connecting resumes it either way.
	waitFor(t, func() bool {
		st := getWorkspace(t, srv.Handler(), ws.Name).Status
		return st == "stopped" || st == "ready"
	})
	if rec := do(t, srv.Handler(), http.MethodPost, "/workspaces/"+ws.Name+"/connect", ""); rec.Code != http.StatusOK {
		t.Fatalf("connect status = %d: %s", rec.Code, rec.Body)
	}
	if n, starts := countSetups(fake), fake.worker(ws.Name).starts; n != 1 || starts < 3 {
		t.Fatalf("setups = %d (want 1), worker starts = %d (want 3)", n, starts)
	}
	if !strings.Contains(workerStartScript, `. "$state/env"`) || setupEnvFile != workerDir+"/env" {
		t.Fatal("the worker does not load the workspace environment")
	}
}

// TestSetupOutputBatches: output reaches the chat in whole lines, at most
// once a flush interval, in order and complete; past the limit it is cut
// with a pointer to the log; the tail for errors is the end of it.
func TestSetupOutputBatches(t *testing.T) {
	var mu sync.Mutex
	var got []string
	o := newSetupOutput(func(s string) {
		mu.Lock()
		got = append(got, s)
		mu.Unlock()
	})
	o.write("one\ntw")
	o.write("o\nthr")
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 1
	})
	mu.Lock()
	first := got[0]
	mu.Unlock()
	if first != "one\ntwo\n" {
		t.Fatalf("first batch = %q", first)
	}
	o.write("ee")
	o.close()
	if strings.Join(got, "") != "one\ntwo\nthree" {
		t.Fatalf("batches = %q", got)
	}
	if tail := o.tail(); tail != "one\ntwo\nthree" {
		t.Fatalf("tail = %q", tail)
	}

	got = nil
	o = newSetupOutput(func(s string) { got = append(got, s) })
	line := strings.Repeat("é", 50) + "\n"
	for range 2 * maxSetupChatBytes / len(line) {
		o.write(line)
	}
	o.write("last line")
	o.close()
	all := strings.Join(got, "")
	if len(all) > maxSetupChatBytes+200 || !strings.Contains(all, setupLogFile) || strings.Contains(all, "last line") {
		t.Fatalf("kept %d bytes, ends %q", len(all), all[len(all)-100:])
	}
	if !strings.Contains(all, strings.Repeat("é", 10)) || strings.ContainsRune(all, '�') {
		t.Fatal("output was cut inside a character")
	}
	tail := o.tail()
	if !strings.HasPrefix(tail, "…\n"+strings.Repeat("é", 50)+"\n") || !strings.HasSuffix(tail, "last line") || len(tail) > setupTailBytes+10 {
		t.Fatalf("tail = %q", tail)
	}
}

// TestSandboxEnvironmentMigration: workspaces saved before images and
// setup scripts load with neither, and a workspace's image and script
// never change once saved.
func TestSandboxEnvironmentMigration(t *testing.T) {
	released := migrations
	defer func() { migrations = released }()
	path := filepath.Join(t.TempDir(), "mosso.db")
	migrations = released[:2]
	st, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO workspaces (id, repo, base, branch, model, sandbox_id, status, state, created_at, updated_at)
		VALUES ('mosso-0123456789ab', 'octo/widgets', 'main', 'mosso/x', 'claude-opus-5-5', 'mosso-0123456789ab', 'stopped', '{}', '2026-05-01T12:00:00.000000Z', '2026-05-01T12:00:00.000000Z')`); err != nil {
		t.Fatal(err)
	}
	st.close()

	migrations = released
	st, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.close()
	loaded, err := st.load()
	if err != nil || len(loaded) != 1 || loaded[0].Image != "" || loaded[0].SetupScript != "" || loaded[0].Model != "claude-opus-5-5" {
		t.Fatalf("loaded = %+v, %v", loaded, err)
	}

	rec := testRecord("mosso-ba9876543210")
	rec.Image, rec.SetupScript = testImage, testSetupScript
	rec.CreatedAt = time.Now()
	if err := st.save(rec); err != nil {
		t.Fatal(err)
	}
	rec.Image, rec.SetupScript = "other", "other"
	if err := st.save(rec); err != nil {
		t.Fatal(err)
	}
	loaded, err = st.load()
	if err != nil || len(loaded) != 2 || loaded[1].Image != testImage || loaded[1].SetupScript != testSetupScript {
		t.Fatalf("loaded = %+v, %v", loaded, err)
	}
	if err := st.setSettings(map[string]string{settingDefaultImage: testImage, settingDefaultSetupScript: "x"}); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.setting(settingDefaultImage); v != testImage {
		t.Fatalf("default image = %q", v)
	}
}
