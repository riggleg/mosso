package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/riggleg/mosso/internal/claude"
	"github.com/riggleg/mosso/internal/vercel"
	"github.com/riggleg/mosso/internal/worker"
)

// Images in Claude's activity (screenshots, charts, images a tool returned
// or Claude's reply refers to) are kept by the sandbox worker in its image
// store, named by content (see internal/worker/images.go); chat entries
// only carry their IDs. GET /workspaces/{name}/images/{id} serves them to
// the web UI. Small ones are also cached on this machine, so they stay
// visible while the sandbox is stopped and after it is gone.

const (
	// maxCachedImageBytes is the largest image cached locally, and
	// maxImageCacheBytes the cache's size per workspace.
	maxCachedImageBytes = 2 << 20
	maxImageCacheBytes  = 64 << 20
	// imageFetchTimeout bounds reading one image from the sandbox.
	imageFetchTimeout = time.Minute
)

// errImageChanged means the sandbox has no valid image for an ID.
var errImageChanged = errors.New("the image is missing or was changed in the sandbox")

// image returns the image with that ID if the chat shows it. Only those
// are served, so a workspace's endpoint never reads anything else.
func (c *chat) image(id string) (claude.Image, bool) {
	for i := len(c.entries) - 1; i >= 0; i-- {
		if ev := c.entries[i].Event; ev != nil {
			for _, img := range ev.Images {
				if img.ID == id {
					return img, true
				}
			}
		}
	}
	return claude.Image{}, false
}

// imageCachePath is where an image of the workspace is cached, or "" if
// Mosso keeps no cache (it has no data directory).
func (s *Server) imageCachePath(name, id string) string {
	if s.imageCache == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") || !worker.ValidImageID(id) {
		return ""
	}
	return filepath.Join(s.imageCache, name, id)
}

// cachedImage returns a cached image, or nil. A file that no longer
// matches its ID is ignored.
func (s *Server) cachedImage(name, id string) []byte {
	p := s.imageCachePath(name, id)
	if p == "" {
		return nil
	}
	data, err := os.ReadFile(p)
	if err != nil || worker.ImageID(data) != id {
		return nil
	}
	return data
}

