package vercel

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
)

// File is a file to write into a sandbox. Name is relative to the target
// directory and may contain directories ("id/photo.png").
type File struct {
	Name string
	Data []byte
}

// WriteFiles extracts files under dir (which must already exist) in the
// sandbox session, using Vercel's documented write-files endpoint: a gzipped
// tarball with the target directory in the x-cwd header. Files are created
// with mode 0644, so nothing is executable. Names must be relative and
// contain no "." or ".." elements.
func (c *Client) WriteFiles(ctx context.Context, sessionID, dir string, files []File) error {
	if !strings.HasPrefix(dir, "/") {
		return errors.New("dir must be an absolute path")
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	dirs := map[string]bool{}
	for _, f := range files {
		if f.Name == "" || path.IsAbs(f.Name) || path.Clean(f.Name) != f.Name || strings.HasPrefix(f.Name, "../") || strings.ContainsRune(f.Name, 0) {
			return fmt.Errorf("invalid file name %q", f.Name)
		}
		if d := path.Dir(f.Name); d != "." && !dirs[d] {
			dirs[d] = true
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: d + "/", Mode: 0o755}); err != nil {
				return err
			}
		}
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: f.Name, Mode: 0o644, Size: int64(len(f.Data))}); err != nil {
			return err
		}
		if _, err := tw.Write(f.Data); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	resp, err := c.send(ctx, c.http, http.MethodPost, "/v2/sandboxes/sessions/"+url.PathEscape(sessionID)+"/fs/write", nil, &buf,
		map[string]string{"Content-Type": "application/gzip", "x-cwd": dir})
	if err != nil {
		return fmt.Errorf("write files: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return nil
}

// ReadFile opens a file in the sandbox session for reading. The caller
// must close it, and should bound how much it reads.
func (c *Client) ReadFile(ctx context.Context, sessionID, filePath string) (io.ReadCloser, error) {
	resp, err := c.send(ctx, c.http, http.MethodPost, "/v2/sandboxes/sessions/"+url.PathEscape(sessionID)+"/fs/read", nil,
		strings.NewReader(`{"path":`+jsonString(filePath)+`}`), nil)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	return resp.Body, nil
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
