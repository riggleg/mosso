package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthz(t *testing.T) {
	rec := httptest.NewRecorder()
	NewHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	var result map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result["status"] != "ok" {
		t.Fatalf("unexpected health response: %v", result)
	}
}

func TestUnknownRoute(t *testing.T) {
	rec := httptest.NewRecorder()
	NewHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/sessions", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestCheckListenAddr(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080", "127.0.0.2:80"} {
		if err := CheckListenAddr(addr); err != nil {
			t.Errorf("CheckListenAddr(%q) = %v, want nil", addr, err)
		}
	}
	for _, addr := range []string{"0.0.0.0:8080", ":8080", "192.168.1.5:8080", "[::]:8080", "example.com:8080", "127.0.0.1"} {
		if err := CheckListenAddr(addr); err == nil {
			t.Errorf("CheckListenAddr(%q) = nil, want error", addr)
		}
	}
}

func TestLocalOnly(t *testing.T) {
	srv, fake := newTestServer(t)
	h := srv.Handler()
	cases := []struct {
		name        string
		url         string
		contentType string
		origin      string
		want        int
	}{
		{"rebound host", "http://evil.example:8080/sandboxes", "application/json", "", http.StatusForbidden},
		{"cross-origin page", "http://127.0.0.1:8080/sandboxes", "application/json", "https://evil.example", http.StatusForbidden},
		{"simple text/plain POST", "http://127.0.0.1:8080/sandboxes", "text/plain", "", http.StatusUnsupportedMediaType},
		{"form POST", "http://127.0.0.1:8080/sandboxes", "application/x-www-form-urlencoded", "", http.StatusUnsupportedMediaType},
		{"no content type", "http://127.0.0.1:8080/sandboxes", "", "", http.StatusUnsupportedMediaType},
		{"loopback origin", "http://localhost:8080/sandboxes", "application/json; charset=utf-8", "http://localhost:8080", http.StatusCreated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.url, strings.NewReader("{}"))
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.want, rec.Body)
			}
		})
	}
	if len(fake.created) != 1 {
		t.Fatalf("created %d sandboxes, want only the allowed request to create one", len(fake.created))
	}
}
