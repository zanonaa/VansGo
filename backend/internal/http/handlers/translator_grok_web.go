package handlers

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Grok Web (grok.com) fala NDJSON em /rest/app-chat/conversations/new com o
// cookie sso. Um turno único carrega todo o histórico ("role: texto", última
// user nua) e a resposta chega em result.response.token / modelResponse.
// Ferramentas voltam via shim <tool> (mesmo protocolo kimi/deepseek).
const (
	grokWebChatURL = "https://grok.com/rest/app-chat/conversations/new"
	grokWebUA      = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/136.0.0.0 Safari/537.36"
	// ponytail: linha maior que isso é upstream hostil tentando OOM; eventos
	// legítimos do conversations/new ficam abaixo de 1 MiB.
	grokWebMaxLine = 8 << 20
)

// grokWebModelMap traduz os ids do catálogo para (modelName, modelMode,
// isReasoning) do app-chat; fora do mapa cai no fast (paridade com a origem).
func grokWebModelMap(model string) (string, string, bool) {
	switch model {
	case "grok-3":
		return "grok-3", "MODEL_MODE_GROK_3", false
	case "grok-3-mini":
		return "grok-3", "MODEL_MODE_GROK_3_MINI_THINKING", true
	case "grok-3-thinking":
		return "grok-3", "MODEL_MODE_GROK_3_THINKING", true
	case "grok-4":
		return "grok-4", "MODEL_MODE_GROK_4", false
	case "grok-4-mini":
		return "grok-4-mini", "MODEL_MODE_GROK_4_MINI_THINKING", true
	case "grok-4-thinking":
		return "grok-4", "MODEL_MODE_GROK_4_THINKING", true
	case "grok-4-heavy":
		return "grok-4", "MODEL_MODE_HEAVY", true
	case "grok-4.1-mini":
		return "grok-4-1-thinking-1129", "MODEL_MODE_GROK_4_1_MINI_THINKING", true
	case "grok-4.1-fast":
		return "grok-4-1-thinking-1129", "MODEL_MODE_FAST", false
	case "grok-4.1-expert":
		return "grok-4-1-thinking-1129", "MODEL_MODE_EXPERT", true
	case "grok-4.1-thinking":
		return "grok-4-1-thinking-1129", "MODEL_MODE_GROK_4_1_THINKING", true
	case "grok-4.2", "grok-4.20", "grok-4.20-beta":
		return "grok-420", "MODEL_MODE_GROK_420", false
	}
	return "grok-4-1-thinking-1129", "MODEL_MODE_FAST", false
}

// SendGrokWeb expõe o executor do grok.com ao gateway e ao node.
func (t *TranslatorSend) SendGrokWeb(w http.ResponseWriter, r *http.Request, provider, model string, body map[string]any, source string, stream bool) {
	t.sendGrokWeb(w, r, provider, model, body, source, stream)
}

