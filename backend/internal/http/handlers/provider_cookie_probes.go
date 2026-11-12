package handlers

// Sondas de validação dos providers webCookie: um request leve com o cookie
// da sessão. Cookie válido = resposta != 401/403 (400/429 também provam que a
// sessão foi aceita), espelhando a origem hikers-router.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const cookieProbeChromeUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36"

// cookieProbeError descreve a falha de uma sonda: 401/403 viram mensagem de
// cookie inválido; os demais erros são de rede/upstream.
type cookieProbeError struct {
	status int
}

func (e cookieProbeError) Error() string { return fmt.Sprintf("probe returned %d", e.status) }

func isCookieProbeRejected(err error) bool {
	probe, ok := err.(cookieProbeError)
	return ok && (probe.status == 401 || probe.status == 403)
}

// cookieProbeDo dispara o request da sonda e devolve o corpo (limitado) junto
// do status; rejeição de sessão vira cookieProbeError.
func cookieProbeDo(ctx context.Context, client *http.Client, request *http.Request) ([]byte, error) {
	requestCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	request = request.WithContext(requestCtx)
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
	if response.StatusCode == 401 || response.StatusCode == 403 {
		return body, cookieProbeError{status: response.StatusCode}
	}
	return body, nil
}

// cookieProbeDeepSeek pede um proof-of-work: prova o Bearer token sem gastar
// uma mensagem de chat.
func cookieProbeDeepSeek(ctx context.Context, client *http.Client, token string) error {
	payload, _ := json.Marshal(map[string]any{"target_path": "/api/v0/chat/completion"})
	request, err := http.NewRequestWithContext(ctx, "POST", "https://chat.deepseek.com/api/v0/chat/create_pow_challenge", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", cookieProbeChromeUA)
	_, err = cookieProbeDo(ctx, client, request)
	return err
}

// cookieProbeKimi reusa o executor Connect-RPC com um turno mínimo "ping".
func cookieProbeKimi(ctx context.Context, client *http.Client, token string) error {
	body := kimiWebChatBody("ping", "")
	body["options"].(map[string]any)["thinking"] = false
	encoded := kimiWebFrame(body)
	request, err := http.NewRequestWithContext(ctx, "POST", kimiWebChatURL, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/connect+json")
	request.Header.Set("Accept", "*/*")
	request.Header.Set("connect-protocol-version", "1")
	request.Header.Set("Origin", kimiWebBaseURL)
	request.Header.Set("Referer", kimiWebBaseURL+"/")
	request.Header.Set("User-Agent", kimiWebUA)
	request.Header.Set("Authorization", "Bearer "+token)
	_, err = cookieProbeDo(ctx, client, request)
	return err
}

// cookieProbeMuseSpark replica validateMuseSparkConnection da origem.
func cookieProbeMuseSpark(ctx context.Context, client *http.Client, token string) error {
	payload, _ := json.Marshal(map[string]any{
		"doc_id": "29ae946c82d1f301196c6ca2226400b5",
		"variables": map[string]any{
			"assistantMessageId": newKimiWebNonce(),
			"content":            "ping",
			"conversationId":     "c." + newKimiWebNonce(),
			"currentBranchPath":  "0",
			"entryPoint":         "KADABRA__CHAT__UNIFIED_INPUT_BAR",
			"isNewConversation":  true,
			"mode":               "mode_fast",
			"turnId":             newKimiWebNonce(),
			"userMessageId":      newKimiWebNonce(),
		},
	})
	request, err := http.NewRequestWithContext(ctx, "POST", "https://www.meta.ai/api/graphql", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Cookie", "ecto_1_sess="+token)
	request.Header.Set("Origin", "https://www.meta.ai")
	request.Header.Set("Referer", "https://www.meta.ai/")
	request.Header.Set("User-Agent", cookieProbeChromeUA)
	_, err = cookieProbeDo(ctx, client, request)
	return err
}

// cookieProbeGrok replica o probe Cloudflare-bypass da origem: fingerprint
// Chrome completo com x-statsig-id e x-xai-request-id.
func cookieProbeGrok(ctx context.Context, client *http.Client, token string) error {
	statsig := base64.StdEncoding.EncodeToString([]byte("e:TypeError: Cannot read properties of null (reading 'children')"))
	trace := make([]byte, 16)
	span := make([]byte, 8)
	_, _ = rand.Read(trace)
	_, _ = rand.Read(span)
	requestID := newKimiWebNonce()
	payload, _ := json.Marshal(map[string]any{
		"temporary": true, "modelName": "grok-4", "modelMode": "MODEL_MODE_GROK_4", "message": "ping",
		"fileAttachments": []any{}, "imageAttachments": []any{},
		"disableSearch": false, "enableImageGeneration": false, "returnImageBytes": false,
		"returnRawGrokInXaiRequest": false, "enableImageStreaming": false, "imageGenerationCount": 0,
		"forceConcise": false, "toolOverrides": map[string]any{}, "enableSideBySide": true, "sendFinalMetadata": true,
		"isReasoning": false, "disableTextFollowUps": true, "disableMemory": true,
		"forceSideBySide": false, "isAsyncChat": false, "disableSelfHarmShortCircuit": false,
	})
	request, err := http.NewRequestWithContext(ctx, "POST", "https://grok.com/rest/app-chat/conversations/new", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "*/*")
	request.Header.Set("Accept-Language", "en-US,en;q=0.9")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Cookie", "sso="+token)
	request.Header.Set("Origin", "https://grok.com")
	request.Header.Set("Referer", "https://grok.com/")
	request.Header.Set("Sec-Ch-Ua", `"Google Chrome";v="136", "Chromium";v="136", "Not(A:Brand";v="24"`)
	request.Header.Set("Sec-Ch-Ua-Mobile", "?0")
	request.Header.Set("Sec-Ch-Ua-Platform", `"macOS"`)
	request.Header.Set("Sec-Fetch-Dest", "empty")
	request.Header.Set("Sec-Fetch-Mode", "cors")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	request.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36")
	request.Header.Set("x-statsig-id", statsig)
	request.Header.Set("x-xai-request-id", requestID)
	request.Header.Set("traceparent", fmt.Sprintf("00-%s-%s-00", hex.EncodeToString(trace), hex.EncodeToString(span)))
	_, err = cookieProbeDo(ctx, client, request)
	return err
}

// cookieProbePerplexity replica o probe perplexity_ask da origem.
func cookieProbePerplexity(ctx context.Context, client *http.Client, token string) error {
	requestID := newKimiWebNonce()
	contextID := newKimiWebNonce()
	payload, _ := json.Marshal(map[string]any{
		"query_str": "ping",
		"params": map[string]any{
			"query_str": "ping", "search_focus": "internet", "mode": "concise", "model_preference": "pplx_pro",
			"sources": []any{"web"}, "attachments": []any{},
			"frontend_uuid": requestID, "frontend_context_uuid": contextID,
			"version": "2.18", "language": "en-US", "timezone": "UTC",
			"search_recency_filter": nil, "is_incognito": true, "use_schematized_api": true, "last_backend_uuid": nil,
		},
	})
	request, err := http.NewRequestWithContext(ctx, "POST", "https://www.perplexity.ai/rest/sse/perplexity_ask", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Origin", "https://www.perplexity.ai")
	request.Header.Set("Referer", "https://www.perplexity.ai/")
	request.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36")
	request.Header.Set("X-App-ApiClient", "default")
	request.Header.Set("X-App-ApiVersion", "2.18")
	request.Header.Set("Cookie", "__Secure-next-auth.session-token="+token)
	_, err = cookieProbeDo(ctx, client, request)
	return err
}

// claudeWebNormalizeCookie aceita o valor cru do sessionKey ou um header
// Cookie: completo colado (contendo "="): o par verbatim preserva cf_clearance.
func claudeWebNormalizeCookie(raw string) string {
	value := strings.TrimSpace(raw)
	value = strings.TrimPrefix(value, "bearer ")
	value = strings.TrimPrefix(strings.TrimPrefix(value, "Cookie:"), "cookie:")
	value = strings.TrimSpace(value)
	if strings.Contains(value, "=") {
		return value
	}
	return "sessionKey=" + value
}

// cookieProbeClaude valida o sessionKey com o GET /organizations do claude.ai;
// 403 com marcador Cloudflare é challenge e não cookie inválido.
func cookieProbeClaude(ctx context.Context, client *http.Client, token string) error {
	request, err := http.NewRequestWithContext(ctx, "GET", claudeWebOrganizationsURL, nil)
	if err != nil {
		return err
	}
	for key, value := range claudeWebBrowserHeaders() {
		request.Header.Set(key, value)
	}
	request.Header.Set("Cookie", claudeWebNormalizeCookie(token))
	body, err := cookieProbeDo(ctx, client, request)
	if err != nil {
		if probe, ok := err.(cookieProbeError); ok && probe.status == 403 && claudeWebIsChallenge(probe.status, request.Header.Get("cf-mitigated"), string(body)) {
			return fmt.Errorf("Cloudflare challenge — retry from a browser IP or paste the full Cookie header with cf_clearance")
		}
		return err
	}
	return nil
}

// webCookieProbe roteia a sonda de cada provider cookie; retorna a mensagem de
// erro pronta para o usuário quando a sessão foi rejeitada.
func webCookieProbe(ctx context.Context, client *http.Client, provider, token string) error {
	switch provider {
	case "deepseek-web":
		return cookieProbeDeepSeek(ctx, client, token)
	case "kimi-web":
		return cookieProbeKimi(ctx, client, token)
	case "muse-spark-web":
		return cookieProbeMuseSpark(ctx, client, token)
	case "grok-web":
		return cookieProbeGrok(ctx, client, token)
	case "perplexity-web":
		return cookieProbePerplexity(ctx, client, token)
	case "claude-web":
		return cookieProbeClaude(ctx, client, token)
	}
	return fmt.Errorf("unsupported provider: %s", provider)
}

func webCookieProbeError(provider string) string {
	switch provider {
	case "deepseek-web":
		return "Invalid DeepSeek token — paste the 64-char Bearer token from chat.deepseek.com"
	case "kimi-web":
		return "Invalid Kimi access_token — capture access_token from localStorage at www.kimi.ai"
	case "muse-spark-web":
		return "Invalid session cookie — re-paste ecto_1_sess from meta.ai"
	case "grok-web":
		return "Invalid SSO cookie — re-paste from grok.com DevTools → Cookies → sso"
	case "perplexity-web":
		return "Invalid session cookie — re-paste __Secure-next-auth.session-token from perplexity.ai"
	case "claude-web":
		return "Invalid sessionKey — re-paste from claude.ai DevTools → Cookies → sessionKey"
	}
	return "Invalid API key"
}
