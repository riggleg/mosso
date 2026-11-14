package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"testing"
)

// onePixelPNG is a valid 1×1 PNG.
var onePixelPNG, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==")

type upFile struct {
	name string
	data []byte
}

// postUpload posts files to a workspace and returns the response.
func postUpload(t *testing.T, base, ws string, files ...upFile) *http.Response {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, f := range files {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="files"; filename="`+strings.ReplaceAll(f.name, `"`, "%22")+`"`)
		h.Set("Content-Type", "application/x-claimed-by-client")
		p, _ := mw.CreatePart(h)
		_, _ = p.Write(f.data)
	}
	_ = mw.Close()
	resp, err := http.Post(base+"/workspaces/"+ws+"/attachments", mw.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func uploaded(t *testing.T, resp *http.Response) []attachment {
	t.Helper()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload status = %d: %s", resp.StatusCode, b)
	}
	var out struct {
		Attachments []attachment `json:"attachments"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Attachments
}

func errorOf(t *testing.T, resp *http.Response, status int) string {
	t.Helper()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != status {
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, status, b)
	}
	return string(b)
}

func get(t *testing.T, url string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func TestSanitizeFilename(t *testing.T) {
	for in, want := range map[string]string{
		"shot.png":                    "shot.png",
		"../../etc/passwd":            "passwd",
		`C:\Users\me\Desktop\a b.txt`: "a b.txt",
		".bashrc":                     "bashrc",
		"..":                          "file",
		"":                            "file",
		"a\nb\x00c.txt":               "abc.txt",
		"$(rm -rf ~).sh":              "_(rm -rf _).sh",
		"résumé.pdf":                  "résumé.pdf",
		"Screenshot 2026-11-14 at 9.41.02 AM.png": "Screenshot 2026-11-14 at 9.41.02 AM.png",
		strings.Repeat("a", 300) + ".png":         strings.Repeat("a", maxFilenameBytes-4) + ".png",
	} {
		if got := sanitizeFilename(in); got != want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUploadStoresFilesInTheSandboxOutsideTheRepo(t *testing.T) {
	srv, fake, ts, ws := newChatServer(t)
	atts := uploaded(t, postUpload(t, ts.URL, ws.Name,
		upFile{"shot.png", onePixelPNG},
		upFile{"notes.md", []byte("# hi")},
		upFile{"logo.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)},
		upFile{"evil.png", []byte("<html>not an image</html>")},
	))
	if len(atts) != 4 {
		t.Fatalf("got %d attachments", len(atts))
	}
	if !atts[0].Image || atts[0].Type != "image/png" || atts[0].Size != int64(len(onePixelPNG)) {
		t.Fatalf("png = %+v", atts[0])
	}
	if atts[1].Image || atts[1].Name != "notes.md" {
		t.Fatalf("md = %+v", atts[1])
	}
	if atts[2].Image {
		t.Fatal("SVG must be treated as a file, never an inline image")
	}
	if atts[3].Image {
		t.Fatalf("content decides whether it is an image, not the name: %+v", atts[3])
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	files := attachmentFiles(fake.files["sbx_"+ws.Name])
	for _, a := range atts {
		p := attachmentsDir + "/" + a.ID + "/" + a.Name
		if _, ok := files[p]; !ok {
			t.Errorf("%s not written; have %v", p, files)
		}
	}
	if len(files) != 4 {
		t.Errorf("wrote %d files", len(files))
	}
	for p := range files {
		if strings.Contains(p, ws.Dir) {
			t.Errorf("%s is inside the repository", p)
		}
	}
	if srv.workspaces[ws.Name].attachBytes == 0 {
		t.Error("workspace attachment bytes are not tracked")
	}
}

func TestUploadNamesAreSafeAndNeverCollide(t *testing.T) {
	_, fake, ts, ws := newChatServer(t)
	atts := uploaded(t, postUpload(t, ts.URL, ws.Name,
		upFile{"image.png", onePixelPNG},
		upFile{"image.png", append([]byte{}, onePixelPNG...)},
		upFile{"../../../etc/cron.d/x", []byte("evil")},
		upFile{`..\..\windows.ini`, []byte("evil")},
	))
	ids := map[string]bool{}
	for _, a := range atts {
		ids[a.ID] = true
	}
	if len(ids) != 4 {
		t.Fatalf("attachment IDs collide: %+v", atts)
	}
	if atts[2].Name != "x" || atts[3].Name != "windows.ini" {
		t.Fatalf("traversal not stripped: %+v", atts)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for p := range fake.files["sbx_"+ws.Name] {
		if strings.HasPrefix(p, workerDir+"/bin/") {
			continue // the worker binary
		}
		if rel, ok := strings.CutPrefix(p, attachmentsDir+"/"); !ok || strings.Contains(p, "..") || strings.Count(rel, "/") != 1 {
			t.Errorf("unsafe path %q", p)
		}
	}
}

// attachmentFiles returns the sandbox files that are attachments (not the
// worker binary).
func attachmentFiles(files map[string][]byte) map[string][]byte {
	out := map[string][]byte{}
	for p, data := range files {
		if strings.HasPrefix(p, attachmentsDir+"/") {
			out[p] = data
		}
	}
	return out
}

func TestUploadLimits(t *testing.T) {
	_, fake, ts, ws := newChatServer(t)
	many := make([]upFile, maxMessageFiles+1)
	for i := range many {
		many[i] = upFile{"f.txt", []byte("x")}
	}
	errorOf(t, postUpload(t, ts.URL, ws.Name, many...), http.StatusRequestEntityTooLarge)
	errorOf(t, postUpload(t, ts.URL, ws.Name, upFile{"big.bin", make([]byte, maxAttachmentBytes+1)}), http.StatusRequestEntityTooLarge)
	errorOf(t, postUpload(t, ts.URL, ws.Name,
		upFile{"a.bin", bytes.Repeat([]byte{1}, maxAttachmentBytes)},
		upFile{"b.bin", bytes.Repeat([]byte{1}, maxMessageBytes-maxAttachmentBytes+1)}), http.StatusRequestEntityTooLarge)
	errorOf(t, postUpload(t, ts.URL, ws.Name, upFile{"empty.txt", nil}), http.StatusBadRequest)
	errorOf(t, postUpload(t, ts.URL, ws.Name), http.StatusBadRequest)
	fake.mu.Lock()
	n := len(attachmentFiles(fake.files["sbx_"+ws.Name]))
	fake.mu.Unlock()
	if n != 0 {
		t.Fatalf("rejected uploads wrote %d files", n)
	}
	// Exactly at the limits is fine.
	uploaded(t, postUpload(t, ts.URL, ws.Name, upFile{"ok.bin", bytes.Repeat([]byte{1}, maxAttachmentBytes)}))
}

func TestWorkspaceAttachmentTotalIsLimited(t *testing.T) {
	srv, _, ts, ws := newChatServer(t)
	srv.mu.Lock()
	srv.workspaces[ws.Name].attachBytes = maxWorkspaceBytes - 10
	srv.mu.Unlock()
	errorOf(t, postUpload(t, ts.URL, ws.Name, upFile{"a.txt", []byte("0123456789ab")}), http.StatusRequestEntityTooLarge)
	uploaded(t, postUpload(t, ts.URL, ws.Name, upFile{"a.txt", []byte("0123456789")}))
}

func TestUploadFailureCanBeRetried(t *testing.T) {
	srv, fake, ts, ws := newChatServer(t)
	fake.mu.Lock()
	fake.failWrite = true
	fake.mu.Unlock()
	msg := errorOf(t, postUpload(t, ts.URL, ws.Name, upFile{"a.png", onePixelPNG}), http.StatusBadGateway)
	if !strings.Contains(msg, "could not copy") {
		t.Fatalf("unhelpful error: %s", msg)
	}
	if srv.workspaces[ws.Name].attachBytes != 0 || len(srv.workspaces[ws.Name].attachments) != 0 {
		t.Fatal("failed upload left state behind")
	}
	fake.mu.Lock()
	fake.failWrite = false
	fake.mu.Unlock()
	uploaded(t, postUpload(t, ts.URL, ws.Name, upFile{"a.png", onePixelPNG}))
}

func TestUploadRequestChecks(t *testing.T) {
	_, _, ts, ws := newChatServer(t)
	// A browser page on another origin cannot upload.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	p, _ := mw.CreateFormFile("files", "a.txt")
	_, _ = p.Write([]byte("x"))
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/workspaces/"+ws.Name+"/attachments", bytes.NewReader(body.Bytes()))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	errorOf(t, resp, http.StatusForbidden)
	// Multipart is accepted nowhere else.
	resp, err = http.Post(ts.URL+"/workspaces/"+ws.Name+"/chat", mw.FormDataContentType(), bytes.NewReader(body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	errorOf(t, resp, http.StatusUnsupportedMediaType)
	errorOf(t, postUpload(t, ts.URL, "mosso-nope", upFile{"a", []byte("x")}), http.StatusNotFound)
}

func TestMessageWithAttachmentsReachesClaude(t *testing.T) {
	_, fake, ts, ws := newChatServer(t)
	events := openEvents(t, ts.URL, ws.Name, "")
	events.next()
	atts := uploaded(t, postUpload(t, ts.URL, ws.Name, upFile{"shot.png", onePixelPNG}, upFile{"spec.pdf", []byte("%PDF-1.4 x")}))
	chat := ts.URL + "/workspaces/" + ws.Name + "/chat"
	ids, _ := json.Marshal([]string{atts[0].ID, atts[1].ID})

	// Attachments without text are a valid message.
	resp := postJSON(t, chat, `{"prompt":"","attachments":`+string(ids)+`}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	entries := events.until("result")
	user := entries[0]
	if user.Type != "user" || user.Text != "" || len(user.Attachments) != 2 || user.Attachments[0].Name != "shot.png" || user.Attachments[1].Type != "application/pdf" {
		t.Fatalf("user entry = %+v", user)
	}
	task := fake.claude.last()
	prompt := task.Prompt
	for _, want := range []string{attachmentsDir + "/" + atts[0].ID + "/shot.png", "image/png, image", attachmentsDir + "/" + atts[1].ID + "/spec.pdf", "Read tool"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, base64.StdEncoding.EncodeToString(onePixelPNG)) || strings.Contains(prompt, "PNG") {
		t.Error("file contents must not be in the prompt")
	}
	if task.Text != "" || !strings.Contains(string(task.Attachments), atts[0].ID) {
		t.Errorf("the worker records the user's text and attachments, got %q %s", task.Text, task.Attachments)
	}
	fake.mu.Lock()
	for _, c := range fake.cmds {
		if args, _ := json.Marshal(c["args"]); strings.Contains(string(args), atts[0].ID) {
			t.Error("attachments belong in the prompt, not in command arguments")
		}
	}
	fake.mu.Unlock()

	// A follow-up can attach the same file again, and a plain one resumes
	// the session.
	postJSON(t, chat, `{"prompt":"make it look like this","attachments":["`+atts[0].ID+`"]}`)
	second := events.until("result")
	if second[0].Text != "make it look like this" || len(second[0].Attachments) != 1 {
		t.Fatalf("follow-up entry = %+v", second[0])
	}

	// The history, with its attachments, is replayed to a new tab.
	fresh := openEvents(t, ts.URL, ws.Name, "")
	if e := fresh.entry(); len(e.Attachments) != 2 {
		t.Fatalf("replayed entry = %+v", e)
	}
}

func TestMessageRejectsUnknownAndForeignAttachments(t *testing.T) {
	_, _, ts, ws := newChatServer(t)
	chat := ts.URL + "/workspaces/" + ws.Name + "/chat"
	for _, body := range []string{
		`{"prompt":"hi","attachments":["nope"]}`,
		`{"prompt":"hi","attachments":["../../etc/passwd"]}`,
		`{"attachments":[]}`,
		`{"prompt":"hi","attachments":["` + strings.Repeat(`a","`, maxMessageFiles) + `a"]}`,
	} {
		if resp := postJSON(t, chat, body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d", body, resp.StatusCode)
		}
	}
}

func TestAttachmentsAreIsolatedBetweenWorkspaces(t *testing.T) {
	srv, fake, ts, a := newChatServer(t)
	rec := do(t, srv.Handler(), http.MethodPost, "/workspaces", `{"repo":"octo/widgets"}`)
	var b testWorkspace
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	if b.Name == "" || b.Name == a.Name {
		t.Fatalf("second workspace: %s", rec.Body)
	}
	att := uploaded(t, postUpload(t, ts.URL, a.Name, upFile{"secret.txt", []byte("a's secret")}))[0]

	if resp, body := get(t, ts.URL+"/workspaces/"+b.Name+"/attachments/"+att.ID); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("B read A's attachment: %d %s", resp.StatusCode, body)
	}
	if resp := postJSON(t, ts.URL+"/workspaces/"+b.Name+"/chat", `{"prompt":"x","attachments":["`+att.ID+`"]}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("B attached A's file: %d", resp.StatusCode)
	}
	if resp, body := get(t, ts.URL+"/workspaces/"+a.Name+"/attachments/"+att.ID); resp.StatusCode != http.StatusOK || string(body) != "a's secret" {
		t.Fatalf("A cannot read its own: %d %s", resp.StatusCode, body)
	}
	fake.mu.Lock()
	leaked := len(attachmentFiles(fake.files["sbx_"+b.Name]))
	fake.mu.Unlock()
	if leaked != 0 {
		t.Fatal("B's sandbox received A's file")
	}
}

func TestDownloadIsServedSafely(t *testing.T) {
	_, _, ts, ws := newChatServer(t)
	atts := uploaded(t, postUpload(t, ts.URL, ws.Name,
		upFile{"shot.png", onePixelPNG},
		upFile{"page.html", []byte("<script>alert(1)</script>")},
		upFile{"fake.png", []byte("<script>alert(1)</script>")},
		upFile{"logo.svg", []byte("<svg onload=alert(1)>")},
	))
	url := func(i int) string { return ts.URL + "/workspaces/" + ws.Name + "/attachments/" + atts[i].ID }

	resp, body := get(t, url(0))
	if resp.StatusCode != 200 || !bytes.Equal(body, onePixelPNG) || resp.Header.Get("Content-Type") != "image/png" || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "inline") {
		t.Fatalf("image: %d %v %q", resp.StatusCode, resp.Header, body)
	}
	for _, i := range []int{1, 2, 3} {
		resp, _ := get(t, url(i))
		if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") {
			t.Errorf("%s served as %q, %q", atts[i].Name, ct, resp.Header.Get("Content-Disposition"))
		}
	}
	resp, _ = get(t, url(0)+"?download=1")
	if !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Error("download=1 should force a download")
	}
	for _, h := range []string{"X-Content-Type-Options", "Content-Security-Policy"} {
		if resp.Header.Get(h) == "" {
			t.Errorf("missing %s", h)
		}
	}
	if resp, _ := get(t, ts.URL+"/workspaces/"+ws.Name+"/attachments/nope"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown attachment: %d", resp.StatusCode)
	}
}

func TestAttachmentsGoWithTheWorkspace(t *testing.T) {
	srv, fake, ts, ws := newChatServer(t)
	att := uploaded(t, postUpload(t, ts.URL, ws.Name, upFile{"a.png", onePixelPNG}))[0]

	// The sandbox is gone (expired) but the workspace is still listed: the
	// attachment is reported as gone, not as a server error.
	fake.mu.Lock()
	delete(fake.files, "sbx_"+ws.Name)
	fake.mu.Unlock()
	if resp, _ := get(t, ts.URL+"/workspaces/"+ws.Name+"/attachments/"+att.ID); resp.StatusCode != http.StatusGone {
		t.Fatalf("expired sandbox: status = %d", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/workspaces/"+ws.Name, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("terminate: %v %v", resp, err)
	}
	if len(fake.deleted) != 1 {
		t.Fatalf("sandbox not deleted: %v", fake.deleted)
	}
	srv.mu.Lock()
	n := len(srv.workspaces)
	srv.mu.Unlock()
	if n != 0 {
		t.Fatal("workspace and its attachment records are still held")
	}
	if resp, _ := get(t, ts.URL+"/workspaces/"+ws.Name+"/attachments/"+att.ID); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("after terminate: status = %d", resp.StatusCode)
	}
}