func (t *TranslatorSend) sendGrokWeb(w http.ResponseWriter, r *http.Request, provider, model string, body map[string]any, source string, stream bool) {
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
	sso := ttsString(firstNonEmpty(connection["apiKey"], connection["accessToken"]))
	if sso == "" {
		fail(400, "Missing Grok SSO cookie — paste the sso cookie value from grok.com")
		return
	}

	intermediate := body
	if source != "openai" {
		if intermediate, err = translateSource(body, source); err != nil {
			fail(400, err.Error())
			return
		}
	}

	// Tool shim: o app-chat não registra tools, então o contrato vai no prompt
	// e as chamadas voltam como texto <tool>{...}</tool> (nonce anti copy-attack).
	requestedTools, _ := intermediate["tools"].([]any)
	var toolNonce string
	var toolContract string
	if len(requestedTools) > 0 {
		toolNonce = newKimiWebNonce()
		toolContract = kimiWebToolContract(toolNonce, kimiWebToolLines(requestedTools))
	}
	message, _, err := kimiWebFoldMessages(intermediate["messages"], toolContract)
	if err != nil {
		fail(400, err.Error())
		return
	}
	if message == "" {
		fail(400, "Grok Web requires a non-empty user message")
		return
	}

	grokModel, modelMode, _ := grokWebModelMap(model)
	payload, _ := json.Marshal(map[string]any{
		"temporary": true, "modelName": grokModel, "modelMode": modelMode, "message": message,
		"fileAttachments": []any{}, "imageAttachments": []any{},
		"disableSearch": false, "enableImageGeneration": false, "returnImageBytes": false,
		"returnRawGrokInXaiRequest": false, "enableImageStreaming": false, "imageGenerationCount": 0,
		"forceConcise": false, "toolOverrides": map[string]any{}, "enableSideBySide": true, "sendFinalMetadata": true,
		"isReasoning": false, "disableTextFollowUps": false, "disableMemory": true,
		"forceSideBySide": false, "isAsyncChat": false, "disableSelfHarmShortCircuit": false,
		"deviceEnvInfo": map[string]any{
			"darkModeEnabled": false, "devicePixelRatio": 2,
			"screenWidth": 2056, "screenHeight": 1329, "viewportWidth": 2056, "viewportHeight": 1083,
		},
	})

	metadata, _ := connection["providerSpecificData"].(map[string]any)
	client, relay, cleanup, err := connectionHTTP(r.Context(), metadata, t.GetPool, t.Client)
	if err != nil {
		fail(500, err.Error())
		return
	}
	defer cleanup()

	do := func() (*http.Response, error) {
		request, err := http.NewRequestWithContext(r.Context(), "POST", grokWebChatURL, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		grokWebBrowserHeaders(request, cookiePair(sso, "sso"))
		if err := rewriteConnectionRelay(request, relay); err != nil {
			return nil, err
		}
		return client.Do(request)
	}
	response, err := do()
	if err != nil {
		fail(502, "Grok connection failed: "+err.Error())
		return
	}
	defer response.Body.Close()
	if response.StatusCode == 401 || response.StatusCode == 403 {
		details, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
		fail(response.StatusCode, fmt.Sprintf("Grok auth failed — SSO cookie may be expired (%d). Re-paste your sso cookie value from grok.com. %s", response.StatusCode, strings.TrimSpace(string(details))))
		return
	}
	if response.StatusCode == 429 {
		fail(429, "Grok rate limited. Wait a moment and retry, or rotate cookies.")
		return
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		details, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
		fail(response.StatusCode, fmt.Sprintf("Grok error: %d — %s", response.StatusCode, strings.TrimSpace(string(details))))
		return
	}

	// NDJSON sempre em stream; com tools o texto é buffered para o shim
	// extrair os tool calls antes da emissão.
	reader := bufio.NewReaderSize(response.Body, 64<<10)
	if stream && toolNonce == "" {
		t.grokWebStreamRelay(w, reader, model)
		return
	}
	text, err := grokWebAccumulate(reader)
	if err != nil {
		fail(502, "Grok stream error: "+err.Error())
		return
	}

	content := strings.TrimSpace(text)
	var toolCalls []any
	finish := "stop"
	if toolNonce != "" {
		var stripped string
		stripped, toolCalls = parseKimiWebToolCalls(text, toolNonce)
		content = strings.TrimSpace(stripped)
		if len(toolCalls) > 0 {
			finish = "tool_calls"
		}
	}
	if usage, ok := r.Context().Value(usageContextKey{}).(*requestUsage); ok && usage != nil {
		usage.addText(content)
		if len(toolCalls) > 0 {
			usage.addToolCalls(toolCalls)
		}
	}

	if !stream {
		message := map[string]any{"role": "assistant", "content": content}
		if len(toolCalls) > 0 {
			message["tool_calls"] = openAIToolCalls(toolCalls)
		}
		nodeReply(w, 200, map[string]any{
			"id":      fmt.Sprintf("chatcmpl-grok-%s", uuid.NewString()[:12]),
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
		})
		return
	}
	writeOpenAIDeltas(w, model, "", content, toolCalls, finish)
}

// grokWebStreamRelay repassa o NDJSON do grok.com como SSE OpenAI em tempo real.
func (t *TranslatorSend) grokWebStreamRelay(w http.ResponseWriter, reader *bufio.Reader, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)
	id := fmt.Sprintf("chatcmpl-grok-%s", uuid.NewString()[:12])
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
	err := grokWebScan(reader, func(kind, chunk string) {
		if kind != "text" {
			return
		}
		if !emitted {
			emitted = true
			writeChunk(map[string]any{"role": "assistant", "content": ""}, nil)
		}
		writeChunk(map[string]any{"content": chunk}, nil)
	})
	if err != nil {
		writeChunk(map[string]any{"content": fmt.Sprintf("[Error: %v]", err)}, nil)
	}
	writeChunk(map[string]any{}, "stop")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// grokWebAccumulate consome o NDJSON e devolve o texto completo da resposta
// (o snapshot final de modelResponse.message substitui os tokens acumulados).
func grokWebAccumulate(reader *bufio.Reader) (string, error) {
	var text strings.Builder
	sawContent := false
	err := grokWebScan(reader, func(kind, chunk string) {
		if kind == "final" {
			text.Reset()
			text.WriteString(chunk)
			sawContent = true
			return
		}
		if kind == "text" {
			text.WriteString(chunk)
			sawContent = true
		}
	})
	if err != nil {
		return "", err
	}
	if !sawContent {
		return "", fmt.Errorf("stream ended without content")
	}
	return text.String(), nil
}

// grokWebScan percorre as linhas NDJSON do conversations/new e chama onEvent
// com ("text", token) para deltas e ("final", message) para o snapshot.
func grokWebScan(reader *bufio.Reader, onEvent func(kind, chunk string)) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64<<10), grokWebMaxLine)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event struct {
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
			Result *struct {
				Response *struct {
					Token         *string `json:"token"`
					ModelResponse *struct {
						Message string `json:"message"`
					} `json:"modelResponse"`
				} `json:"response"`
			} `json:"result"`
		}
		if json.Unmarshal([]byte(line), &event) != nil {
			continue
		}
		if event.Error != nil && event.Error.Message != "" {
			return fmt.Errorf("Grok error: %s", event.Error.Message)
		}
		if event.Result == nil || event.Result.Response == nil {
			continue
		}
		response := event.Result.Response
		if response.ModelResponse != nil && response.ModelResponse.Message != "" {
			onEvent("final", response.ModelResponse.Message)
			continue
		}
		if response.Token != nil && *response.Token != "" {
			onEvent("text", *response.Token)
		}
	}
	return scanner.Err()
}

