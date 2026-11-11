package handlers

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/bits"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DeepSeek Web (chat.deepseek.com) exige um proof-of-work por request: o
// cliente pede um challenge, procura o contador cujo hash bate o alvo e envia
// o resultado em x-ds-pow-response. A API não tem system message nem tools,
// então o histórico dobra num prompt único e as tools voltam via shim de texto.
const (
	deepseekWebBaseURL        = "https://chat.deepseek.com"
	deepseekWebSessionURL     = deepseekWebBaseURL + "/api/v0/chat_session/create"
	deepseekWebChallengeURL   = deepseekWebBaseURL + "/api/v0/chat/create_pow_challenge"
	deepseekWebCompletionURL  = deepseekWebBaseURL + "/api/v0/chat/completion"
	deepseekWebCompletionPath = "/api/v0/chat/completion"
	deepseekWebUA             = "Mozilla/5.0 (X11; Linux x86_64; rv:155.0) Gecko/20100101 Firefox/155.0"
	deepseekWebModel          = "default"
	deepseekWebTimeZone       = "-10800"
	deepseekWebLocale         = "pt_BR"
	deepseekWebClientVer      = "2.7.1"
	deepseekWebMaxLine        = 8 << 20
)

// deepseekWebPowAnswer é o JSON que vai base64 em x-ds-pow-response.
type deepseekWebPowAnswer struct {
	Algorithm  string `json:"algorithm"`
	Challenge  string `json:"challenge"`
	Salt       string `json:"salt"`
	Answer     int64  `json:"answer"`
	Signature  string `json:"signature"`
	TargetPath string `json:"target_path"`
}

// deepseekWebChallenge é o payload de create_pow_challenge.
type deepseekWebChallenge struct {
	Algorithm  string `json:"algorithm"`
	Challenge  string `json:"challenge"`
	Salt       string `json:"salt"`
	Signature  string `json:"signature"`
	Difficulty int64  `json:"difficulty"`
	ExpireAt   int64  `json:"expire_at"`
}

// SendDeepSeekWeb expõe o executor do chat.deepseek.com ao gateway e ao node.
func (t *TranslatorSend) SendDeepSeekWeb(w http.ResponseWriter, r *http.Request, provider, model string, body map[string]any, source string, stream bool) {
	t.sendDeepSeekWeb(w, r, provider, model, body, source, stream)
}

