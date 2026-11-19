package handlers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// kimiWebMockUpstream decodifica o ChatRequest recebido e responde com o
// stream Connect produzido por respond(prompt); captura o Authorization.
type kimiWebMockUpstream struct {
	respond       func(prompt string) []byte
	chatRequest   map[string]any
	auth          string
	lastPrompt    string
	requestFrames []([]byte)
}

func (m *kimiWebMockUpstream) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if r.Header.Get("Content-Type") != "application/connect+json" {
		w.WriteHeader(415)
		return
	}
	m.auth = r.Header.Get("Authorization")
	payload, err := io.ReadAll(bufio.NewReader(bytes.NewReader(body)))
	if err != nil {
		w.WriteHeader(400)
		return
	}
	// Pega o primeiro frame (request único Connect-unary).
	flags := payload[0]
	length := int(payload[1])<<24 | int(payload[2])<<16 | int(payload[3])<<8 | int(payload[4])
	m.requestFrames = append(m.requestFrames, payload[0:5+length])
	_ = flags
	if json.Unmarshal(payload[5:5+length], &m.chatRequest) != nil {
		w.WriteHeader(400)
		return
	}
	m.lastPrompt = kimiWebMockPrompt(m.chatRequest["message"])
	w.Header().Set("Content-Type", "application/connect+json")
	w.WriteHeader(200)
	if m.respond != nil {
		w.Write(m.respond(m.lastPrompt))
	}
}

// kimiWebMockPrompt extrai o texto do block do message do ChatRequest.
func kimiWebMockPrompt(message any) string {
	typed, _ := message.(map[string]any)
	blocks, _ := typed["blocks"].([]any)
	if len(blocks) == 0 {
		return ""
	}
	block, _ := blocks[0].(map[string]any)
	text, _ := block["text"].(map[string]any)
	content, _ := text["content"].(string)
	return content
}

// kimiWebResponse monta o stream Connect: raciocínio, texto e EndStream vazio.
func kimiWebResponse(think, text string) []byte {
	frames := []byte{}
	if think != "" {
		frames = append(frames, kimiWebFrame(map[string]any{"op": "set", "mask": "block.think", "block": map[string]any{"think": map[string]any{"content": think}}})...)
	}
	if text != "" {
		frames = append(frames, kimiWebFrame(map[string]any{"op": "set", "mask": "block.text", "block": map[string]any{"text": map[string]any{"content": text}}})...)
	}
	end := kimiWebFrame(map[string]any{})
	end[0] = 0x02
	return append(frames, end...)
}

// kimiWebPrompt devolve o último prompt recebido pelo mock.
func kimiWebPrompt(t *testing.T, upstream *kimiWebMockUpstream) string {
	t.Helper()
	return upstream.lastPrompt
}

func kimiWebTestSend(t *testing.T, transport http.RoundTripper) *TranslatorSend {
	t.Helper()
	return &TranslatorSend{
		Connections: sendConnections{[]map[string]any{{
			"id":       "conn-kimi",
			"provider": "kimi-web",
			"isActive": true,
			"apiKey":   "access-token-fake",
		}}},
		Client: &http.Client{Transport: transport},
		GetPool: func(ctx context.Context, id string) (map[string]any, error) {
			return nil, nil
		},
	}
}

func kimiWebKimiTransport(t *testing.T, upstream *kimiWebMockUpstream) http.RoundTripper {
	return kiroImportTransport(func(r *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		upstream.handle(recorder, r)
		response := recorder.Result()
		response.Body = io.NopCloser(recorder.Body)
		return response, nil
	})
}

