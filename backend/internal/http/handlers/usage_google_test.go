package handlers

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestGeminiUsageResolvesProjectAndBuckets(t *testing.T) {
	calls := 0
	u := &UsageContracts{Client: &http.Client{Transport: kiroImportTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		body := `{"cloudaicompanionProject":{"id":" project "},"currentTier":{"name":"Pro"}}`
		if calls == 2 {
			raw, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(raw), `"project":"project"`) {
				t.Fatal(string(raw))
			}
			body = `{"buckets":[{"modelId":"gemini","remainingFraction":0.25,"resetTime":"2026-11-01T00:00:00Z"},{"remainingFraction":1}]}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}}
	result := u.geminiUsage(httptest.NewRequest("GET", "/", nil), "token", map[string]any{})
	quota := result["quotas"].(map[string]any)["gemini"].(map[string]any)
	if result["plan"] != "Pro" || quota["used"] != float64(750) || len(result["quotas"].(map[string]any)) != 1 {
		t.Fatal(result)
	}
}

func TestAntigravityUsageThreeSourcesSingleflight(t *testing.T) {
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	u := &UsageContracts{Client: &http.Client{Transport: kiroImportTransport(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		body := `{}`
		switch {
		case strings.Contains(r.URL.Path, "fetchAvailableModels"):
			if r.Header.Get("User-Agent") != agUserAgent {
				t.Errorf("fetch UA: %s", r.Header.Get("User-Agent"))
			}
			if !strings.Contains(string(raw), `"project":"proj-1"`) {
				t.Errorf("fetch body: %s", raw)
			}
			body = `{"models":{"gemini-3-flash":{"displayName":"Gemini 3 Flash","quotaInfo":{"remainingFraction":1}},"internal":{"isInternal":true,"quotaInfo":{"remainingFraction":1}}}}`
		case strings.Contains(r.URL.Path, "retrieveUserQuotaSummary"):
			if calls.Add(1) == 1 {
				close(started)
				<-release
			}
			body = `{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0.5,"resetTime":"2026-12-02T00:00:00Z"},{"bucketId":"gemini-5h","window":"5h","remainingFraction":1,"resetTime":"2026-11-25T21:41:41Z"}]}]}`
		case strings.Contains(r.URL.Path, "retrieveUserQuota"):
			body = `{"buckets":[{"modelId":"gemini-4-neon-xhigh","remainingFraction":0.9,"resetTime":"2026-11-25T21:41:41Z"},{"modelId":"tab_fast","remainingFraction":1},{"modelId":"claude-opus-5-5-high","remainingFraction":1,"resetTime":"2026-11-25T21:54:56Z"}]}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}}
	connection := map[string]any{"providerSpecificData": map[string]any{"projectId": "proj-1"}}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result := u.antigravityUsage(httptest.NewRequest("GET", "/", nil), "token", connection)
			quotas := result["quotas"].(map[string]any)
			// Fonte 1: catálogo dinâmico (sem allowlist estática); internal segue fora.
			if quotas["gemini-3-flash"] == nil || quotas["internal"] != nil {
				t.Error(quotas)
			}
			// Fonte 2: retrieveUserQuota per-model; tab_* fora.
			neon := quotas["gemini-4-neon-xhigh"].(map[string]any)
			if neon["used"] != float64(100) || quotas["tab_fast"] != nil {
				t.Error(neon, quotas["tab_fast"])
			}
			// Fonte 3: summary por família com janelas independentes.
			// 5h com fraction 1 e resetTime é janela cheia (used 0), não unlimited.
			session := quotas["gemini_session"].(map[string]any)
			if quotas["gemini_weekly"].(map[string]any)["used"] != float64(500) || session["used"] != float64(0) || session["unlimited"] == true {
				t.Error(quotas["gemini_weekly"], session)
			}
		}()
	}
	<-started
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("weekly calls %d", calls.Load())
	}
}