// sendDeepSeekWeb executa o chat do chat.deepseek.com e dobra o stream SSE de
// volta para o formato do cliente (SSE/JSON OpenAI; Claude/Responses convertidos).
func (t *TranslatorSend) sendDeepSeekWeb(w http.ResponseWriter, r *http.Request, provider, model string, body map[string]any, source string, stream bool) {
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
		fail(400, "Missing DeepSeek Web token — paste the 64-char Bearer token from chat.deepseek.com")
		return
	}

	intermediate := body
	if source != "openai" {
		if intermediate, err = translateSource(body, source); err != nil {
			fail(400, err.Error())
			return
		}
	}

	// Tool shim: a API web não aceita tools; o contrato vai no prompt e as
	// chamadas voltam como texto <tool>{...}</tool>.
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
		fail(400, "DeepSeek Web requires a non-empty user message")
		return
	}
	if systemPrompt != "" {
		prompt = systemPrompt + "\n\n" + prompt
	}

	metadata, _ := connection["providerSpecificData"].(map[string]any)
	client, relay, cleanup, err := connectionHTTP(r.Context(), metadata, t.GetPool, t.Client)
	if err != nil {
		fail(500, err.Error())
		return
	}
	defer cleanup()

	expired := "DeepSeek token rejected — update the connection with a fresh Bearer token from chat.deepseek.com"
	call := func(url string, payload any, pow string, target any) error {
		return deepseekWebAPICall(r.Context(), client, relay, url, access, pow, payload, target)
	}

	var session struct {
		BizData struct {
			ChatSession struct {
				ID string `json:"id"`
			} `json:"chat_session"`
		} `json:"biz_data"`
	}
	if err := call(deepseekWebSessionURL, map[string]any{}, "", &session); err != nil {
		if isDeepSeekWebAuthError(err) {
			fail(401, expired)
			return
		}
		fail(502, err.Error())
		return
	}
	if session.BizData.ChatSession.ID == "" {
		fail(502, "DeepSeek did not return a chat session id")
		return
	}

	var challenge struct {
		BizData struct {
			Challenge deepseekWebChallenge `json:"challenge"`
		} `json:"biz_data"`
	}
	if err := call(deepseekWebChallengeURL, map[string]any{"target_path": deepseekWebCompletionPath}, "", &challenge); err != nil {
		if isDeepSeekWebAuthError(err) {
			fail(401, expired)
			return
		}
		fail(502, err.Error())
		return
	}
	pow, err := deepseekWebSolve(challenge.BizData.Challenge)
	if err != nil {
		fail(502, err.Error())
		return
	}

	request, err := deepseekWebRequest(r.Context(), client, relay, deepseekWebCompletionURL, access, pow, map[string]any{
		"chat_session_id":   session.BizData.ChatSession.ID,
		"parent_message_id": nil,
		"model_type":        deepseekWebModel,
		"prompt":            prompt,
		"ref_file_ids":      []any{},
		"thinking_enabled":  false,
		"search_enabled":    false,
		"action":            nil,
		"preempt":           false,
		"shumei_device_id":  "",
	})
	if err != nil {
		fail(502, err.Error())
		return
	}
	response, err := client.Do(request)
	if err != nil {
		fail(502, err.Error())
		return
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		io.Copy(io.Discard, io.LimitReader(response.Body, 8<<10))
		fail(response.StatusCode, expired)
		return
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		details, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
		fail(response.StatusCode, fmt.Sprintf("DeepSeek error: %d — %s", response.StatusCode, strings.TrimSpace(string(details))))
		return
	}
	// Erro de negócio chega como JSON com code != 0 em HTTP 200.
	if !strings.Contains(response.Header.Get("Content-Type"), "event-stream") {
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		var envelope struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		if json.Unmarshal(raw, &envelope) == nil {
			fail(502, fmt.Sprintf("DeepSeek error: %d — %s", envelope.Code, envelope.Msg))
			return
		}
		fail(502, "DeepSeek unexpected response: "+strings.TrimSpace(string(raw)))
		return
	}

	reader := bufio.NewReader(response.Body)
	if stream && len(requestedTools) == 0 {
		t.deepseekWebStreamRelay(w, reader, model)
		return
	}
	text, think, err := deepseekWebAccumulate(reader)
	if err != nil {
		fail(502, "DeepSeek stream error: "+err.Error())
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
			"id":      fmt.Sprintf("chatcmpl-deepseek-%d", time.Now().UnixNano()),
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
		})
		return
	}
	writeOpenAIDeltas(w, model, think, content, toolCalls, finish)
}

// deepseekWebStreamRelay repassa o SSE do DeepSeek como SSE OpenAI em tempo real.
func (t *TranslatorSend) deepseekWebStreamRelay(w http.ResponseWriter, reader *bufio.Reader, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)
	id := fmt.Sprintf("chatcmpl-deepseek-%d", time.Now().UnixNano())
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
	_, _ = deepseekWebScan(reader, func(text, think string) {
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

// writeOpenAIDeltas emite a resposta acumulada de uma vez em SSE OpenAI.
func writeOpenAIDeltas(w http.ResponseWriter, model, think, content string, toolCalls []any, finish string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, _ := w.(http.Flusher)
	id := fmt.Sprintf("chatcmpl-deepseek-%d", time.Now().UnixNano())
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

// deepseekWebAccumulate consome o SSE e devolve (texto, raciocínio).
func deepseekWebAccumulate(reader *bufio.Reader) (string, string, error) {
	text, think := strings.Builder{}, strings.Builder{}
	finished, err := deepseekWebScan(reader, func(deltaText, deltaThink string) {
		text.WriteString(deltaText)
		think.WriteString(deltaThink)
	})
	if err != nil {
		return "", "", err
	}
	if !finished {
		return "", "", fmt.Errorf("stream ended without response/status FINISHED")
	}
	return text.String(), think.String(), nil
}

// deepseekWebScan percorre os eventos SSE e chama onDelta a cada fragmento de
// texto/raciocínio. Retorna se response/status FINISHED foi visto.
func deepseekWebScan(reader *bufio.Reader, onDelta func(text, think string)) (bool, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64<<10), deepseekWebMaxLine)
	finished := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		text, think, status, err := deepseekWebDelta([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))))
		if err != nil {
			return finished, err
		}
		if (text != "" || think != "") && onDelta != nil {
			onDelta(text, think)
		}
		if status == "FINISHED" {
			finished = true
		}
	}
	return finished, scanner.Err()
}

