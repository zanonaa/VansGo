package handlers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Z.ai Web (chat.z.ai) fala SSE em /api/v2/chat/completions com um JWT de
// sessão guest (sem exp) colado como token. Cada evento é
// {"type":"chat:completion","data":{"delta_content","phase"}} — phase
// thinking vira reasoning, answer vira texto, other traz usage e done fecha.
// Ferramentas voltam via shim <tool> (mesmo protocolo kimi/deepseek/grok).
const (
	zaiWebChatURL = "https://chat.z.ai/api/v2/chat/completions"
	zaiWebAuthURL = "https://chat.z.ai/api/v1/auths/"
	zaiWebUA      = "Mozilla/5.0 (X11; Linux x86_64; rv:155.0) Gecko/20100101 Firefox/155.0"
	zaiWebFE      = "prod-fe-1.2.10"
	// ponytail: linha maior que isso é upstream hostil tentando OOM; eventos
	// legítimos do chat:completion ficam abaixo de 1 MiB.
	zaiWebMaxLine = 8 << 20
)

// zaiWebModelMap traduz os ids do catálogo para o model do chat.z.ai; fora do
// mapa cai no x-preview-l (GLM-5.3-Flash, o que o site usa por padrão).
func zaiWebModelMap(model string) string {
	switch model {
	case "x-preview-l", "glm-5.3-flash", "glm-5-flash":
		return "x-preview-l"
	case "x-preview", "glm-5":
		return "x-preview"
	default:
		return "x-preview-l"
	}
}

func (t *TranslatorSend) SendZaiWeb(w http.ResponseWriter, r *http.Request, provider, model string, body map[string]any, source string, stream bool) {
	t.sendZaiWeb(w, r, provider, model, body, source, stream)
}

