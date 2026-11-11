package handlers

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Kimi Web (www.kimi.ai, chat consumer) fala Connect-RPC: corpo e resposta em
// frames de 5 bytes (1 flag + 4 de comprimento big-endian) envolvendo JSON.
// Sem o framing o upstream devolve invalid_argument para todo request.
const (
	kimiWebBaseURL  = "https://www.kimi.ai"
	kimiWebAuthURL  = "https://auth.kimi.ai"
	kimiWebChatURL  = kimiWebBaseURL + "/apiv2/kimi.gateway.chat.v1.ChatService/Chat"
	kimiWebUA       = "Mozilla/5.0 (X11; Linux x86_64; rv:155.0) Gecko/20100101 Firefox/155.0"
	kimiWebScenario = "SCENARIO_CHAT"
	kimiWebModel    = "k28-instant"
	// ponytail: frame maior que isso é upstream hostil tentando OOM por
	// header mentiroso; eventos legítimos têm menos de 1 KiB.
	kimiWebMaxFrame = 8 << 20
)

// sendKimiWeb executa o chat do www.kimi.ai e dobra o stream Connect de volta
// para o formato do cliente (SSE/JSON OpenAI; Claude/Responses via conversão).
func (t *TranslatorSend) sendKimiWeb(w http.ResponseWriter, r *http.Request, provider, model string, body map[string]any, source string, stream bool) {
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
		fail(400, "Missing Kimi access_token — capture access_token from localStorage at www.kimi.ai")
		return
	}

	intermediate := body
	if source != "openai" {
		if intermediate, err = translateSource(body, source); err != nil {
			fail(400, err.Error())
			return
		}
	}

	// Tool shim: a API web rejeita o campo tools preenchido, então o contrato
	// vai no prompt e as chamadas voltam como texto <tool>{...}</tool>.
	requestedTools, _ := intermediate["tools"].([]any)
	var toolNonce string
	var toolContract string
	if len(requestedTools) > 0 {
		toolNonce = newKimiWebNonce()
		toolContract = kimiWebToolContract(toolNonce, kimiWebToolLines(requestedTools))
	}
	prompt, systemPrompt, err := kimiWebFoldMessages(intermediate["messages"], toolContract)
	if err != nil {
		fail(400, err.Error())
		return
	}
	if prompt == "" {
		fail(400, "Kimi Web requires a non-empty user message")
		return
	}

	encoded := kimiWebFrame(kimiWebChatBody(prompt, systemPrompt))
	metadata, _ := connection["providerSpecificData"].(map[string]any)
	client, relay, cleanup, err := connectionHTTP(r.Context(), metadata, t.GetPool, t.Client)
	if err != nil {
		fail(500, err.Error())
		return
	}
	defer cleanup()

	do := func(token string) (*http.Response, error) {
		request, err := http.NewRequestWithContext(r.Context(), "POST", kimiWebChatURL, bytes.NewReader(encoded))
		if err != nil {
			return nil, err
		}
		request.Header.Set("Content-Type", "application/connect+json")
		request.Header.Set("Accept", "*/*")
		request.Header.Set("connect-protocol-version", "1")
		request.Header.Set("Origin", kimiWebBaseURL)
		request.Header.Set("Referer", kimiWebBaseURL+"/")
		request.Header.Set("User-Agent", kimiWebUA)
		request.Header.Set("Authorization", "Bearer "+token)
		if err := rewriteConnectionRelay(request, relay); err != nil {
			return nil, err
		}
		return client.Do(request)
	}

	response, err := do(access)
	if err != nil {
		fail(502, err.Error())
		return
	}
	// Access JWT dura ~15min; em 401/403 renova pelo refresh_token e tenta uma vez.
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		io.Copy(io.Discard, io.LimitReader(response.Body, 8<<10))
		response.Body.Close()
		patch, refreshErr := t.refreshConnection(r.Context(), provider, connection)
		if refreshErr != nil {
			fail(500, refreshErr.Error())
			return
		}
		if patch == nil {
			fail(response.StatusCode, "Kimi access token expired and no refresh token is configured on the connection")
			return
		}
		response, err = do(ttsString(patch["accessToken"]))
		if err != nil {
			fail(502, err.Error())
			return
		}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		details, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
		fail(response.StatusCode, fmt.Sprintf("Kimi error: %d — %s", response.StatusCode, strings.TrimSpace(string(details))))
		return
	}

	// O upstream sempre responde em stream Connect, independente do que o
	// cliente pediu. Com tools no turno, o texto é buffered para o parse do
	// shim extrair os tool calls antes da emissão; sem tools, flua direto.
	reader := bufio.NewReader(response.Body)
	if stream && len(requestedTools) == 0 {
		t.kimiWebStreamRelay(w, r, reader, model)
		return
	}
	text, think, err := kimiWebAccumulate(reader)
	if err != nil {
		fail(502, "Kimi Connect protocol error: "+err.Error())
		return
	}

	content := text
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
		if think != "" {
			message["reasoning_content"] = think
		}
		if len(toolCalls) > 0 {
			message["tool_calls"] = openAIToolCalls(toolCalls)
		}
		nodeReply(w, 200, map[string]any{
			"id":      fmt.Sprintf("chatcmpl-kimi-%d", time.Now().UnixNano()),
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
		})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)
	id := fmt.Sprintf("chatcmpl-kimi-%d", time.Now().UnixNano())
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
	first := true
	if think != "" {
		writeChunk(map[string]any{"role": "assistant", "reasoning_content": think}, nil)
		first = false
	}
	if content != "" || len(toolCalls) == 0 {
		delta := map[string]any{"content": content}
		if first {
			delta["role"] = "assistant"
		}
		writeChunk(delta, nil)
	}
	for i, call := range toolCalls {
		writeChunk(map[string]any{"tool_calls": []any{map[string]any{"index": i, "id": fmt.Sprintf("call_%d", i), "type": "function", "function": call}}}, nil)
	}
	writeChunk(map[string]any{}, finish)
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// kimiWebStreamRelay repassa o stream Connect como SSE OpenAI em tempo real.
func (t *TranslatorSend) kimiWebStreamRelay(w http.ResponseWriter, r *http.Request, reader *bufio.Reader, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)
	id := fmt.Sprintf("chatcmpl-kimi-%d", time.Now().UnixNano())
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
	emittedRole := false
	for {
		flags, payload, err := kimiWebNextFrame(reader)
		if err != nil {
			return
		}
		if flags&0x02 != 0 {
			if !emittedRole {
				writeChunk(map[string]any{"role": "assistant", "content": ""}, nil)
			}
			writeChunk(map[string]any{}, "stop")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		delta, kind, ok := kimiWebDelta(payload)
		if !ok {
			continue
		}
		if !emittedRole {
			emittedRole = true
			writeChunk(map[string]any{"role": "assistant", "content": ""}, nil)
		}
		if kind == "think" {
			writeChunk(map[string]any{"reasoning_content": delta}, nil)
		} else {
			writeChunk(map[string]any{"content": delta}, nil)
		}
	}
}

