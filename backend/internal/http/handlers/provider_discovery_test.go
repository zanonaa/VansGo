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

// TestProviderDiscoveryAntigravity cobre o caso dedicado do handler: o
// catálogo dinâmico do fetchAvailableModels (mesmo módulo do gateway) e o
// erro do fetch virando 502 explícito pro dashboard.
func TestProviderDiscoveryAntigravity(t *testing.T) {
	agModelsCache = map[string]agModelsEntry{}
	failFetch := true
	store := &gcpStore{item: map[string]any{"id": "connection", "provider": "antigravity", "accessToken": "oauth-token", "email": "discovery-user@example.com"}}
	handler := &ProviderDiscovery{Connections: store, Client: &http.Client{Transport: kiroImportTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != agModelsURL || r.Method != http.MethodPost {
			t.Fatalf("discovery request: %s %s", r.Method, r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer oauth-token" || r.Header.Get("User-Agent") != agUserAgent {
			t.Fatalf("discovery headers: %v", r.Header)
		}
		if failFetch {
			return &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader(`boom`))}, nil
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{
			"models": {"gemini-3.8-flash-tiered": {"displayName": "Gemini 3.8 Flash", "maxTokens": 1048576, "maxOutputTokens": 65536}, "gemini-2.5-flash": {"displayName": "Gemini 2.5 Flash"}},
			"tieredModelIds": {"flash": ["gemini-3.8-flash-tiered"]},
			"agentModelSorts": [{"groups": [{"modelIds": ["gemini-2.5-flash"]}]}]
		}`))}, nil
	})}}
	// Falha do fetch vira 502 explícito (antes: 400 "does not support models listing").
	w := httptest.NewRecorder()
	handler.Get(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 502 || !strings.Contains(w.Body.String(), "Failed to fetch models") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	failFetch = false
	w = httptest.NewRecorder()
	handler.Get(w, httptest.NewRequest("GET", "/", nil))
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, `"id":"gemini-2.5-flash"`) || !strings.Contains(body, `"id":"gemini-3.8-flash-tiered-high"`) || !strings.Contains(body, "Gemini 3.8 Flash (High)") {
		t.Fatalf("%d %s", w.Code, body)
	}
	// Ids crus (sem prefixo ag/) pra casar com o fluxo de import do dashboard.
	if strings.Contains(body, `"ag/`) {
		t.Fatal(body)
	}
}

func TestProviderDiscoveryCodex(t *testing.T) {
	store := &gcpStore{item: map[string]any{"id": "connection", "provider": "codex", "accessToken": "tok"}}
	handler := &ProviderDiscovery{Connections: store, Client: &http.Client{Transport: kiroImportTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://chatgpt.com/backend-api/codex/models?client_version=0.159.0" {
			t.Fatal("wrong codex discovery URL: " + r.URL.String())
		}
		if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("originator") != "codex_cli_rs" || r.Header.Get("User-Agent") != "codex_cli_rs/0.159.0" {
			t.Fatal("missing codex identity headers")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"models":[{"slug":"gpt-6.1-sol","display_name":"GPT 6.1 Sol"}]}`))}, nil
	})}}
	w := httptest.NewRecorder()
	handler.Get(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"gpt-6.1-sol"`) || !strings.Contains(w.Body.String(), "GPT 6.1 Sol") {
		t.Fatal(w.Body.String())
	}
}
