package handlers

// Claude Web (claude.ai consumer) fala SSE estilo Anthropic no endpoint de
// completion da organização. Porta do executor claude-web do OmniRoute
// (release/v3.8.54): org via GET /organizations, turno via POST completion.
// Sem solver Turnstile/TLS-impersonate no primeiro port — challenge do
// Cloudflare é reportado ao cliente em vez de resolvido.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	claudeWebBaseURL = "https://claude.ai/api"
	// claudeWebOrganizationsURL valida o cookie e descobre a organização.
	claudeWebOrganizationsURL = claudeWebBaseURL + "/organizations"
	// claudeWebCompletionURLTemplate: {0}=org uuid, {1}=conversation uuid.
	claudeWebCompletionURLTemplate = claudeWebBaseURL + "/organizations/%s/chat_conversations/%s/completion"
	// claudeWebDefaultModel segue o OmniRoute (claude-sonnet-4-6).
	claudeWebDefaultModel = "claude-sonnet-4-6"
	claudeWebTimezone     = "America/Sao_Paulo"
	claudeWebLocale       = "en-US"
	claudeWebUA           = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36"
	claudeWebChromeVer    = "149"
)

// claudeWebBrowserHeaders é o fingerprint Chrome do OmniRoute (v2-linux-unified).
// Cloudflare amarra cf_clearance ao fingerprint: use o MESMO conjunto em todas
// as chamadas claude.ai da sessão.
func claudeWebBrowserHeaders() map[string]string {
	return map[string]string{
		"Accept":                    "text/event-stream",
		"Accept-Language":           claudeWebLocale + ",en;q=0.9",
		"Content-Type":              "application/json",
		"Origin":                    "https://claude.ai",
		"Referer":                   "https://claude.ai/new",
		"Sec-Ch-Ua":                 `"Chromium";v="` + claudeWebChromeVer + `", "Not-A.Brand";v="24", "Google Chrome";v="` + claudeWebChromeVer + `"`,
		"Sec-Ch-Ua-Mobile":          "?0",
		"Sec-Ch-Ua-Platform":        `"Linux"`,
		"Sec-Fetch-Dest":            "empty",
		"Sec-Fetch-Mode":            "cors",
		"Sec-Fetch-Site":            "same-origin",
		"User-Agent":                claudeWebUA,
		"anthropic-client-platform": "web_claude_ai",
	}
}

// claudeWebIsChallenge detecta a página de challenge do Cloudflare: 403 com
// header cf-mitigated: challenge ou corpo "Just a moment"/"Attention Required".
func claudeWebIsChallenge(status int, cfMitigated, body string) bool {
	if status != http.StatusForbidden {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(cfMitigated), "challenge") {
		return true
	}
	return strings.Contains(body, "Just a moment") || strings.Contains(body, "Attention Required")
}

// SendClaudeWeb expõe o executor do claude.ai ao gateway e ao node.
func (t *TranslatorSend) SendClaudeWeb(w http.ResponseWriter, r *http.Request, provider, model string, body map[string]any, source string, stream bool) {
	t.sendClaudeWeb(w, r, provider, model, body, source, stream)
}