func (t *TranslatorSend) sendZaiWeb(w http.ResponseWriter, r *http.Request, provider, model string, body map[string]any, source string, stream bool) {
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
	token := ttsString(firstNonEmpty(connection["apiKey"], connection["accessToken"]))
	if token == "" {
		fail(400, "Missing Z.ai token — paste the guest token from chat.z.ai")
		return
	}

	intermediate := body
	if source != "openai" {
		if intermediate, err = translateSource(body, source); err != nil {
			fail(400, err.Error())
			return
		}
	}

	message, _, err := kimiWebFoldMessages(intermediate["messages"])
	if err != nil {
		fail(400, err.Error())
		return
	}
	if message == "" {
		fail(400, "Z.ai Web requires a non-empty user message")
		return
	}

	userID := zaiWebUserID(token)
	chatID := uuid.NewString()
	now := time.Now()
	payload, _ := json.Marshal(map[string]any{
		"stream": true, "model": zaiWebModelMap(model),
		"messages":         []any{map[string]any{"role": "user", "content": message}},
		"signature_prompt": message, "params": map[string]any{}, "extra": map[string]any{},
		"features": map[string]any{
			"image_generation": false, "web_search": false, "preview_mode": true,
			"flags": []any{"general_agent"}, "auto_web_search": false,
			"vlm_tools_enable": false, "vlm_web_search_enable": false, "vlm_website_mode": false,
			"enable_thinking": true, "reasoning_effort": "max",
		},
		"variables": map[string]any{
			"{{USER_NAME}}": "Guest", "{{USER_LOCATION}}": "Unknown",
			"{{CURRENT_DATETIME}}": now.Format("2006-01-02 15:04:05"),
			"{{CURRENT_DATE}}":     now.Format("2006-01-02"),
			"{{CURRENT_TIME}}":     now.Format("15:04:05"),
			"{{CURRENT_WEEKDAY}}":  now.Weekday().String(),
			"{{CURRENT_TIMEZONE}}": "America/Sao_Paulo",
			"{{USER_LANGUAGE}}":    "en-US",
		},
		"chat_id": chatID, "id": uuid.NewString(),
		"current_user_message_id":        uuid.NewString(),
		"current_user_message_parent_id": nil,
		"background_tasks":               map[string]any{"title_generation": false, "tags_generation": false},
		// shortcut: captcha_verify_param vazio — o chat.z.ai aceita o guest
		// sem captcha; se começar a exigir, capturar do frontend do chat.z.ai.
		"captcha_verify_param": "",
	})

	metadata, _ := connection["providerSpecificData"].(map[string]any)
	client, relay, cleanup, err := connectionHTTP(r.Context(), metadata, t.GetPool, t.Client)
	if err != nil {
		fail(500, err.Error())
		return
	}
	defer cleanup()

	do := func(token, userID string) (*http.Response, error) {
		request, err := http.NewRequestWithContext(r.Context(), "POST", zaiWebURL(token, userID), bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		zaiWebBrowserHeaders(request, token)
		if err := rewriteConnectionRelay(request, relay); err != nil {
			return nil, err
		}
		return client.Do(request)
	}
	response, err := do(token, userID)
	if err != nil {
		fail(502, "Z.ai connection failed: "+err.Error())
		return
	}
	// O guest é anônimo: um 401 é token velho, não limite. Um GET em
	// /api/v1/auths/ emite outro token, que fica salvo na connection.
	if response.StatusCode == http.StatusUnauthorized {
		io.Copy(io.Discard, io.LimitReader(response.Body, 8<<10))
		response.Body.Close()
		fresh, refreshErr := zaiWebGuestToken(r.Context(), client, relay)
		if refreshErr != nil {
			fail(502, "Z.ai guest refresh failed: "+refreshErr.Error())
			return
		}
		if _, refreshErr = t.Connections.Update(r.Context(), ttsString(connection["id"]), map[string]any{"apiKey": fresh}); refreshErr != nil {
			fail(500, "persist refreshed Z.ai token: "+refreshErr.Error())
			return
		}
		token = fresh
		response, err = do(token, zaiWebUserID(token))
		if err != nil {
			fail(502, "Z.ai connection failed: "+err.Error())
			return
		}
	}
	defer response.Body.Close()
	if response.StatusCode == 401 || response.StatusCode == 403 {
		details, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
		fail(response.StatusCode, fmt.Sprintf("Z.ai auth failed — guest token may be expired (%d). Re-paste your token from chat.z.ai. %s", response.StatusCode, strings.TrimSpace(string(details))))
		return
	}
	if response.StatusCode == 429 {
		fail(429, "Z.ai rate limited. Wait a moment and retry, or rotate tokens.")
		return
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		details, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
		fail(response.StatusCode, fmt.Sprintf("Z.ai error: %d — %s", response.StatusCode, strings.TrimSpace(string(details))))
		return
	}

	// SSE sempre em stream; flua direto.
	reader := bufio.NewReaderSize(response.Body, 64<<10)
	if stream {
		t.zaiWebStreamRelay(w, reader, model)
		return
	}
	text, think, err := zaiWebAccumulate(reader)
	if err != nil {
		fail(502, "Z.ai stream error: "+err.Error())
		return
	}

	content := strings.TrimSpace(text)
	if usage, ok := r.Context().Value(usageContextKey{}).(*requestUsage); ok && usage != nil {
		usage.addText(content)
	}

	if !stream {
		message := map[string]any{"role": "assistant", "content": content}
		if think != "" {
			message["reasoning_content"] = think
		}
		nodeReply(w, 200, map[string]any{
			"id":      fmt.Sprintf("chatcmpl-zai-%s", uuid.NewString()[:12]),
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": "stop"}},
		})
		return
	}
	writeOpenAIDeltas(w, model, think, content, nil, "stop")
}

// zaiWebStreamRelay repassa o SSE do chat.z.ai como SSE OpenAI em tempo real;
// phase thinking sai como reasoning_content e answer como content.
func (t *TranslatorSend) zaiWebStreamRelay(w http.ResponseWriter, reader *bufio.Reader, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)
	id := fmt.Sprintf("chatcmpl-zai-%s", uuid.NewString()[:12])
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
	err := zaiWebScan(reader, func(phase, chunk string) {
		if chunk == "" {
			return
		}
		if !emitted {
			emitted = true
			writeChunk(map[string]any{"role": "assistant", "content": ""}, nil)
		}
		if phase == "thinking" {
			writeChunk(map[string]any{"reasoning_content": chunk}, nil)
			return
		}
		writeChunk(map[string]any{"content": chunk}, nil)
	})
	if err != nil {
		writeChunk(map[string]any{"content": fmt.Sprintf("[Error: %v]", err)}, nil)
	}
	writeChunk(map[string]any{}, "stop")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// zaiWebAccumulate consome o SSE e devolve (texto, raciocínio).
func zaiWebAccumulate(reader *bufio.Reader) (string, string, error) {
	text, think := strings.Builder{}, strings.Builder{}
	err := zaiWebScan(reader, func(phase, chunk string) {
		if phase == "thinking" {
			think.WriteString(chunk)
			return
		}
		text.WriteString(chunk)
	})
	if err != nil {
		return "", "", err
	}
	return text.String(), think.String(), nil
}

// zaiWebScan percorre os eventos SSE do chat:completion e chama onDelta com
// (phase, delta_content). Erro embutido no evento encerra o scan.
func zaiWebScan(reader *bufio.Reader, onDelta func(phase, chunk string)) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64<<10), zaiWebMaxLine)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var event struct {
			Type string `json:"type"`
			Data struct {
				DeltaContent string `json:"delta_content"`
				Phase        string `json:"phase"`
				Error        *struct {
					Detail string `json:"detail"`
					Code   string `json:"code"`
				} `json:"error"`
			} `json:"data"`
		}
		if json.Unmarshal([]byte(payload), &event) != nil || event.Type != "chat:completion" {
			continue
		}
		if event.Data.Error != nil && event.Data.Error.Code != "" {
			detail := event.Data.Error.Detail
			if detail == "" {
				detail = event.Data.Error.Code
			}
			return fmt.Errorf("Z.ai error: %s", detail)
		}
		onDelta(event.Data.Phase, event.Data.DeltaContent)
	}
	return scanner.Err()
}

