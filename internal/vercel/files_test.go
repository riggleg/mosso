package vercel

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestWriteFilesSendsAGzippedTarToTheDirectory(t *testing.T) {
	f, c := newFakeAPI(t)
	f.handle("POST /v2/sandboxes/sessions/sess/fs/write", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "{}") })
	err := c.WriteFiles(context.Background(), "sess", "/tmp/mosso-attachments", []File{{Name: "abc/shot.png", Data: []byte("PNGDATA")}, {Name: "bin/tool", Data: []byte("#!/bin/sh"), Mode: 0o755}})
	if err != nil {
		t.Fatal(err)
	}
	req := f.requests[0]
	if req.ContentType != "application/gzip" || req.Header.Get("x-cwd") != "/tmp/mosso-attachments" {
		t.Fatalf("content type %q, headers %v", req.ContentType, req.Header)
	}
	gz, err := gzip.NewReader(strings.NewReader(req.RawBody))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	got := map[string]string{}
	modes := map[string]int64{}
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		b, _ := io.ReadAll(tr)
		got[hdr.Name], modes[hdr.Name] = string(b), hdr.Mode
	}
	if got["abc/shot.png"] != "PNGDATA" || modes["abc/shot.png"] != 0o644 || modes["bin/tool"] != 0o755 {
		t.Fatalf("tar contents = %v modes %v", got, modes)
	}
}

func TestWriteFilesRejectsUnsafeNames(t *testing.T) {
	_, c := newFakeAPI(t)
	for _, name := range []string{"", "/etc/passwd", "../x", "a/../../x", "a//b", "./a", "a\x00b"} {
		if err := c.WriteFiles(context.Background(), "s", "/tmp/x", []File{{Name: name, Data: []byte("x")}}); err == nil {
			t.Errorf("name %q was accepted", name)
		}
	}
	if err := c.WriteFiles(context.Background(), "s", "relative", nil); err == nil {
		t.Error("a relative directory was accepted")
	}
}

func TestReadFile(t *testing.T) {
	f, c := newFakeAPI(t)
	f.handle("POST /v2/sandboxes/sessions/s/fs/read", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "BYTES")
	})
	rc, err := c.ReadFile(context.Background(), "s", `/tmp/a "b".png`)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if b, _ := io.ReadAll(rc); string(b) != "BYTES" {
		t.Fatalf("got %q", b)
	}
	if got := f.requests[0].Body["path"]; got != `/tmp/a "b".png` {
		t.Fatalf("path = %v", got)
	}
}