// sendClaudeWeb executa o chat do claude.ai e dobra o stream SSE Anthropic de
// volta para o formato do cliente (SSE/JSON OpenAI; Claude/Responses convertidos).
func (t *TranslatorSend) sendClaudeWeb(w http.ResponseWriter, r *http.Request, provider, model string, body map[string]any, source string, stream bool) {
	fail := func(status int, message string) {
		nodeReply(w, status, map[string]any{"success": false, "error": message})
	}
	connections, err := t.Connections.List(r.Context(), provider, nil)
	if err != nil {
		fail(500, err.Error())
		return
	}
	var connection map[string]any
	for _, candidate := range connections {
		if candidate["isActive"] != false {
			connection = candidate
			break
		}
	}
	if connection == nil {
		fail(400, "No active connection for provider: "+provider)
		return
	}
	usageSelected(r.Context(), connection)
	access := ttsString(firstNonEmpty(connection["apiKey"], connection["accessToken"]))
	if access == "" {
		fail(400, "Missing Claude sessionKey — paste the cookie from claude.ai DevTools → Cookies → sessionKey")
		return
	}
	if model == "" || model == "default" {
		model = claudeWebDefaultModel
	}

	intermediate := body
	if source != "openai" {
		if intermediate, err = translateSource(body, source); err != nil {
			fail(400, err.Error())
			return
		}
	}

	prompt, _, err := kimiWebFoldMessages(intermediate["messages"])
	if err != nil {
		fail(400, err.Error())
		return
	}
	if prompt == "" {
		fail(400, "Claude Web requires a non-empty user message")
		return
	}

	metadata, _ := connection["providerSpecificData"].(map[string]any)
	client, relay, cleanup, err := connectionHTTP(r.Context(), metadata, t.GetPool, t.Client)
	if err != nil {
		fail(500, err.Error())
		return
	}
	defer cleanup()

	// Org discovery: o cookie valida e devolve [{uuid,...}]. Também serve de
	// sonda de sessão antes de gastar o turno.
	organizations, orgStatus, _, orgErr := claudeWebListOrganizations(r.Context(), client, relay, access)
	if orgErr != nil {
		fail(orgStatus, orgErr.Error())
		return
	}
	orgID := ""
	for _, org := range organizations {
		if id := ttsString(firstNonEmpty(org["uuid"], org["id"])); id != "" {
			orgID = id
			break
		}
	}
	if orgID == "" {
		fail(502, "Claude did not return an organization for this session")
		return
	}

	conversationID := uuid.NewString()
	requestedTools, _ := intermediate["tools"].([]any)
	payload := claudeWebCompletionPayload(prompt, model, requestedTools)
	encoded, _ := json.Marshal(payload)
	completionURL := fmt.Sprintf(claudeWebCompletionURLTemplate, orgID, conversationID)
	request, err := http.NewRequestWithContext(r.Context(), "POST", completionURL, bytes.NewReader(encoded))
	if err != nil {
		fail(500, err.Error())
		return
	}
	for key, value := range claudeWebBrowserHeaders() {
		request.Header.Set(key, value)
	}
	request.Header.Set("Cookie", claudeWebNormalizeCookie(access))
	if err := rewriteConnectionRelay(request, relay); err != nil {
		fail(500, err.Error())
		return
	}

	response, err := client.Do(request)
	if err != nil {
		fail(502, err.Error())
		return
	}
	defer response.Body.Close()

	// Página de challenge do Cloudflare chega como HTML 403 e não como SSE.
	if response.StatusCode == http.StatusForbidden {
		sniff, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
		if claudeWebIsChallenge(response.StatusCode, response.Header.Get("cf-mitigated"), string(sniff)) {
			fail(403, "Cloudflare challenge on claude.ai — retry from a browser IP or paste the full Cookie header with cf_clearance")
			return
		}
		fail(403, "Claude session rejected — update the connection with a fresh sessionKey from claude.ai")
		return
	}
	if response.StatusCode == http.StatusUnauthorized {
		io.Copy(io.Discard, io.LimitReader(response.Body, 8<<10))
		fail(401, "Claude session expired — update the connection with a fresh sessionKey from claude.ai")
		return
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		details, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
		fail(response.StatusCode, fmt.Sprintf("Claude error: %d — %s", response.StatusCode, strings.TrimSpace(string(details))))
		return
	}
	if !strings.Contains(response.Header.Get("Content-Type"), "event-stream") {
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		fail(502, "Claude unexpected response: "+strings.TrimSpace(string(raw)))
		return
	}

	reader := bufio.NewReader(response.Body)
	if stream {
		t.claudeWebStreamRelay(w, reader, model)
		return
	}
	text, think, err := claudeWebAccumulate(reader)
	if err != nil {
		fail(502, "Claude stream error: "+err.Error())
		return
	}

	content := text
	if usage, ok := r.Context().Value(usageContextKey{}).(*requestUsage); ok && usage != nil {
		usage.addText(content)
	}

	if !stream {
		message := map[string]any{"role": "assistant", "content": content}
		if think != "" {
			message["reasoning_content"] = think
		}
		nodeReply(w, 200, map[string]any{
			"id":      fmt.Sprintf("chatcmpl-claude-web-%d", time.Now().UnixNano()),
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": "stop"}},
		})
		return
	}
	writeOpenAIDeltas(w, model, think, content, nil, "stop")
}

// claudeWebStreamRelay repassa o SSE do claude.ai como SSE OpenAI em tempo real.
func (t *TranslatorSend) claudeWebStreamRelay(w http.ResponseWriter, reader *bufio.Reader, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)
	id := fmt.Sprintf("chatcmpl-claude-web-%d", time.Now().UnixNano())
	writeChunk := func(delta map[string]any, fin any) {
		encoded, _ := json.Marshal(map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(),
			"model":   model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": fin}},
		})
		fmt.Fprintf(w, "data: %s\n\n", encoded)
		if flusher != nil {
			flusher.Flush()
		}
	}
	emitted := false
	_, _ = claudeWebScan(reader, func(text, think string) {
		if !emitted {
			emitted = true
			writeChunk(map[string]any{"role": "assistant", "content": ""}, nil)
		}
		if think != "" {
			writeChunk(map[string]any{"reasoning_content": think}, nil)
		}
		if text != "" {
			writeChunk(map[string]any{"content": text}, nil)
		}
	})
	writeChunk(map[string]any{}, "stop")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// claudeWebListOrganizations executa GET /organizations com o cookie da sessão.