// deepseekWebDelta interpreta um payload SSE e devolve o texto/raciocínio
// incremental e o status da resposta. Erros de negócio vêm com code != 0.
func deepseekWebDelta(payload []byte) (string, string, string, error) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(payload, &raw) != nil {
		return "", "", "", nil
	}
	if codeRaw, ok := raw["code"]; ok {
		var code int
		if json.Unmarshal(codeRaw, &code) == nil && code != 0 {
			var message string
			json.Unmarshal(raw["msg"], &message)
			return "", "", "", fmt.Errorf("DeepSeek error %d: %s", code, message)
		}
	}
	var path, op string
	if pathRaw, ok := raw["p"]; ok {
		json.Unmarshal(pathRaw, &path)
		json.Unmarshal(raw["o"], &op)
		if path == "response/status" {
			var status string
			json.Unmarshal(raw["v"], &status)
			return "", "", status, nil
		}
		if op == "APPEND" && strings.HasSuffix(path, "/content") {
			var delta string
			if json.Unmarshal(raw["v"], &delta) == nil {
				return delta, "", "", nil
			}
		}
		return "", "", "", nil
	}
	if value, ok := raw["v"]; ok {
		var delta string
		if json.Unmarshal(value, &delta) == nil {
			return delta, "", "", nil
		}
		var snapshot struct {
			Response struct {
				Status    string `json:"status"`
				Fragments []struct {
					Type    string `json:"type"`
					Content string `json:"content"`
				} `json:"fragments"`
			} `json:"response"`
		}
		if json.Unmarshal(value, &snapshot) == nil && snapshot.Response.Fragments != nil {
			text, think := strings.Builder{}, strings.Builder{}
			for _, fragment := range snapshot.Response.Fragments {
				if fragment.Type == "THINK" {
					think.WriteString(fragment.Content)
				} else {
					text.WriteString(fragment.Content)
				}
			}
			return text.String(), think.String(), snapshot.Response.Status, nil
		}
	}
	return "", "", "", nil
}

// deepseekWebAPICall faz uma chamada JSON unary e decodifica data em target.
func deepseekWebAPICall(ctx context.Context, client *http.Client, relay, url, token, pow string, payload, target any) error {
	request, err := deepseekWebRequest(ctx, client, relay, url, token, pow, payload)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return deepseekWebHTTPErrorFor(response.StatusCode, raw)
	}
	var envelope struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("DeepSeek protocol error: %s", strings.TrimSpace(string(raw)))
	}
	if envelope.Code != 0 {
		return fmt.Errorf("DeepSeek error %d: %s", envelope.Code, envelope.Msg)
	}
	if target == nil {
		return nil
	}
	return json.Unmarshal(envelope.Data, target)
}

// deepseekWebHTTPError descreve status e corpo; 401/403 marca expiração.
type deepseekWebHTTPError struct {
	status int
	body   string
}

func (e *deepseekWebHTTPError) Error() string {
	return fmt.Sprintf("DeepSeek error: %d — %s", e.status, e.body)
}

func deepseekWebHTTPErrorFor(status int, raw []byte) error {
	return &deepseekWebHTTPError{status: status, body: strings.TrimSpace(string(raw))}
}

func isDeepSeekWebAuthError(err error) bool {
	httpErr, ok := err.(*deepseekWebHTTPError)
	return ok && (httpErr.status == http.StatusUnauthorized || httpErr.status == http.StatusForbidden)
}

// deepseekWebRequest monta o POST com os headers mínimos validados ao vivo.
func deepseekWebRequest(ctx context.Context, client *http.Client, relay, url, token, pow string, payload any) (*http.Request, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "*/*")
	request.Header.Set("Origin", deepseekWebBaseURL)
	request.Header.Set("Referer", deepseekWebBaseURL+"/")
	request.Header.Set("User-Agent", deepseekWebUA)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("x-device-id", deepseekWebDeviceID(token))
	request.Header.Set("x-device-model", "")
	request.Header.Set("x-client-bundle-id", "com.deepseek.chat")
	request.Header.Set("x-client-platform", "web")
	request.Header.Set("x-client-version", deepseekWebClientVer)
	request.Header.Set("x-client-locale", deepseekWebLocale)
	request.Header.Set("x-client-timezone-offset", deepseekWebTimeZone)
	if pow != "" {
		request.Header.Set("x-ds-pow-response", pow)
	}
	if err := rewriteConnectionRelay(request, relay); err != nil {
		return nil, err
	}
	return request, nil
}

// deepseekWebDeviceID deriva um UUID estável do token: a API espera um device
// fixo por cliente e não expõe registro, então hash evita estado extra.
func deepseekWebDeviceID(token string) string {
	sum := sha256.Sum256([]byte("deepseek-web:" + token))
	raw := sum[:16]
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}