// zaiWebGuestToken pede um JWT guest novo. O endpoint é anônimo: cada GET
// cria uma sessão, então serve tanto para renovar um token rejeitado quanto
// para semear uma connection sem token.
func zaiWebGuestToken(ctx context.Context, client *http.Client, relay string) (string, error) {
	requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, "GET", zaiWebAuthURL, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", zaiWebUA)
	request.Header.Set("x-fe-version", zaiWebFE)
	if err := rewriteConnectionRelay(request, relay); err != nil {
		return "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("guest auth returned %d", response.StatusCode)
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&body); err != nil {
		return "", err
	}
	if strings.TrimSpace(body.Token) == "" {
		return "", fmt.Errorf("guest auth returned no token")
	}
	return body.Token, nil
}

// zaiWebURL monta a query de fingerprint do chat.z.ai (timestamp, requestId,
// user_id e o próprio token).
func zaiWebURL(token, userID string) string {
	query := url.Values{}
	query.Set("timestamp", fmt.Sprintf("%d", time.Now().UnixMilli()))
	query.Set("requestId", uuid.NewString())
	query.Set("user_id", userID)
	query.Set("version", "0.0.1")
	query.Set("platform", "web")
	query.Set("token", token)
	query.Set("language", "en-US")
	query.Set("timezone", "America/Sao_Paulo")
	return zaiWebChatURL + "?" + query.Encode()
}

// zaiWebUserID extrai o claim id do JWT guest; fora de formato cai num uuid
// estável derivado do próprio token.
func zaiWebUserID(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return uuid.NewSHA1(uuid.NameSpaceURL, []byte(token)).String()
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return uuid.NewSHA1(uuid.NameSpaceURL, []byte(token)).String()
	}
	var claims struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.ID == "" {
		return uuid.NewSHA1(uuid.NameSpaceURL, []byte(token)).String()
	}
	return claims.ID
}

// zaiWebBrowserHeaders replica o fingerprint Firefox do chat.z.ai. O
// x-signature fica de fora: o servidor guest não o valida (o captcha é que
// barra replay). Accept-Encoding fica por conta do transporte Go.
func zaiWebBrowserHeaders(request *http.Request, token string) {
	request.Header.Set("Accept", "*/*")
	request.Header.Set("Accept-Language", "pt-BR,pt;q=0.9,en-US;q=0.8,en;q=0.7")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://chat.z.ai")
	request.Header.Set("Referer", "https://chat.z.ai/")
	request.Header.Set("User-Agent", zaiWebUA)
	request.Header.Set("x-fe-version", zaiWebFE)
	request.Header.Set("x-region", "overseas")
	request.Header.Set("Cookie", "token="+token)
}
