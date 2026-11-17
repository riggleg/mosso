package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// fastResumeRetries shortens resuming's backoff and per-request timeout.
func fastResumeRetries(t *testing.T) {
	t.Helper()
	backoff, timeout, check := resumeBackoff, resumeAttemptTimeout, resumeCheckTimeout
	resumeBackoff, resumeAttemptTimeout, resumeCheckTimeout = 10*time.Millisecond, 300*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { resumeBackoff, resumeAttemptTimeout, resumeCheckTimeout = backoff, timeout, check })
}

// stoppedHarness returns a harness whose sandbox session has ended, with
// the given resume failures scripted. Call fastResumeRetries first.
func stoppedHarness(t *testing.T, faults ...string) *persistentHarness {
	t.Helper()
	h := newPersistentHarness(t)
	h.fake.timeout(h.ws.Name)
	h.waitStatus("stopped")
	h.fake.mu.Lock()
	h.fake.resumeFaults, h.fake.resumes = faults, 0
	h.fake.mu.Unlock()
	return h
}

// connect clicks Resume and returns the status code and body.
func (h *persistentHarness) connect() (int, string) {
	h.t.Helper()
	resp := postJSON(h.t, h.ts.URL+"/workspaces/"+h.ws.Name+"/connect", `{}`)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func (h *persistentHarness) resumes() int {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	return h.fake.resumes
}

func TestResumeRetriesTransientErrors(t *testing.T) {
	fastResumeRetries(t)
	h := stoppedHarness(t, "503", "502")
	if code, body := h.connect(); code != http.StatusOK {
		t.Fatalf("connect = %d: %s", code, body)
	}
	if n := h.resumes(); n != 3 {
		t.Fatalf("resume requests = %d, want 3", n)
	}
	if got := h.fake.session(h.ws.Name); !strings.HasSuffix(got, "_2") {
		t.Fatalf("session = %s, want the second one", got)
	}
}

func TestResumeRetriesAfterATimeout(t *testing.T) {
	fastResumeRetries(t)
	h := stoppedHarness(t, "hang")
	if code, body := h.connect(); code != http.StatusOK {
		t.Fatalf("connect = %d: %s", code, body)
	}
	if n := h.resumes(); n != 2 {
		t.Fatalf("resume requests = %d, want 2", n)
	}
}

// TestResumeTimeoutThatResumedAnyway: the resume went through but its
// answer was lost; Mosso uses the new session instead of resuming again.
func TestResumeTimeoutThatResumedAnyway(t *testing.T) {
	fastResumeRetries(t)
	h := stoppedHarness(t, "resume-hang")
	h.fake.mu.Lock()
	lookups := h.fake.lookups
	h.fake.mu.Unlock()
	if code, body := h.connect(); code != http.StatusOK {
		t.Fatalf("connect = %d: %s", code, body)
	}
	h.fake.mu.Lock()
	lookups = h.fake.lookups - lookups
	h.fake.mu.Unlock()
	if n := h.resumes(); n != 1 || lookups == 0 {
		t.Fatalf("resume requests = %d, lookups = %d; want 1 resume and a lookup", n, lookups)
	}
	if got := h.fake.session(h.ws.Name); !strings.HasSuffix(got, "_2") {
		t.Fatalf("session = %s, want the second one", got)
	}
	if st := h.status(); st.Status != "ready" {
		t.Fatalf("status = %+v", st)
	}
}

func TestResumeGivesUpAfterThreeAttempts(t *testing.T) {
	fastResumeRetries(t)
	h := stoppedHarness(t, "503", "503", "503", "503")
	code, body := h.connect()
	if code != http.StatusBadGateway || !strings.Contains(body, "scripted 503") || !strings.Contains(body, "gave up after 3 attempts") {
		t.Fatalf("connect = %d: %s", code, body)
	}
	if n := h.resumes(); n != 3 {
		t.Fatalf("resume requests = %d, want 3", n)
	}
	if st := h.status(); st.Status != "stopped" {
		t.Fatalf("status = %+v; the sandbox should stay resumable", st)
	}
	h.fake.mu.Lock()
	created, deleted := len(h.fake.created), len(h.fake.deleted)
	h.fake.mu.Unlock()
	if created != 1 || deleted != 0 {
		t.Fatalf("created %d, deleted %d sandboxes while retrying", created, deleted)
	}
	// Once Vercel is back, the next try works.
	if code, body := h.connect(); code != http.StatusOK {
		t.Fatalf("connect = %d: %s", code, body)
	}
}

func TestResumeDoesNotRetryPermanentErrors(t *testing.T) {
	for _, fault := range []string{"400", "401", "403"} {
		t.Run(fault, func(t *testing.T) {
			fastResumeRetries(t)
			h := stoppedHarness(t, fault)
			if code, body := h.connect(); code != http.StatusBadGateway || !strings.Contains(body, "scripted "+fault) {
				t.Fatalf("connect = %d: %s", code, body)
			}
			if n := h.resumes(); n != 1 {
				t.Fatalf("resume requests = %d, want 1", n)
			}
		})
	}
	t.Run("snapshot gone", func(t *testing.T) {
		fastResumeRetries(t)
		h := stoppedHarness(t)
		h.fake.mu.Lock()
		h.fake.snapshotLost = map[string]bool{h.ws.Name: true}
		h.fake.mu.Unlock()
		if code, _ := h.connect(); code != http.StatusBadGateway {
			t.Fatalf("connect = %d", code)
		}
		if n := h.resumes(); n != 1 {
			t.Fatalf("resume requests = %d, want 1", n)
		}
		if st := h.status(); st.Status != "error" || st.Error != goneMessage {
			t.Fatalf("status = %+v", st)
		}
	})
}

func TestResumeStopsWhenCancelled(t *testing.T) {
	fastResumeRetries(t)
	resumeBackoff = time.Hour
	h := stoppedHarness(t, "503", "503")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for h.resumes() == 0 {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()
	start := time.Now()
	_, err := h.srv.resumeSandbox(ctx, h.ws.Name)
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "scripted 503") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 10*time.Second || h.resumes() != 1 {
		t.Fatalf("took %s and %d resume requests", time.Since(start), h.resumes())
	}
}
