package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// claudeWebMockUpstream verifica o formato das chamadas claude.ai do executor
// e responde o SSE Anthropic canônico (message_start → deltas → message_stop).
type claudeWebMockUpstream struct {
	organizations int
	completions   int
	respond       func() string
}

func (u *claudeWebMockUpstream) handle(w *httptest.ResponseRecorder, r *http.Request) {
	if r.URL.Path == "/api/organizations" {
		u.organizations++
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`[{"uuid":"org-1","name":"Org"}]`))
		return
	}
	if strings.HasSuffix(r.URL.Path, "/completion") {
		u.completions++
		var payload map[string]any
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &payload)
		text := u.respond()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteString("data: {\"type\":\"message_start\"}\n\n")
		w.WriteString("data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"" + text + "\"}}\n\n")
		w.WriteString("data: {\"type\":\"message_stop\"}\n\n")
	}
}

func claudeWebTestSend(upstream *claudeWebMockUpstream) *TranslatorSend {
	return &TranslatorSend{
		Connections: sendConnections{[]map[string]any{{
			"id":       "conn-claude-web",
			"provider": "claude-web",
			"isActive": true,
			"apiKey":   "sk-ant-test",
		}}},
		Client: &http.Client{Transport: kiroImportTransport(func(r *http.Request) (*http.Response, error) {
			recorder := httptest.NewRecorder()
			upstream.handle(recorder, r)
			response := recorder.Result()
			response.Body = io.NopCloser(recorder.Body)
			return response, nil
		})},
		GetPool: func(ctx context.Context, id string) (map[string]any, error) { return nil, nil },
	}
}

func TestClaudeWebSendE2E(t *testing.T) {
	upstream := &claudeWebMockUpstream{respond: func() string { return "olá claude" }}
	send := claudeWebTestSend(upstream)
	w := httptest.NewRecorder()
	send.SendClaudeWeb(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"stream":false}`)), "claude-web", "claude-sonnet-4-6", map[string]any{
		"model":    "claude-sonnet-4-6",
		"messages": []any{map[string]any{"role": "user", "content": "diga olá"}},
	}, "openai", false)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if upstream.organizations != 1 || upstream.completions != 1 {
		t.Fatalf("upstream org=%d completion=%d", upstream.organizations, upstream.completions)
	}
	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if len(result.Choices) != 1 || result.Choices[0].Message.Content != "olá claude" || result.Choices[0].FinishReason != "stop" {
		t.Fatalf("conteúdo: %s", w.Body.String())
	}
}

func TestClaudeWebSendE2EStream(t *testing.T) {
	upstream := &claudeWebMockUpstream{respond: func() string { return "fluxo" }}
	send := claudeWebTestSend(upstream)
	w := httptest.NewRecorder()
	send.SendClaudeWeb(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"stream":true}`)), "claude-web", "claude-sonnet-4-6", map[string]any{
		"model":    "claude-sonnet-4-6",
		"messages": []any{map[string]any{"role": "user", "content": "ping"}},
	}, "openai", true)
	body := w.Body.String()
	if !strings.Contains(body, `"content":"fluxo"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("stream incompleto: %s", body)
	}
}

func TestClaudeWebChallengeDetection(t *testing.T) {
	if !claudeWebIsChallenge(403, "challenge", "") {
		t.Fatal("cf-mitigated header não detectado")
	}
	if !claudeWebIsChallenge(403, "", "<html><title>Just a moment...</title></html>") {
		t.Fatal("corpo do challenge não detectado")
	}
	if claudeWebIsChallenge(403, "", "regular error") || claudeWebIsChallenge(401, "challenge", "") {
		t.Fatal("falso positivo")
	}
}

func TestClaudeWebNormalizeCookie(t *testing.T) {
	cases := map[string]string{
		"sk-ant-sid01-abc":                            "sessionKey=sk-ant-sid01-abc",
		"sessionKey=sk-ant-sid01-abc":                 "sessionKey=sk-ant-sid01-abc",
		"sessionKey=sk-ant-sid01-abc; cf_clearance=x": "sessionKey=sk-ant-sid01-abc; cf_clearance=x",
		"Cookie: sessionKey=sk-ant-sid01-abc":         "sessionKey=sk-ant-sid01-abc",
	}
	for input, expected := range cases {
		if got := claudeWebNormalizeCookie(input); got != expected {
			t.Fatalf("normalize(%q) = %q, esperado %q", input, got, expected)
		}
	}
}

func TestWebCookieProbeRoutes(t *testing.T) {
	// deepseek-web: sonda acerta o pow challenge com Bearer e body correto.
	var seenPath, seenAuth string
	probe := &ProviderValidation{Client: &http.Client{Transport: kiroImportTransport(func(r *http.Request) (*http.Response, error) {
		seenPath, seenAuth = r.URL.Path, r.Header.Get("Authorization")
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})}}
	w := httptest.NewRecorder()
	probe.Post(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"provider":"deepseek-web","apiKey":"token-64"}`)))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"valid":true`) {
		t.Fatalf("deepseek probe: %d %s", w.Code, w.Body.String())
	}
	if seenPath != "/api/v0/chat/create_pow_challenge" || seenAuth != "Bearer token-64" {
		t.Fatalf("probe errado: %s %s", seenPath, seenAuth)
	}

	// deepseek-web: code!=0 no corpo (HTTP 200) vira cookie rejeitado.
	probe = &ProviderValidation{Client: &http.Client{Transport: kiroImportTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"code":40003,"msg":"Authorization Failed (invalid token)"}`))}, nil
	})}}
	w = httptest.NewRecorder()
	probe.Post(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"provider":"deepseek-web","apiKey":"tok"}`)))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Authorization Failed") {
		t.Fatalf("deepseek body code: %d %s", w.Code, w.Body.String())
	}

	// kimi-web: cookie rejeitado vira mensagem específica, não 502.
	probe = &ProviderValidation{Client: &http.Client{Transport: kiroImportTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})}}
	w = httptest.NewRecorder()
	probe.Post(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"provider":"kimi-web","apiKey":"tok"}`)))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Invalid Kimi access_token") {
		t.Fatalf("kimi probe: %d %s", w.Code, w.Body.String())
	}

	// Provider desconhecido continua "validation unavailable".
	w = httptest.NewRecorder()
	probe.Post(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"provider":"sem-probe","apiKey":"k"}`)))
	if w.Code != 400 || !strings.Contains(w.Body.String(), "validation unavailable") {
		t.Fatalf("unsupported: %d %s", w.Code, w.Body.String())
	}
}

func TestConnectionProbeWebCookie(t *testing.T) {
	store := &gcpStore{item: map[string]any{"id": "cw", "provider": "claude-web", "apiKey": "sk-ant-test"}}
	probe := &ConnectionProbe{Connections: store, Client: &http.Client{Transport: kiroImportTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/api/organizations" || r.Header.Get("Cookie") != "sessionKey=sk-ant-test" {
			t.Fatalf("probe request: %s %s", r.URL.Path, r.Header.Get("Cookie"))
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`[{"uuid":"org-1"}]`))}, nil
	})}}
	result, err := probe.Test(context.Background(), "cw")
	if err != nil || result["valid"] != true || store.patch["testStatus"] != "active" {
		t.Fatalf("result %v err %v patch %v", result, err, store.patch)
	}
}
