package worker

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// bin holds the worker binary for the sandbox (linux/amd64), when `make
// worker` has built it. The directory always has a README so the package
// compiles without the binary.
//
//go:embed bin
var bin embed.FS

var (
	binaryMu     sync.Mutex
	binaryCached []byte
)

// Binary returns the linux/amd64 mosso-worker binary to upload into a
// sandbox: the one embedded by `make worker`, or, when Mosso runs from its
// source tree with `go run`, one cross-compiled now with the Go toolchain
// (cached for the life of the process).
func Binary(ctx context.Context) ([]byte, error) {
	binaryMu.Lock()
	defer binaryMu.Unlock()
	if binaryCached != nil {
		return binaryCached, nil
	}
	if b, err := bin.ReadFile("bin/mosso-worker"); err == nil && len(b) > 0 {
		binaryCached = b
		return b, nil
	}
	b, err := build(ctx)
	if err != nil {
		return nil, err
	}
	binaryCached = b
	return b, nil
}

// BinaryID names a build of the binary, so a sandbox only receives it once.
func BinaryID(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// build cross-compiles cmd/mosso-worker from Mosso's source tree, found
// by walking up from the current directory.
func build(ctx context.Context) ([]byte, error) {
	root, ok := moduleRoot()
	if !ok {
		return nil, errors.New("the mosso-worker binary is not embedded in this build: run `make worker` (or start Mosso from the repository root with `make run`)")
	}
	dir, err := os.MkdirTemp("", "mosso-worker-build-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	out := filepath.Join(dir, "mosso-worker")
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-ldflags=-s -w", "-o", out, "./cmd/mosso-worker")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	if msg, err := cmd.CombinedOutput(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, errors.New("the mosso-worker binary is not embedded in this build and Go is not installed to build it: run `make worker`")
		}
		return nil, fmt.Errorf("build mosso-worker for the sandbox: %w: %s", err, strings.TrimSpace(string(msg)))
	}
	return os.ReadFile(out)
}

// moduleRoot finds the directory holding Mosso's go.mod and worker source,
// starting from the current directory.
func moduleRoot() (string, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "cmd", "mosso-worker", "main.go")); err == nil {
			if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
				return dir, true
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", false
}
