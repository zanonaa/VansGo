package handlers

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Perplexity Web (www.perplexity.ai) fala SSE em /rest/sse/perplexity_ask com
// o cookie __Secure-next-auth.session-token. O corpo carrega um "query" JSON
// (instruções + histórico + pergunta) e o texto chega em markdown_block.chunks.
// A API web não tem tools nem system: o histórico dobra num query JSON e as
// tools voltam como texto (shim de referência, igual à origem hikers-router).
const (
	pplxWebAskURL  = "https://www.perplexity.ai/rest/sse/perplexity_ask"
	pplxWebVersion = "2.18"
	pplxWebUA      = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"
	pplxWebMaxLine = 4 << 20
)

// SendPerplexityWeb expõe o executor do perplexity.ai ao gateway e ao node.
func (t *TranslatorSend) SendPerplexityWeb(w http.ResponseWriter, r *http.Request, provider, model string, body map[string]any, source string, stream bool) {
	t.sendPerplexityWeb(w, r, provider, model, body, source, stream)
}

func (t *TranslatorSend) sendPerplexityWeb(w http.ResponseWriter, r *http.Request, provider, model string, body map[string]any, source string, stream bool) {
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
	cookie := ttsString(firstNonEmpty(connection["apiKey"], connection["accessToken"]))
	if cookie == "" {
		fail(400, "Missing Perplexity session cookie — paste __Secure-next-auth.session-token from www.perplexity.ai")
		return
	}

	intermediate := body
	if source != "openai" {
		if intermediate, err = translateSource(body, source); err != nil {
			fail(400, err.Error())
			return
		}
	}

	prompt, systemPrompt, err := kimiWebFoldMessages(intermediate["messages"])
	if err != nil {
		fail(400, err.Error())
		return
	}
	if prompt == "" {
		fail(400, "Perplexity Web requires a non-empty user message")
		return
	}

	metadata, _ := connection["providerSpecificData"].(map[string]any)
	client, relay, cleanup, err := connectionHTTP(r.Context(), metadata, t.GetPool, t.Client)
	if err != nil {
		fail(500, err.Error())
		return
	}
	defer cleanup()

	query := pplxWebQuery(systemPrompt, prompt)
	focus, sources := "internet", []any{"web"}
	payload, _ := json.Marshal(map[string]any{
		"query_str": query,
		"params": map[string]any{
			"query_str":             query,
			"search_focus":          focus,
			"mode":                  "concise",
			"model_preference":      pplxWebModelPreference(model),
			"sources":               sources,
			"attachments":           []any{},
			"frontend_uuid":         newKimiWebNonce(),
			"frontend_context_uuid": newKimiWebNonce(),
			"version":               pplxWebVersion,
			"language":              "en-US",
			"timezone":              "UTC",
			"search_recency_filter": nil,
			"is_incognito":          true,
			"use_schematized_api":   true,
			"last_backend_uuid":     nil,
		},
	})
	request, err := http.NewRequestWithContext(r.Context(), "POST", pplxWebAskURL, bytes.NewReader(payload))
	if err != nil {
		fail(500, err.Error())
		return
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Origin", "https://www.perplexity.ai")
	request.Header.Set("Referer", "https://www.perplexity.ai/")
	request.Header.Set("User-Agent", pplxWebUA)
	request.Header.Set("X-App-ApiClient", "default")
	request.Header.Set("X-App-ApiVersion", pplxWebVersion)
	request.Header.Set("Cookie", cookiePair(cookie, "__Secure-next-auth.session-token"))
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
	if response.StatusCode == 401 || response.StatusCode == 403 {
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
		if response.StatusCode == 403 && claudeWebIsChallenge(403, "", string(raw)) {
			fail(403, "Cloudflare challenge on perplexity.ai — retry from a browser IP or paste the cookie with cf_clearance")
			return
		}
		fail(response.StatusCode, "Perplexity auth failed — re-paste __Secure-next-auth.session-token from www.perplexity.ai")
		return
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		details, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
		fail(response.StatusCode, fmt.Sprintf("Perplexity error: %d — %s", response.StatusCode, strings.TrimSpace(string(details))))
		return
	}

	reader := bufio.NewReaderSize(response.Body, 64<<10)
	if stream {
		t.pplxWebStreamRelay(w, reader, model)
		return
	}
	text, err := pplxWebAccumulate(reader)
	if err != nil {
		fail(502, "Perplexity stream error: "+err.Error())
		return
	}

	content := pplxWebClean(text)
	if usage, ok := r.Context().Value(usageContextKey{}).(*requestUsage); ok && usage != nil {
		usage.addText(content)
	}

	if !stream {
		message := map[string]any{"role": "assistant", "content": content}
		nodeReply(w, 200, map[string]any{
			"id":      fmt.Sprintf("chatcmpl-pplx-%d", time.Now().UnixNano()),
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": "stop"}},
		})
		return
	}
	writeOpenAIDeltas(w, model, "", content, nil, "stop")
}

// pplxWebStreamRelay repassa o SSE do Perplexity como SSE OpenAI em tempo real.
func (t *TranslatorSend) pplxWebStreamRelay(w http.ResponseWriter, reader *bufio.Reader, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)
	id := fmt.Sprintf("chatcmpl-pplx-%d", time.Now().UnixNano())
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
	_, _ = pplxWebScan(reader, func(delta string) {
		if !emitted {
			emitted = true
			writeChunk(map[string]any{"role": "assistant", "content": ""}, nil)
		}
		if delta != "" {
			writeChunk(map[string]any{"content": pplxWebClean(delta)}, nil)
		}
	})
	writeChunk(map[string]any{}, "stop")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// pplxWebQuery monta o campo query: instruções de system + tools hint +
// instrução de busca + pergunta (mesma política da origem).
func pplxWebQuery(systemPrompt, prompt string) string {
	instructions := []string{}
	if systemPrompt != "" {
		instructions = append(instructions, systemPrompt)
	}
	instructions = append(instructions, "You have built-in web search. Answer questions directly using search results.")
	obj := map[string]any{"instructions": instructions, "query": prompt}
	encoded, _ := json.Marshal(obj)
	if len(encoded) > 96000 {
		encoded = encoded[len(encoded)-96000:]
	}
	return string(encoded)
}

// pplxWebModelPreference mapeia os ids do catálogo para o preference do
// perplexity_ask; ids fora do mapa vão verbatim (o backend aceita prefs novas).
func pplxWebModelPreference(model string) string {
	switch model {
	case "pplx-auto":
		return "pplx_pro"
	case "pplx-sonar":
		return "experimental"
	case "pplx-gpt":
		return "gpt54"
	case "pplx-gemini":
		return "gemini31pro_high"
	case "pplx-sonnet":
		return "claude46sonnet"
	case "pplx-opus":
		return "claude46opus"
	case "pplx-nemotron":
		return "nv_nemotron_3_super"
	}
	return model
}

// pplxWebAccumulate consome o SSE e devolve o texto completo da resposta.
func pplxWebAccumulate(reader *bufio.Reader) (string, error) {
	var text strings.Builder
	if _, err := pplxWebScan(reader, func(delta string) {
		text.WriteString(delta)
	}); err != nil {
		return "", err
	}
	if text.Len() == 0 {
		return "", fmt.Errorf("Perplexity returned no answer text")
	}
	return text.String(), nil
}

// pplxWebScan percorre os eventos SSE do perplexity_ask e chama onDelta a cada
// fragmento novo de resposta. A API atual marca o texto como "ask_text" (e não
// mais "markdown_block"), entrega chunks fragmentados nos eventos e o snapshot
// completo em answer/chunks no evento DONE; versões antigas mandavam chunks
// cumulativos. aceita() trata as duas formas sem duplicar texto.
func pplxWebScan(reader *bufio.Reader, onDelta func(delta string)) (bool, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64<<10), pplxWebMaxLine)
	finished := false
	emitted := strings.Builder{}
	accept := func(chunk string) {
		if chunk == "" {
			return
		}
		current := emitted.String()
		switch {
		case chunk == current, len(current) > len(chunk) && strings.HasPrefix(current, chunk):
			// Duplicata ou snapshot antigo.
		case len(chunk) > len(current) && strings.HasPrefix(chunk, current):
			// Snapshot cumulativo: só o resto é novo.
			onDelta(chunk[len(current):])
			emitted.Reset()
			emitted.WriteString(chunk)
		default:
			onDelta(chunk)
			emitted.Reset()
			emitted.WriteString(current + chunk)
		}
	}
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if raw == "" || raw == "[DONE]" {
			continue
		}
		var event struct {
			Blocks []struct {
				Usage string `json:"intended_usage"`
				Mark  struct {
					Chunks   []string `json:"chunks"`
					Progress string   `json:"progress"`
					Answer   string   `json:"answer"`
				} `json:"markdown_block"`
			} `json:"blocks"`
			Text      string `json:"text"`
			Final     bool   `json:"final"`
			Status    string `json:"status"`
			ErrorCode int    `json:"error_code"`
			ErrorMsg  string `json:"error_message"`
		}
		if json.Unmarshal([]byte(raw), &event) != nil {
			continue
		}
		if event.ErrorCode != 0 || event.ErrorMsg != "" {
			return finished, fmt.Errorf("Perplexity error %d: %s", event.ErrorCode, event.ErrorMsg)
		}
		for _, block := range event.Blocks {
			if !strings.Contains(block.Usage, "text") && !strings.Contains(block.Usage, "markdown") {
				continue
			}
			if block.Mark.Progress == "DONE" {
				full := block.Mark.Answer
				if full == "" {
					full = strings.Join(block.Mark.Chunks, "")
				}
				accept(full)
				finished = true
				continue
			}
			accept(strings.Join(block.Mark.Chunks, ""))
		}
		if len(event.Blocks) == 0 && event.Text != "" {
			accept(strings.TrimSpace(event.Text))
		}
		if event.Final || event.Status == "COMPLETED" {
			finished = true
		}
	}
	return finished, scanner.Err()
}

// pplxWebClean remove citações [n], tags grok/xml e wrapper <response> —
// mesma limpeza da origem hikers-router.
func pplxWebClean(text string) string {
	text = pplxWebRegexpCitations.ReplaceAllString(text, "")
	text = pplxWebRegexpGrokTags.ReplaceAllString(text, "")
	text = pplxWebRegexpGrokSelf.ReplaceAllString(text, "")
	text = pplxWebRegexpXMLDecl.ReplaceAllString(text, "")
	text = pplxWebRegexpResponseTag.ReplaceAllString(text, "")
	return strings.TrimSpace(text)
}

// Regexes do pplxWebClean (citações, tags grok, declaração XML, wrapper).
var (
	pplxWebRegexpCitations   = regexp.MustCompile(`\[\d+\]`)
	pplxWebRegexpGrokTags    = regexp.MustCompile(`(?s)<grok:[^>]*>.*?</grok:[^>]*>`)
	pplxWebRegexpGrokSelf    = regexp.MustCompile(`<grok:[^>]*/>`)
	pplxWebRegexpXMLDecl     = regexp.MustCompile(`<\?xml[^?]*\?>`)
	pplxWebRegexpResponseTag = regexp.MustCompile(`</?response\b[^>]*>`)
)