// kimiWebAccumulate consome o stream Connect e devolve (texto, raciocínio).
func kimiWebAccumulate(reader *bufio.Reader) (string, string, error) {
	text, think := strings.Builder{}, strings.Builder{}
	sawEnd := false
	for {
		flags, payload, err := kimiWebNextFrame(reader)
		if err != nil {
			if err == io.EOF {
				break
			}
			return "", "", err
		}
		if flags&0x02 != 0 {
			sawEnd = true
			var endErr map[string]any
			if json.Unmarshal(payload, &endErr) == nil && endErr["error"] != nil {
				return "", "", fmt.Errorf("Kimi EndStream error: %v", endErr["error"])
			}
			break
		}
		delta, kind, ok := kimiWebDelta(payload)
		if !ok {
			continue
		}
		if kind == "think" {
			think.WriteString(delta)
		} else {
			text.WriteString(delta)
		}
	}
	if !sawEnd {
		return "", "", fmt.Errorf("stream ended without a successful EndStream frame")
	}
	return text.String(), think.String(), nil
}

// kimiWebNextFrame lê um frame Connect. Retorna flags, payload e erro.
func kimiWebNextFrame(reader *bufio.Reader) (byte, []byte, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(reader, header); err != nil {
		return 0, nil, err
	}
	length := binary.BigEndian.Uint32(header[1:5])
	if length > kimiWebMaxFrame {
		return 0, nil, fmt.Errorf("frame length %d exceeds limit", length)
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return 0, nil, err
	}
	return header[0], payload, nil
}

