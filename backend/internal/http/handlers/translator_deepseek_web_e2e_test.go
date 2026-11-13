package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// TestDeepSeekWebPoW resolve o vetor do HAR: challenge real do
// create_pow_challenge com answer conhecido (34521).
func TestDeepSeekWebPoW(t *testing.T) {
	challenge := deepseekWebChallenge{
		Algorithm:  "DeepSeekHashV1",
		Challenge:  "7978a748f6fb6c45e9cb50ee67eb1587c1b388fc88e3b8b90c58697f92c25a8b",
		Salt:       "9bf11eb0c075ed5e505d",
		ExpireAt:   1794358083759,
		Difficulty: 144000,
	}
	pow, err := deepseekWebSolve(challenge)
	if err != nil {
		t.Fatalf("solve: %v", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(pow)
	if err != nil {
		t.Fatalf("pow não é base64: %v", err)
	}
	var answer deepseekWebPowAnswer
	if err := json.Unmarshal(decoded, &answer); err != nil {
		t.Fatalf("pow não é JSON: %v", err)
	}
	if answer.Answer != 34521 {
		t.Fatalf("answer %d, esperado 34521", answer.Answer)
	}
	if answer.TargetPath != "/api/v0/chat/completion" || answer.Challenge != challenge.Challenge {
		t.Fatalf("pow incompleto: %+v", answer)
	}
}

// TestDeepSeekWebDelta cobre o formato SSE real: snapshot inicial, delta
// solto, append explícito, status e erro de negócio.
func TestDeepSeekWebDelta(t *testing.T) {
	text, think, status, err := deepseekWebDelta([]byte(`{"v":{"response":{"status":"WIP","fragments":[{"type":"RESPONSE","content":"Oi"},{"type":"THINK","content":"hmm"}]}}}`))
	if err != nil || text != "Oi" || think != "hmm" || status != "WIP" {
		t.Fatalf("snapshot: %q %q %q %v", text, think, status, err)
	}
	text, _, _, _ = deepseekWebDelta([]byte(`{"v":"!"}`))
	if text != "!" {
		t.Fatalf("delta: %q", text)
	}
	text, _, _, _ = deepseekWebDelta([]byte(`{"p":"response/fragments/-1/content","o":"APPEND","v":" mais"}`))
	if text != " mais" {
		t.Fatalf("append: %q", text)
	}
	_, _, status, _ = deepseekWebDelta([]byte(`{"p":"response/status","o":"SET","v":"FINISHED"}`))
	if status != "FINISHED" {
		t.Fatalf("status: %q", status)
	}
	if _, _, _, err := deepseekWebDelta([]byte(`{"code":40003,"msg":"rate limited"}`)); err == nil || !strings.Contains(err.Error(), "40003") {
		t.Fatalf("erro de negócio: %v", err)
	}
}

type deepseekWebMockUpstream struct {
	respond  func(prompt string) string
	prompt   string
	pow      string
	auth     string
	status   int
	requests []string
}

func (m *deepseekWebMockUpstream) handle(w http.ResponseWriter, r *http.Request) {
	m.requests = append(m.requests, r.URL.Path)
	m.auth = r.Header.Get("Authorization")
	switch r.URL.Path {
	case "/api/v0/chat_session/create":
		io.WriteString(w, `{"code":0,"data":{"biz_data":{"chat_session":{"id":"sess-1"}}}}`)
	case "/api/v0/chat/create_pow_challenge":
		io.WriteString(w, `{"code":0,"data":{"biz_data":{"challenge":`+deepseekWebTestChallengeJSON()+`}}}`)
	case "/api/v0/chat/completion":
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		json.Unmarshal(body, &parsed)
		m.prompt, _ = parsed["prompt"].(string)
		m.pow = r.Header.Get("x-ds-pow-response")
		if m.status != 0 {
			w.WriteHeader(m.status)
			io.WriteString(w, `{"code":401,"msg":"unauthorized"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, deepseekWebSSE(m.respond(m.prompt)))
	default:
		w.WriteHeader(404)
	}
}

// deepseekWebTestChallenge devolve um challenge trivial: o answer é 3.
func deepseekWebTestChallenge() deepseekWebChallenge {
	const salt = "salt-test"
	const expireAt = int64(1794000000000)
	prefix := salt + "_" + strconv.FormatInt(expireAt, 10) + "_"
	return deepseekWebChallenge{
		Algorithm:  "DeepSeekHashV1",
		Challenge:  deepseekWebHash([]byte(prefix + "3")),
		Salt:       salt,
		Signature:  "sig-test",
		ExpireAt:   expireAt,
		Difficulty: 10,
	}
}

func deepseekWebTestChallengeJSON() string {
	encoded, _ := json.Marshal(deepseekWebTestChallenge())
	return string(encoded)
}

// deepseekWebSSE monta o stream como o upstream real: primeiro texto no
// snapshot, o restante em deltas, depois status FINISHED e close.
func deepseekWebSSE(text string) string {
	runes := []rune(text)
	head := text
	rest := []rune{}
	if len(runes) > 3 {
		head = string(runes[:3])
		rest = runes[3:]
	}
	var builder strings.Builder
	builder.WriteString("event: ready\ndata: {\"request_message_id\":1,\"response_message_id\":2}\n\n")
	snapshot, _ := json.Marshal(map[string]any{"v": map[string]any{"response": map[string]any{
		"status":    "WIP",
		"fragments": []any{map[string]any{"type": "RESPONSE", "content": head}},
	}}})
	builder.WriteString("data: " + string(snapshot) + "\n\n")
	for len(rest) > 0 {
		size := 4
		if size > len(rest) {
			size = len(rest)
		}
		delta, _ := json.Marshal(map[string]any{"v": string(rest[:size])})
		builder.WriteString("data: " + string(delta) + "\n\n")
		rest = rest[size:]
	}
	builder.WriteString("data: {\"p\":\"response/status\",\"o\":\"SET\",\"v\":\"FINISHED\"}\n\n")
	builder.WriteString("event: close\ndata: {}\n\n")
	return builder.String()
}

func deepseekWebTestSend(upstream *deepseekWebMockUpstream) *TranslatorSend {
	return &TranslatorSend{
		Connections: sendConnections{[]map[string]any{{
			"id":       "conn-deepseek-web",
			"provider": "deepseek-web",
			"isActive": true,
			"apiKey":   "token-fake-64",
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

func TestDeepSeekWebSendE2ENoTools(t *testing.T) {
	upstream := &deepseekWebMockUpstream{respond: func(string) string { return "olá mundo" }}
	send := deepseekWebTestSend(upstream)
	w := httptest.NewRecorder()
	send.SendDeepSeekWeb(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"stream":false}`)), "deepseek-web", "default", map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "Seja breve"},
			map[string]any{"role": "user", "content": "diga oi"},
		},
	}, "openai", false)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var completion map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &completion); err != nil {
		t.Fatalf("resposta não é JSON: %v", err)
	}
	choice := completion["choices"].([]any)[0].(map[string]any)
	message := choice["message"].(map[string]any)
	if message["content"] != "olá mundo" {
		t.Fatalf("conteúdo: %v", message["content"])
	}
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish: %v", choice["finish_reason"])
	}
	if upstream.auth != "Bearer token-fake-64" {
		t.Fatalf("auth: %q", upstream.auth)
	}
	if upstream.prompt != "Seja breve\n\ndiga oi" {
		t.Fatalf("prompt dobrado: %q", upstream.prompt)
	}
	// O proof-of-work enviado precisa carregar o answer do challenge do mock.
	decoded, _ := base64.StdEncoding.DecodeString(upstream.pow)
	var answer deepseekWebPowAnswer
	if json.Unmarshal(decoded, &answer) != nil || answer.Answer != 3 {
		t.Fatalf("pow inválido: %q (%v)", upstream.pow, answer)
	}
	if len(upstream.requests) != 3 {
		t.Fatalf("chamadas: %v", upstream.requests)
	}
}