// deepseekWebSolve resolve o DeepSeekHashV1 e devolve o header já codificado.
func deepseekWebSolve(challenge deepseekWebChallenge) (string, error) {
	if challenge.Challenge == "" || challenge.Difficulty <= 0 {
		return "", fmt.Errorf("DeepSeek sent an empty proof-of-work challenge")
	}
	prefix := challenge.Salt + "_" + strconv.FormatInt(challenge.ExpireAt, 10) + "_"
	var answer int64 = -1
	for i := int64(0); i < challenge.Difficulty; i++ {
		if deepseekWebHash([]byte(prefix+strconv.FormatInt(i, 10))) == challenge.Challenge {
			answer = i
			break
		}
	}
	if answer < 0 {
		return "", fmt.Errorf("DeepSeek proof-of-work has no solution below difficulty %d", challenge.Difficulty)
	}
	payload, _ := json.Marshal(deepseekWebPowAnswer{
		Algorithm:  challenge.Algorithm,
		Challenge:  challenge.Challenge,
		Salt:       challenge.Salt,
		Answer:     answer,
		Signature:  challenge.Signature,
		TargetPath: deepseekWebCompletionPath,
	})
	return base64.StdEncoding.EncodeToString(payload), nil
}

// deepseekWebRC são as constantes de round do Keccak. DeepSeekHashV1 roda
// 23 rounds (RC[1..23]), pulando o round 0 do SHA3-256 padrão.
var deepseekWebRC = [24]uint64{
	0x0000000000000001, 0x0000000000008082, 0x800000000000808a, 0x8000000080008000,
	0x000000000000808b, 0x0000000080000001, 0x8000000080008081, 0x8000000000008009,
	0x000000000000008a, 0x0000000000000088, 0x0000000080008009, 0x000000008000000a,
	0x000000008000808b, 0x800000000000008b, 0x8000000000008089, 0x8000000000008003,
	0x8000000000008002, 0x8000000000000080, 0x000000000000800a, 0x800000008000000a,
	0x8000000080008081, 0x8000000000008080, 0x0000000080000001, 0x8000000080008008,
}

// deepseekWebRot é a tabela de rotações por lane (x + 5y).
var deepseekWebRot = [25]int{
	0, 1, 62, 28, 27,
	36, 44, 6, 55, 20,
	3, 10, 43, 25, 39,
	41, 45, 15, 21, 8,
	18, 2, 61, 56, 14,
}

func deepseekWebPermute(state *[25]uint64) {
	var c, d [5]uint64
	for round := 1; round < 24; round++ {
		for x := 0; x < 5; x++ {
			c[x] = state[x] ^ state[x+5] ^ state[x+10] ^ state[x+15] ^ state[x+20]
		}
		for x := 0; x < 5; x++ {
			d[x] = c[(x+4)%5] ^ bits.RotateLeft64(c[(x+1)%5], 1)
		}
		for x := 0; x < 5; x++ {
			for y := 0; y < 5; y++ {
				state[x+5*y] ^= d[x]
			}
		}
		var b [25]uint64
		for x := 0; x < 5; x++ {
			for y := 0; y < 5; y++ {
				b[y+5*((2*x+3*y)%5)] = bits.RotateLeft64(state[x+5*y], deepseekWebRot[x+5*y])
			}
		}
		for x := 0; x < 5; x++ {
			for y := 0; y < 5; y++ {
				state[x+5*y] = b[x+5*y] ^ (^b[(x+1)%5+5*y] & b[(x+2)%5+5*y])
			}
		}
		state[0] ^= deepseekWebRC[round]
	}
}

// deepseekWebHash é o Keccak-f1600 de 23 rounds com padding SHA3 e saída de
// 32 bytes em hex — o hash do DeepSeekHashV1 (não é SHA3-256).
func deepseekWebHash(data []byte) string {
	const rate = 136
	padded := make([]byte, len(data)+1)
	copy(padded, data)
	padded[len(data)] = 0x06
	if remainder := len(padded) % rate; remainder != 0 {
		padded = append(padded, make([]byte, rate-remainder)...)
	}
	padded[len(padded)-1] |= 0x80
	var state [25]uint64
	for offset := 0; offset < len(padded); offset += rate {
		for lane := 0; lane < rate/8; lane++ {
			state[lane] ^= binary.LittleEndian.Uint64(padded[offset+lane*8:])
		}
		deepseekWebPermute(&state)
	}
	var digest [32]byte
	for i := 0; i < 4; i++ {
		binary.LittleEndian.PutUint64(digest[i*8:], state[i])
	}
	return hex.EncodeToString(digest[:])
}