// status/challenge distinguem 401 (sessão), 403 challenge (Cloudflare) e erro
// de rede para a mensagem correta no executor e no probe.
func claudeWebListOrganizations(ctx context.Context, client *http.Client, relay, cookie string) ([]map[string]any, int, bool, error) {
	request, err := http.NewRequestWithContext(ctx, "GET", claudeWebOrganizationsURL, nil)
	if err != nil {
		return nil, 0, false, err
	}
	for key, value := range claudeWebBrowserHeaders() {
		request.Header.Set(key, value)
	}
	request.Header.Set("Cookie", claudeWebNormalizeCookie(cookie))
	if err := rewriteConnectionRelay(request, relay); err != nil {
		return nil, 0, false, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, 502, false, err
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode == http.StatusUnauthorized {
		return nil, 401, false, fmt.Errorf("Claude session expired — update the connection with a fresh sessionKey from claude.ai")
	}
	if claudeWebIsChallenge(response.StatusCode, response.Header.Get("cf-mitigated"), string(raw)) {
		return nil, 403, true, fmt.Errorf("Cloudflare challenge on claude.ai — retry from a browser IP or paste the full Cookie header with cf_clearance")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, response.StatusCode, false, fmt.Errorf("Claude organizations error: %d — %s", response.StatusCode, strings.TrimSpace(string(raw)))
	}
	var organizations []map[string]any
	if err := json.Unmarshal(raw, &organizations); err != nil {
		return nil, response.StatusCode, false, fmt.Errorf("Claude organizations payload inesperado")
	}
	return organizations, response.StatusCode, false, nil
}

// claudeWebCompletionPayload monta o corpo do turno conforme o OmniRoute
// (payload.ts): prompt, estilos padrão, uuids do turno e params de criação.
func claudeWebCompletionPayload(prompt, model string, tools []any) map[string]any {
	nativeTools := make([]any, 0, len(tools))
	for _, tool := range tools {
		entry, _ := tool.(map[string]any)
		function, _ := entry["function"].(map[string]any)
		if entry == nil || function == nil {
			continue
		}
		native := map[string]any{"name": ttsString(function["name"])}
		if description := ttsString(function["description"]); description != "" {
			native["description"] = description
		}
		if schema, ok := function["parameters"].(map[string]any); ok && len(schema) > 0 {
			native["input_schema"] = schema
		}
		nativeTools = append(nativeTools, native)
	}
	styles := []any{map[string]any{
		"type": "default", "key": "Default", "name": "Normal",
		"nameKey": "normal_style_name", "prompt": "Normal\n",
		"summary": "Default responses from Claude", "summaryKey": "normal_style_summary", "isDefault": true,
	}}
	return map[string]any{
		"prompt": prompt,
		"model":  model,
		// checkpoint: IANA fixo; upgrade quando o runtime expõe o tz do cliente.
		"timezone":            claudeWebTimezone,
		"personalized_styles": styles,
		"locale":              claudeWebLocale,
		"tools":               nativeTools,
		"turn_message_uuids":  map[string]any{"human_message_uuid": uuid.NewString(), "assistant_message_uuid": uuid.NewString()},
		"attachments":         []any{},
		"effort":              "low",
		"files":               []any{},
		"sync_sources":        []any{},
		"rendering_mode":      "messages",
		"thinking_mode":       "off",
		"create_conversation_params": map[string]any{
			"name": "", "model": model, "include_conversation_preferences": true,
			"paprika_mode": nil, "compass_mode": nil, "is_temporary": false,
			"enabled_imagine": true, "tool_search_mode": "auto",
		},
	}
}

// claudeWebScan percorre o SSE Anthropic e emite deltas de texto/thinking.
func claudeWebScan(reader *bufio.Reader, onDelta func(text, think string)) (bool, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
	finished := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var event struct {
			Type  string `json:"type"`
			Delta struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				Thinking string `json:"thinking"`
			} `json:"delta"`
			Error map[string]any `json:"error"`
		}
		if json.Unmarshal([]byte(payload), &event) != nil {
			continue
		}
		switch event.Type {
		case "content_block_delta":
			if event.Delta.Type == "text_delta" && event.Delta.Text != "" && onDelta != nil {
				onDelta(event.Delta.Text, "")
			}
			if event.Delta.Type == "thinking_delta" && event.Delta.Thinking != "" && onDelta != nil {
				onDelta("", event.Delta.Thinking)
			}
		case "message_stop":
			finished = true
		case "error":
			return finished, fmt.Errorf("claude.ai stream error: %s", payload)
		}
	}
	return finished, scanner.Err()
}

// claudeWebAccumulate junta o stream SSE Anthropic em texto/thinking puros.
func claudeWebAccumulate(reader *bufio.Reader) (string, string, error) {
	var text, think strings.Builder
	_, err := claudeWebScan(reader, func(deltaText, deltaThink string) {
		text.WriteString(deltaText)
		think.WriteString(deltaThink)
	})
	return text.String(), think.String(), err
}
