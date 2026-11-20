package handlers

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProviderDiscoveryCustomAndUpstreamError(t *testing.T) {
	store := &gcpStore{item: map[string]any{"id": "connection", "provider": "openai-compatible-test", "apiKey": "secret", "providerSpecificData": map[string]any{"baseUrl": "https://example.invalid/v1/"}}}
	calls := 0
	handler := &ProviderDiscovery{Connections: store, Client: &http.Client{Transport: kiroImportTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != "https://example.invalid/v1/models" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatal("wrong discovery request")
		}
		if calls > 1 {
			return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader(`denied`))}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"model"}]}`))}, nil
	})}}
	w := httptest.NewRecorder()
	handler.Get(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"model"`) {
		t.Fatal(w.Body.String())
	}
	w = httptest.NewRecorder()
	handler.Get(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 403 || !strings.Contains(w.Body.String(), "Failed to fetch models: 403") {
		t.Fatal(w.Body.String())
	}
	store.item = nil
	w = httptest.NewRecorder()
	handler.Get(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 404 {
		t.Fatal(w.Code)
	}
}

func TestProviderDiscoveryCodex(t *testing.T) {
	store := &gcpStore{item: map[string]any{"id": "connection", "provider": "codex", "accessToken": "tok"}}
	handler := &ProviderDiscovery{Connections: store, Client: &http.Client{Transport: kiroImportTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://chatgpt.com/backend-api/codex/models" {
			t.Fatal("wrong codex discovery URL: " + r.URL.String())
		}
		if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("originator") != "codex_cli_rs" || r.Header.Get("User-Agent") != "codex_cli_rs/0.159.0" {
			t.Fatal("missing codex identity headers")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"models":[{"id":"gpt-6.1-sol"}]}`))}, nil
	})}}
	w := httptest.NewRecorder()
	handler.Get(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "gpt-6.1-sol") {
		t.Fatal(w.Body.String())
	}
}