// kimiWebDelta extrai o delta de texto do frame. op "set" em block.text /
// block.think carrega o primeiro chunk; op "append" em *.content os seguintes.
// Heartbeats e metadados (chat.*, message.*, stage) são suprimidos.
func kimiWebDelta(payload []byte) (string, string, bool) {
	var msg struct {
		Op    string `json:"op"`
		Mask  string `json:"mask"`
		Block struct {
			Text struct {
				Content string `json:"content"`
			} `json:"text"`
			Think struct {
				Content string `json:"content"`
			} `json:"think"`
		} `json:"block"`
	}
	if json.Unmarshal(payload, &msg) != nil {
		return "", "", false
	}
	switch {
	case msg.Op == "append" && msg.Mask == "block.text.content":
		return msg.Block.Text.Content, "text", msg.Block.Text.Content != ""
	case msg.Op == "append" && msg.Mask == "block.think.content":
		return msg.Block.Think.Content, "think", msg.Block.Think.Content != ""
	case msg.Op == "set" && msg.Mask == "block.text":
		return msg.Block.Text.Content, "text", msg.Block.Text.Content != ""
	case msg.Op == "set" && msg.Mask == "block.think":
		return msg.Block.Think.Content, "think", msg.Block.Think.Content != ""
	}
	return "", "", false
}

// kimiWebFrame embrulha o JSON no envelope Connect de 5 bytes.
func kimiWebFrame(body map[string]any) []byte {
	encoded, _ := json.Marshal(body)
	framed := make([]byte, 5+len(encoded))
	framed[0] = 0
	binary.BigEndian.PutUint32(framed[1:5], uint32(len(encoded)))
	copy(framed[5:], encoded)
	return framed
}

// kimiWebChatBody monta o ChatRequest conforme o SPA atual (HAR 2026-11):
// scenario SCENARIO_CHAT e options.model k28-instant.
func kimiWebChatBody(prompt, systemPrompt string) map[string]any {
	options := map[string]any{"thinking": true, "enable_plugin": false, "model": kimiWebModel}
	if systemPrompt != "" {
		options["system_prompt"] = systemPrompt
	}
	return map[string]any{
		"chat_id":  "",
		"scenario": kimiWebScenario,
		"tools":    []any{},
		"message": map[string]any{
			"id": "", "parent_id": "", "children_message_ids": []any{},
			"role":     "user",
			"blocks":   []any{map[string]any{"id": "", "message_id": "", "text": map[string]any{"content": prompt}}},
			"scenario": kimiWebScenario,
			"labels":   []any{}, "references": []any{}, "is_goal": false,
		},
		"options":    options,
		"project_id": "",
	}
}