func TestDeepSeekWebSendE2EToolResultContinuation(t *testing.T) {
	upstream := &deepseekWebMockUpstream{respond: func(string) string { return "arquivos listados" }}
	send := deepseekWebTestSend(upstream)
	w := httptest.NewRecorder()
	send.SendDeepSeekWeb(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"stream":false}`)), "deepseek-web", "default", map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "liste /tmp"},
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "call_0", "type": "function", "function": map[string]any{"name": "list_files", "arguments": `{"path":"/tmp"}`}}}},
			map[string]any{"role": "tool", "tool_call_id": "call_0", "content": "a.txt"},
		},
	}, "openai", false)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(upstream.prompt, "User: [tool result for call_0]") || !strings.Contains(upstream.prompt, "a.txt") {
		t.Fatalf("continuação de tool não dobrada: %q", upstream.prompt)
	}
	if !strings.Contains(w.Body.String(), "arquivos listados") {
		t.Fatalf("resposta final ausente: %s", w.Body.String())
	}
}

func TestDeepSeekWebSendE2EStreamRelay(t *testing.T) {
	upstream := &deepseekWebMockUpstream{respond: func(string) string { return "olá mundo" }}
	send := deepseekWebTestSend(upstream)
	w := httptest.NewRecorder()
	send.SendDeepSeekWeb(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"stream":true}`)), "deepseek-web", "default", map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "diga oi"}},
	}, "openai", true)
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d ctype %q body %s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"content":"ol`) || !strings.Contains(body, `"finish_reason":"stop"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("stream incompleto: %s", body)
	}
}

func TestDeepSeekWebSendE2EUnauthorized(t *testing.T) {
	upstream := &deepseekWebMockUpstream{status: http.StatusUnauthorized, respond: func(string) string { return "" }}
	send := deepseekWebTestSend(upstream)
	w := httptest.NewRecorder()
	send.SendDeepSeekWeb(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"stream":false}`)), "deepseek-web", "default", map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "diga oi"}},
	}, "openai", false)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "update the connection") {
		t.Fatalf("erro sem orientação de token: %s", w.Body.String())
	}
}