// O painel de quota não pode ficar preso em "authentication expired" quando o
// token morre entre dois refreshes proativos: em 401, renova com o refresh
// token, persiste e re-tenta uma vez (mesma mecânica do gateway e do grok).
func TestAntigravityUsageRefreshesExpiredToken(t *testing.T) {
	modelsCalls := 0
	u := &UsageContracts{Connections: &usageRefreshStore{connection: map[string]any{"id": "conn"}}, Client: &http.Client{Transport: kiroImportTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "oauth2.googleapis.com" {
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "stale" || r.Form.Get("client_id") == "" {
				t.Fatalf("form de refresh inesperado: %v", r.Form)
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"access_token":"fresh","expires_in":3600}`))}, nil
		}
		if strings.Contains(r.URL.Path, "fetchAvailableModels") {
			modelsCalls++
			if r.Header.Get("Authorization") == "Bearer stale" {
				return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader(`{"error":"Unauthorized"}`))}, nil
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"models":{"gemini-3-flash":{"displayName":"Gemini 3 Flash","quotaInfo":{"remainingFraction":0.5}}}}`))}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})}}
	store := u.Connections.(*usageRefreshStore)
	store.connection["accessToken"] = "stale"
	store.connection["refreshToken"] = "stale"
	store.connection["providerSpecificData"] = map[string]any{"projectId": "proj-1"}

	result := u.antigravityUsage(httptest.NewRequest("GET", "/", nil), "stale", store.connection)
	if message := ttsString(result["message"]); strings.Contains(message, "authentication expired") {
		t.Fatalf("não renovou: %s", message)
	}
	if store.updated["accessToken"] != "fresh" || store.updated["expiresIn"] == nil {
		t.Fatalf("patch não persistido: %v", store.updated)
	}
	if quotas, _ := result["quotas"].(map[string]any); quotas["gemini-3-flash"] == nil {
		t.Fatalf("quotas perdidas após o refresh: %v", result)
	}
	if modelsCalls != 2 {
		t.Fatalf("fetchAvailableModels chamado %d vezes", modelsCalls)
	}
}

// Refresh que falha (ou ausente) preserva o comportamento atual: mensagem de
// expirado, sem quotas — o painel continua honesto sobre o estado.
func TestAntigravityUsageExpiredWithoutRefreshKeepsMessage(t *testing.T) {
	modelsCalls := 0
	u := &UsageContracts{Connections: &usageRefreshStore{connection: map[string]any{"id": "conn"}}, Client: &http.Client{Transport: kiroImportTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "oauth2.googleapis.com" {
			return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"error":"invalid_grant"}`))}, nil
		}
		if strings.Contains(r.URL.Path, "fetchAvailableModels") {
			modelsCalls++
			return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader(`{"error":"Unauthorized"}`))}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})}}
	store := u.Connections.(*usageRefreshStore)
	store.connection["accessToken"] = "stale"
	store.connection["refreshToken"] = "stale"
	store.connection["providerSpecificData"] = map[string]any{"projectId": "proj-1"}

	result := u.antigravityUsage(httptest.NewRequest("GET", "/", nil), "stale", store.connection)
	if message := ttsString(result["message"]); !strings.Contains(message, "authentication expired") {
		t.Fatalf("mensagem de expirado perdida: %v", result)
	}
	if result["authExpired"] != true {
		t.Fatalf("authExpired ausente: %v", result)
	}
	if store.updated != nil {
		t.Fatalf("patch persistido com refresh falho: %v", store.updated)
	}
	if modelsCalls != 1 {
		t.Fatalf("fetchAvailableModels chamado %d vezes (refresh falho não deve re-tentar)", modelsCalls)
	}
}

func TestParseWeeklyQuotaArgonAndUnlimited(t *testing.T) {
	data := func(raw string) map[string]any {
		var parsed map[string]any
		_ = json.Unmarshal([]byte(raw), &parsed)
		return parsed
	}
	// Argon detectado pelo texto do grupo (conta Ultra).
	result := parseWeeklyQuota(data(`{"groups":[{"displayName":"Gemini 4 Argon","buckets":[{"bucketId":"argon-weekly","window":"weekly","remainingFraction":0.25,"resetTime":"2026-12-02T00:00:00Z"}]}]}`))
	if result["gemini_4_argon_weekly"] == nil || result["gemini_4_argon_weekly"].(map[string]any)["used"] != float64(750) {
		t.Fatal(result)
	}
	// unlimited: fraction ≥ 1 sem resetTime (display mantém label da família).
	result = parseWeeklyQuota(data(`{"groups":[{"displayName":"Claude and GPT models","buckets":[{"bucketId":"3p-5h","window":"5h","remainingFraction":1}]}]}`))
	entry := result["claude_gpt_session"].(map[string]any)
	if entry["unlimited"] != true || entry["total"] != float64(0) || entry["displayName"] != "Claude & GPT (5h)" {
		t.Fatal(result)
	}
	// fraction aninhado em bucket.remaining (defensivo, formato OmniRoute).
	result = parseWeeklyQuota(data(`{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-weekly","remaining":{"remainingFraction":0.75,"resetTime":"2026-12-02T00:00:00Z"}}]}]}`))
	if result["gemini_weekly"].(map[string]any)["used"] != float64(250) {
		t.Fatal(result)
	}
}