// kimiWebFoldMessages dobra o histórico OpenAI no turno único do Kimi Web:
// system(s) viram system_prompt; a conversa vira "User:"/""Assistant:"; o
// contrato de tools pronto (quando há shim) entra no fim da última user.
func kimiWebFoldMessages(messages any, toolContract string) (string, string, error) {
	list, ok := messages.([]any)
	if !ok {
		if messages == nil {
			return "", "", nil
		}
		return "", "", fmt.Errorf("messages must be an array")
	}
	system := []string{}
	conversation := []string{}
	lastUser := -1
	for _, raw := range list {
		message, ok := raw.(map[string]any)
		if !ok {
			return "", "", fmt.Errorf("Invalid message")
		}
		role := ttsString(message["role"])
		if role == "tool" || role == "function" {
			// Resultado de tool volta como turno user etiquetado (shim não
			// tem canal nativo; o upstream aceita só user/assistant).
			text := kimiWebTextContent(message["content"])
			conversation = append(conversation, fmt.Sprintf("User: [tool result for %s]\n%s", ttsString(message["tool_call_id"]), text))
			lastUser = len(conversation) - 1
			continue
		}
		if _, has := message["tool_calls"]; has && role == "assistant" {
			// Histórico de chamada anterior: serializa como texto p/ o modelo
			// ver o que executou.
			encoded, _ := json.Marshal(message["tool_calls"])
			conversation = append(conversation, fmt.Sprintf("Assistant: invoked tools: %s", string(encoded)))
			continue
		}
		text := kimiWebTextContent(message["content"])
		switch role {
		case "system", "developer":
			if text != "" {
				system = append(system, text)
			}
		case "user":
			conversation = append(conversation, text)
			lastUser = len(conversation) - 1
		case "assistant":
			if text != "" {
				conversation = append(conversation, "Assistant: "+text)
			}
		default:
			return "", "", fmt.Errorf("Kimi Web does not support message role %s", role)
		}
	}
	if toolContract != "" {
		if lastUser >= 0 && lastUser < len(conversation) {
			conversation[lastUser] += toolContract
		} else {
			conversation = append(conversation, strings.TrimSpace(toolContract))
		}
	}
	return strings.TrimSpace(strings.Join(conversation, "\n\n")), strings.Join(system, "\n\n"), nil
}

// kimiWebTextContent achata conteúdo OpenAI (string ou blocos) em texto.
func kimiWebTextContent(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		parts := []string{}
		for _, raw := range typed {
			block, _ := raw.(map[string]any)
			switch block["type"] {
			case "text", "input_text", "output_text":
				if text := ttsString(block["text"]); text != "" {
					parts = append(parts, text)
				}
			default:
				return ""
			}
		}
		return strings.Join(parts, "")
	}
	return ""
}

// kimiWebToolContract renderiza o contrato do shim com o nonce.
func kimiWebToolContract(nonce string, toolLines []string) string {
	lines := []string{
		"\n\n---\nCLIENT TOOL PROTOCOL (active in this conversation):",
		"The client application provides tools beyond your built-in ones. They are NOT in your native tool registry; they are invoked via a plain-text protocol: the client parses your reply and executes the tool on the user machine. Treat these client tools as fully available to you; never claim they are unavailable. To invoke one, reply with a single line containing a <tool> block",
		fmt.Sprintf(`with JSON that includes the secret binding "_nonce": "%s":`, nonce),
		fmt.Sprintf(`<tool>{"name": "<tool_name>", "arguments": { ... }, "_nonce": "%s"}</tool>`, nonce),
		"These client tools ARE available to you in this conversation. If the user asks you to inspect, create, edit, or verify files, you MUST invoke the relevant client tool before answering. Do not claim that the tools are unavailable and do not describe steps instead of invoking them.",
		"Use a tool ONLY when the request genuinely needs workspace action. For greetings, small talk, thanks, or any question you can answer from your own knowledge, reply directly with the answer and NO <tool> block.",
		"",
		"Available tools:",
	}
	lines = append(lines, toolLines...)
	lines = append(lines, "[Client protocol reminder: the client-tool contract above is active in this conversation. These client tools ARE available via the <tool> block protocol.]")
	return strings.Join(lines, "\n")
}

// kimiWebToolLines serializa cada tool como "- name: description\n  parameters: {...}".
func kimiWebToolLines(tools []any) []string {
	lines := []string{}
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		function, _ := tool["function"].(map[string]any)
		name := ttsString(function["name"])
		if name == "" {
			continue
		}
		line := "- " + name
		if description := ttsString(function["description"]); description != "" {
			line += ": " + description
		}
		if parameters := function["parameters"]; parameters != nil {
			if encoded, err := json.Marshal(parameters); err == nil {
				line += "\n  parameters: " + string(encoded)
			}
		}
		lines = append(lines, line)
	}
	return lines
}