// grokWebBrowserHeaders replica o fingerprint Chrome do executor da origem
// (x-statsig-id sintético, request id, traceparent). O Accept-Encoding fica
// por conta do transporte Go, que descomprime gzip de forma transparente.
func grokWebBrowserHeaders(request *http.Request, cookie string) {
	trace := make([]byte, 16)
	span := make([]byte, 8)
	_, _ = rand.Read(trace)
	_, _ = rand.Read(span)
	request.Header.Set("Accept", "*/*")
	request.Header.Set("Accept-Language", "en-US,en;q=0.9")
	request.Header.Set("Baggage", "sentry-environment=production,sentry-release=d6add6fb0460641fd482d767a335ef72b9b6abb8,sentry-public_key=b311e0f2690c81f25e2c4cf6d4f7ce1c")
	request.Header.Set("Cache-Control", "no-cache")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://grok.com")
	request.Header.Set("Pragma", "no-cache")
	request.Header.Set("Referer", "https://grok.com/")
	request.Header.Set("Sec-Ch-Ua", `"Google Chrome";v="136", "Chromium";v="136", "Not(A:Brand";v="24"`)
	request.Header.Set("Sec-Ch-Ua-Mobile", "?0")
	request.Header.Set("Sec-Ch-Ua-Platform", `"macOS"`)
	request.Header.Set("Sec-Fetch-Dest", "empty")
	request.Header.Set("Sec-Fetch-Mode", "cors")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	request.Header.Set("User-Agent", grokWebUA)
	request.Header.Set("x-statsig-id", grokWebStatsigId())
	request.Header.Set("x-xai-request-id", uuid.NewString())
	request.Header.Set("traceparent", fmt.Sprintf("00-%s-%s-00", hex.EncodeToString(trace), hex.EncodeToString(span)))
	request.Header.Set("Cookie", cookie)
}

// grokWebStatsigId gera o header x-statsig-id: base64 de uma mensagem falsa
// de TypeError, mesma heurística do executor da origem.
func grokWebStatsigId() string {
	coin := make([]byte, 1)
	_, _ = rand.Read(coin)
	var msg string
	if coin[0]&1 == 0 {
		msg = fmt.Sprintf(`e:TypeError: Cannot read properties of null (reading 'children["%s"]')`, grokWebRandom(5, "abcdefghijklmnopqrstuvwxyz0123456789"))
	} else {
		msg = fmt.Sprintf("e:TypeError: Cannot read properties of undefined (reading '%s')", grokWebRandom(10, "abcdefghijklmnopqrstuvwxyz"))
	}
	return base64.StdEncoding.EncodeToString([]byte(msg))
}

// grokWebRandom devolve n caracteres sorteados de chars via crypto/rand.
func grokWebRandom(n int, chars string) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	out := make([]byte, n)
	for i := range out {
		out[i] = chars[int(buf[i])%len(chars)]
	}
	return string(out)
}