// cacheImage keeps a small image on this machine, within the workspace's
// cache budget. Failures only mean it is fetched from the sandbox again.
func (s *Server) cacheImage(name, id string, data []byte) {
	p := s.imageCachePath(name, id)
	if p == "" || len(data) > maxCachedImageBytes {
		return
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	var used int64
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() == id {
			return // already cached
		}
		if fi, err := e.Info(); err == nil {
			used += fi.Size()
		}
	}
	if used+int64(len(data)) > maxImageCacheBytes {
		return
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil && cerr == nil {
		_ = os.Rename(tmp.Name(), p)
	}
}

// fetchImage reads an image from the sandbox's image store. It must be an
// image Mosso shows whose content matches its ID: anything else in the
// sandbox under that name (Claude can write there) is refused.
func (s *Server) fetchImage(ctx context.Context, session, id string) ([]byte, error) {
	body, err := s.vercel.ReadFile(ctx, session, worker.ImagesDir+"/"+id)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, worker.MaxImageBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > worker.MaxImageBytes || worker.ImageID(data) != id || worker.ImageType(data) == "" {
		return nil, errImageChanged
	}
	return data, nil
}

// cacheImages caches an event's small images in the background, while the
// sandbox is running, so they can be shown after it stops. Call with s.mu
// held.
func (s *Server) cacheImagesLocked(ws *workspace, images []claude.Image) {
	if s.imageCache == "" || s.vercel == nil || ws.sandboxState != sandboxRunning {
		return
	}
	var ids []string
	for _, img := range images {
		if worker.ValidImageID(img.ID) && img.Size <= maxCachedImageBytes {
			ids = append(ids, img.ID)
		}
	}
	if len(ids) == 0 {
		return
	}
	name, session := ws.Name, ws.Sandbox.SessionID
	s.imageFetches.Add(1)
	go func() {
		defer s.imageFetches.Done()
		select {
		case s.imageSem <- struct{}{}:
			defer func() { <-s.imageSem }()
		case <-s.ctx.Done():
			return
		}
		for _, id := range ids {
			if s.cachedImage(name, id) != nil {
				continue
			}
			ctx, cancel := context.WithTimeout(s.ctx, imageFetchTimeout)
			data, err := s.fetchImage(ctx, session, id)
			cancel()
			if err != nil {
				if s.ctx.Err() == nil {
					log.Printf("workspace %s: cache image %s: %v", name, id, err)
				}
				continue
			}
			s.cacheImage(name, id, data)
		}
	}()
}

// serveImage serves an image the workspace's chat shows: from the local
// cache, or else from the sandbox if it is running. Like attachments, it
// never resumes a stopped sandbox for a thumbnail. Raster images are shown
// inline; SVG too, but sandboxed by its Content-Security-Policy so a
// script in it cannot run even when opened directly. With ?download=1 it
// is a download.
func (s *Server) serveImage(w http.ResponseWriter, r *http.Request) {
	name, id := r.PathValue("name"), r.PathValue("id")
	s.mu.Lock()
	ws, ok := s.workspaces[name]
	var img claude.Image
	known := false
	var session, state string
	var expired bool
	if ok {
		s.loadChatLocked(ws)
		img, known = ws.chat.image(id)
		session, state = ws.Sandbox.SessionID, ws.sandboxState
		expired = !ws.ExpiresAt.IsZero() && time.Now().After(ws.ExpiresAt)
	}
	s.mu.Unlock()
	switch {
	case !ok:
		writeError(w, http.StatusNotFound, "unknown workspace")
		return
	case !worker.ValidImageID(id) || !known:
		writeError(w, http.StatusNotFound, "unknown image")
		return
	}
	data := s.cachedImage(name, id)
	if data == nil {
		switch {
		case noSandbox(state) != "":
			writeError(w, http.StatusGone, "this image is no longer available: the sandbox is gone")
			return
		case s.vercel == nil || state != sandboxRunning || expired:
			writeError(w, http.StatusGone, "the sandbox is stopped; resume it to see this image")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), imageFetchTimeout)
		defer cancel()
		var err error
		data, err = s.fetchImage(ctx, session, id)
		switch {
		case errors.Is(err, errImageChanged) || vercel.IsNotFound(err):
			writeError(w, http.StatusNotFound, errImageChanged.Error())
			return
		case vercel.SessionStopped(err):
			writeError(w, http.StatusGone, "the sandbox is stopped; resume it to see this image")
			return
		case err != nil:
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		s.cacheImage(name, id, data)
	}
	typ := worker.ImageType(data)
	if typ == "" {
		writeError(w, http.StatusNotFound, errImageChanged.Error())
		return
	}
	filename := imageFilename(img, typ)
	h := w.Header()
	disposition := "inline"
	if r.URL.Query().Get("download") != "" {
		disposition, typ = "attachment", "application/octet-stream"
	}
	h.Set("Content-Type", typ)
	h.Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": filename}))
	h.Set("Content-Length", fmt.Sprint(len(data)))
	h.Set("X-Content-Type-Options", "nosniff")
	// No scripts, ever; an SVG may use its own inline styles and data: images.
	h.Set("Content-Security-Policy", "default-src 'none'; img-src data:; style-src 'unsafe-inline'; sandbox")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	// The ID is the content's hash, so a response never goes stale.
	h.Set("Cache-Control", "private, max-age=86400, immutable")
	_, _ = io.Copy(w, bytes.NewReader(data))
}

// imageFilename names a download: the image's own file name, or one made
// from its ID, with an extension that matches its content.
func imageFilename(img claude.Image, typ string) string {
	ext := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp", "image/svg+xml": ".svg"}[typ]
	name := sanitizeFilename(img.Name)
	if img.Name == "" || name == "file" {
		name = "image-" + img.ID[:8]
	}
	if e := strings.ToLower(filepath.Ext(name)); e != ext && !(ext == ".jpg" && e == ".jpeg") {
		name += ext
	}
	return name
}