func TestKimiWebSendE2ENoTools(t *testing.T) {
	upstream := &kimiWebMockUpstream{respond: func(string) []byte { return kimiWebResponse("pensando", "olá mundo") }}
	send := kimiWebTestSend(t, kimiWebKimiTransport(t, upstream))
	w := httptest.NewRecorder()
	send.SendKimiWeb(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"stream":false}`)), "kimi-web", "k3", map[string]any{
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
		t.Fatalf("conteúdo: %q", message["content"])
	}
	if message["reasoning_content"] != "pensando" {
		t.Fatalf("reasoning: %q", message["reasoning_content"])
	}
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish: %v", choice["finish_reason"])
	}
	// ChatRequest validado: fold, framing, options e auth.
	if upstream.chatRequest["scenario"] != "SCENARIO_CHAT" {
		t.Fatalf("scenario: %v", upstream.chatRequest["scenario"])
	}
	options := upstream.chatRequest["options"].(map[string]any)
	if options["system_prompt"] != "Seja breve" {
		t.Fatalf("system_prompt: %v", options["system_prompt"])
	}
	if options["model"] != kimiWebModel {
		t.Fatalf("model: %v", options["model"])
	}
	if got := kimiWebPrompt(t, upstream); got != "diga oi" {
		t.Fatalf("prompt dobrado: %q", got)
	}
	if upstream.auth != "Bearer access-token-fake" {
		t.Fatalf("auth: %q", upstream.auth)
	}
}

func TestKimiWebSendE2EStreamRelay(t *testing.T) {
	upstream := &kimiWebMockUpstream{respond: func(string) []byte { return kimiWebResponse("pensando", "olá mundo") }}
	send := kimiWebTestSend(t, kimiWebKimiTransport(t, upstream))
	w := httptest.NewRecorder()
	send.SendKimiWeb(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"stream":true}`)), "kimi-web", "k3", map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "diga oi"}},
	}, "openai", true)
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status %d ctype %q body %s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"reasoning_content":"pensando"`) {
		t.Fatalf("stream sem reasoning: %s", body)
	}
	if !strings.Contains(body, `"content":"olá mundo"`) {
		t.Fatalf("stream sem texto: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("stream sem finish: %s", body)
	}
}

func TestKimiWebSendE2EToolResultContinuation(t *testing.T) {
	upstream := &kimiWebMockUpstream{respond: func(string) []byte { return kimiWebResponse("", "arquivos listados") }}
	send := kimiWebTestSend(t, kimiWebKimiTransport(t, upstream))
	w := httptest.NewRecorder()
	send.SendKimiWeb(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"stream":false}`)), "kimi-web", "k3", map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "liste /tmp"},
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{"id": "call_0", "type": "function", "function": map[string]any{"name": "list_files", "arguments": `{"path":"/tmp"}`}}}},
			map[string]any{"role": "tool", "tool_call_id": "call_0", "content": "a.txt"},
		},
	}, "openai", false)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	prompt := kimiWebPrompt(t, upstream)
	if !strings.Contains(prompt, "User: [tool result for call_0]") || !strings.Contains(prompt, "a.txt") || !strings.Contains(prompt, "invoked tools") {
		t.Fatalf("continuação de tool não dobrada: %q", prompt)
	}
	if !strings.Contains(w.Body.String(), "arquivos listados") {
		t.Fatalf("resposta final ausente: %s", w.Body.String())
	}
}

func TestKimiWebSendE2ERefreshOn401(t *testing.T) {
	upstream := &kimiWebMockUpstream{respond: func(string) []byte { return kimiWebResponse("", "olá mundo") }}
	calls := 0
	refreshed := ""
	send := &TranslatorSend{
		Connections: sendConnections{[]map[string]any{{
			"id":           "conn-kimi",
			"provider":     "kimi-web",
			"isActive":     true,
			"apiKey":       "token-velho",
			"refreshToken": "refresh-fake",
		}}},
		Client: &http.Client{Transport: kiroImportTransport(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.String(), "RefreshToken") {
				raw, _ := io.ReadAll(r.Body)
				var payload struct {
					Token string `json:"refresh_token"`
				}
				json.Unmarshal(raw, &payload)
				refreshed = payload.Token
				if r.Header.Get("Content-Type") != "application/json" || r.Method != "POST" || !strings.Contains(r.URL.String(), "auth.kimi.ai") {
					t.Fatalf("refresh request errado: %s %s %s", r.Method, r.URL, raw)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"accessToken":"token-novo","refreshToken":"refresh-novo"}`))}, nil
			}
			calls++
			recorder := httptest.NewRecorder()
			if calls == 1 {
				recorder.WriteHeader(401)
			} else {
				upstream.handle(recorder, r)
			}
			response := recorder.Result()
			response.Body = io.NopCloser(recorder.Body)
			return response, nil
		})},
		GetPool: func(ctx context.Context, id string) (map[string]any, error) { return nil, nil },
	}
	w := httptest.NewRecorder()
	send.SendKimiWeb(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"stream":false}`)), "kimi-web", "k3", map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "diga oi"}},
	}, "openai", false)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if refreshed != "refresh-fake" {
		t.Fatalf("refresh_token não foi no body: %q", refreshed)
	}
	if upstream.auth != "Bearer token-novo" {
		t.Fatalf("retry sem o token novo: %q", upstream.auth)
	}
	if calls != 2 {
		t.Fatalf("upstream chamado %d vezes", calls)
	}
}