// parseKimiWebToolCalls extrai blocos <tool>{...}</tool> (ou <tool_call>) do
// texto. JSON nu nunca vira tool call; nonce presente e errado rejeita o bloco
// (anti copy-attack); nonce ausente é tolerado.
func parseKimiWebToolCalls(text, nonce string) (string, []any) {
	if !strings.Contains(text, "<tool>") && !strings.Contains(text, "<tool_call") {
		return text, nil
	}
	calls := []any{}
	accepted := [][2]int{}
	for _, tag := range []struct{ open, close string }{{"<tool>", "</tool>"},
		{"<tool_call>", "</tool_call>"},
	} {
		rest := text
		base := 0
		for {
			start := strings.Index(rest, tag.open)
			if start < 0 {
				break
			}
			endRel := strings.Index(rest[start:], tag.close)
			if endRel < 0 {
				break
			}
			end := start + endRel + len(tag.close)
			inner := strings.TrimSpace(rest[start+len(tag.open) : start+endRel])
			if call, ok := kimiWebParseToolJSON(inner, nonce, len(calls)); ok {
				calls = append(calls, call)
				accepted = append(accepted, [2]int{base + start, base + end})
			}
			rest = rest[end:]
			base += end
		}
	}
	if len(calls) == 0 {
		return text, nil
	}
	var stripped strings.Builder
	last := 0
	for _, span := range accepted {
		stripped.WriteString(text[last:span[0]])
		last = span[1]
	}
	stripped.WriteString(text[last:])
	return stripped.String(), calls
}

// openAIToolCalls embrulha as chamadas do shim no shape OpenAI (id/type/function).
func openAIToolCalls(calls []any) []any {
	wrapped := make([]any, 0, len(calls))
	for index, call := range calls {
		wrapped = append(wrapped, map[string]any{
			"index": index, "id": fmt.Sprintf("call_%d", index), "type": "function", "function": call,
		})
	}
	return wrapped
}

func kimiWebParseToolJSON(raw, nonce string, index int) (map[string]any, bool) {
	var parsed map[string]any
	if json.Unmarshal([]byte(raw), &parsed) != nil {
		return nil, false
	}
	name, _ := parsed["name"].(string)
	if name == "" {
		name, _ = parsed["command"].(string)
	}
	if name == "" || name == "<tool_name>" {
		return nil, false
	}
	if claimed, has := parsed["_nonce"].(string); has && claimed != nonce && !isRecentKimiWebNonce(claimed) {
		return nil, false
	}
	arguments := "{}"
	switch args := parsed["arguments"].(type) {
	case string:
		if strings.TrimSpace(args) != "" {
			arguments = args
		}
	case map[string]any, []any:
		if encoded, err := json.Marshal(args); err == nil {
			arguments = string(encoded)
		}
	}
	return map[string]any{"name": name, "arguments": arguments}, true
}

// Nonces por request: 12 chars hex, com janela de recentes para aceitar o
// nonce do turno anterior em continuações (paridade com o shim da origem).
var (
	kimiWebNonceMu     sync.Mutex
	kimiWebRecentNonce []string
)

func newKimiWebNonce() string {
	raw := make([]byte, 6)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano()&0xffffffffffff)
	}
	nonce := fmt.Sprintf("%x", raw)
	kimiWebNonceMu.Lock()
	kimiWebRecentNonce = append(kimiWebRecentNonce, nonce)
	if len(kimiWebRecentNonce) > 12 {
		kimiWebRecentNonce = kimiWebRecentNonce[len(kimiWebRecentNonce)-12:]
	}
	kimiWebNonceMu.Unlock()
	return nonce
}

func isRecentKimiWebNonce(value string) bool {
	kimiWebNonceMu.Lock()
	defer kimiWebNonceMu.Unlock()
	for _, nonce := range kimiWebRecentNonce {
		if nonce == value {
			return true
		}
	}
	return false
}
