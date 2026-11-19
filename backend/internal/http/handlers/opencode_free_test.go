package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Executor do free tier do OpenCode (porta do contrato do main do 9router
// v0.5.119: utils/opencodeFingerprint.js + executors/opencode.js).

func TestOpenCodeSessionIDShape(t *testing.T) {
	session := openCodeSessionID()
	if !strings.HasPrefix(session, "ses_") || len(session) != 30 {
		t.Fatalf("shape %q", session)
	}
	// O hex da sessão carrega ~(ms<<12): bit 47 do campo deve ser 1.
	hex := session[4:16]
	if hex[0] < '8' {
		t.Fatalf("sessão não invertida: %q", hex)
	}
	for _, c := range hex {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("hex inválido %q", hex)
		}
	}
	for _, c := range session[16:] {
		if !strings.ContainsRune(openCodeBase62, c) {
			t.Fatalf("base62 inválido %q", session[16:])
		}
	}
}

func TestOpenCodeStableSessionPerIdentity(t *testing.T) {
	first := stableOpenCodeSession("conn:test-identity")
	second := stableOpenCodeSession("conn:test-identity")
	if first != second {
		t.Fatalf("session instável: %s vs %s", first, second)
	}
	other := stableOpenCodeSession("conn:other-identity")
	if other == first {
		t.Fatal("identidades distintas compartilham session")
	}
	forgetOpenCodeSession(first)
	if again := stableOpenCodeSession("conn:test-identity"); again == first {
		t.Fatal("session rejeitada não foi esquecida")
	}
}

func TestOpenCodeHeadersV2(t *testing.T) {
	canonical := "ses_" + strings.Repeat("f", 12) + strings.Repeat("A", 14)
	client := http.Header{}
	client.Set("x-opencode-session", canonical)
	headers := openCodeHeaders(client, "public", "conn-1")
	for _, key := range []string{"x-opencode-session", "x-opencode-session-id", "x-session-affinity", "x-session-id"} {
		if headers[key] != canonical {
			t.Fatalf("%s = %q, want %q", key, headers[key], canonical)
		}
	}
	if _, exists := headers["x-opencode-request"]; exists {
		t.Fatal("x-opencode-request não existe no contrato v2")
	}
	if headers["User-Agent"] != "opencode/latest/2.0.26/cli" || headers["x-opencode-client"] != "cli" || headers["x-opencode-project"] != "global" {
		t.Fatalf("headers %v", headers)
	}
	if headers["Authorization"] != "Bearer public" || headers["Accept"] != "text/event-stream" {
		t.Fatalf("auth/accept %v", headers)
	}
	// Session malformada do cliente é substituída por uma estável da conexão.
	bad := openCodeHeaders(http.Header{"x-opencode-session": {"abc"}}, "", "conn-1")
	if bad["x-opencode-session"] == "abc" || !openCodePlausibleID(bad["x-opencode-session"]) {
		t.Fatalf("session %q", bad["x-opencode-session"])
	}
	if bad["x-opencode-session"] != openCodeHeaders(http.Header{}, "", "conn-1")["x-opencode-session"] {
		t.Fatal("sessions de uma mesma conexão divergem")
	}
}

func TestOpenCodeFingerprintConcealAndAppend(t *testing.T) {
	// Tool do caller com nome gated por substring "terminal": viaja sob alias.
	body := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "my_terminal_tool", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}},
			map[string]any{"type": "function", "function": map[string]any{"name": "read", "description": "Caller read.", "parameters": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}, "offset": map[string]any{"type": "integer"}, "limit": map[string]any{"type": "integer"}}, "required": []string{"path"}}}},
		},
	}
	renames := openCodeApplyFingerprint(body)
	if renames["run_command"] != "my_terminal_tool" {
		t.Fatalf("renames %v", renames)
	}
	tools := body["tools"].([]any)
	names := []string{}
	for _, raw := range tools {
		tool := raw.(map[string]any)
		function := tool["function"].(map[string]any)
		names = append(names, ttsString(function["name"]))
	}
	joined := "," + strings.Join(names, ",") + ","
	for _, want := range []string{"run_command", "read", "shell"} {
		if !strings.Contains(joined, ","+want+",") {
			t.Fatalf("faltou %s em %v", want, names)
		}
	}
	// read do caller com schema compatível: normalizado in place para a def
	// oficial (mesma description + params verbatim do CLI).
	for _, raw := range tools {
		function := raw.(map[string]any)["function"].(map[string]any)
		if ttsString(function["name"]) == "read" {
			if ttsString(function["description"]) != ttsString(openCodeFingerprintDefinitions["read"]["description"]) {
				t.Fatal("read não normalizado in place")
			}
		}
	}
	// Nota de placeholders anexada ao system (read foi appended).
	if encoded, err := json.Marshal(body); err != nil || !strings.Contains(string(encoded), "placeholder that does nothing") {
		t.Fatalf("nota de placeholders ausente: %s", err)
	}
	// Bloco <env> anexado ao primeiro system message (encoder sem escape HTML).
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(body); err != nil || !strings.Contains(buffer.String(), "<env>") {
		t.Fatal("<env> ausente")
	}
}

func TestOpenCodeFingerprintCollisionGoesClientAlias(t *testing.T) {
	// Colisão com schema divergente viaja sob <key>_client e o nome original é
	// restaurado na resposta.
	body := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "shell", "description": "Different shell.", "parameters": map[string]any{"type": "object", "properties": map[string]any{"cmd": map[string]any{"type": "string"}}}}},
		},
	}
	renames := openCodeApplyFingerprint(body)
	if renames["shell_client"] != "shell" {
		t.Fatalf("renames %v", renames)
	}
	names := []string{}
	for _, raw := range body["tools"].([]any) {
		names = append(names, ttsString(raw.(map[string]any)["function"].(map[string]any)["name"]))
	}
	if len(names) != 3 || !strings.Contains(strings.Join(names, ","), "shell_client") {
		t.Fatalf("tools %v", names)
	}
	payload := map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{
		map[string]any{"function": map[string]any{"name": "shell_client"}},
	}}}}}
	openCodeRestoreNames(payload, renames)
	got := payload["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["name"]
	if got != "shell" {
		t.Fatalf("restaurou %q", got)
	}
}

func TestOpenCodeFingerprintPassthroughAndNone(t *testing.T) {
	// Def verbatim do CLI: passthrough, sem renames.
	body := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name":        "shell",
			"description": openCodeFingerprintDefinitions["shell"]["description"],
			"parameters":  openCodeFingerprintDefinitions["shell"]["parameters"],
		}}},
		"tool_choice": "none",
	}
	if renames := openCodeApplyFingerprint(body); renames != nil {
		t.Fatalf("renames %v", renames)
	}
	// tool_choice "none" é deletado (upstream 403).
	if body["tool_choice"] != nil {
		t.Fatalf("tool_choice %v", body["tool_choice"])
	}
	names := []string{}
	for _, raw := range body["tools"].([]any) {
		names = append(names, ttsString(raw.(map[string]any)["function"].(map[string]any)["name"]))
	}
	if len(names) != 2 {
		t.Fatalf("quarteto v2 é read+shell: %v", names)
	}
}

func TestOpenCodeFingerprintWithoutCallerToolsGetsEnv(t *testing.T) {
	body := map[string]any{}
	openCodeApplyFingerprint(body)
	if body["tool_choice"] != nil {
		t.Fatalf("tool_choice %v", body["tool_choice"])
	}
	tools := body["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("par read+shell: %d", len(tools))
	}
	// Sem system: bloco <env> entra como primeiro message.
	messages := body["messages"].([]any)
	if ttsString(messages[0].(map[string]any)["role"]) != "system" || !strings.Contains(ttsString(messages[0].(map[string]any)["content"]), "<env>") {
		t.Fatalf("messages %v", messages)
	}
}

func openCodeSSE(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return "data: " + string(raw)
}

func TestOpenCodeFoldStreamAndRestore(t *testing.T) {
	call1 := map[string]any{"index": 0, "id": "call_1", "function": map[string]any{"name": "run_command", "arguments": `{"a"`}}
	call2 := map[string]any{"index": 0, "function": map[string]any{"name": "", "arguments": ":1}"}}
	chunks := []string{
		openCodeSSE(t, map[string]any{"id": "c1", "created": 1, "model": "m",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": ""}}}}),
		openCodeSSE(t, map[string]any{
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": "pensando ", "tool_calls": []any{call1}}}}}),
		openCodeSSE(t, map[string]any{
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "ok", "tool_calls": []any{call2}}}}}),
		openCodeSSE(t, map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}},
			"usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 5, "total_tokens": 8}}),
		"data: [DONE]",
		"",
	}
	completion, err := openCodeFoldStream(strings.NewReader(strings.Join(chunks, "\n")), "m", map[string]string{"run_command": "My_Terminal_Tool"})
	if err != nil {
		t.Fatal(err)
	}
	choice := completion["choices"].([]any)[0].(map[string]any)
	message := choice["message"].(map[string]any)
	if message["content"] != "ok" || message["reasoning_content"] != "pensando " {
		t.Fatalf("message %v", message)
	}
	call := message["tool_calls"].([]any)[0].(map[string]any)
	function := call["function"].(map[string]any)
	if function["name"] != "My_Terminal_Tool" || function["arguments"] != `{"a":1}` {
		t.Fatalf("call %v", call)
	}
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish %v", choice["finish_reason"])
	}
	usage := completion["usage"].(map[string]any)
	if usage["total_tokens"] != float64(8) {
		t.Fatalf("usage %v", usage)
	}
	encoded, _ := json.Marshal(completion)
	if strings.Contains(string(encoded), `"run_command"`) {
		t.Fatal("alias vazou na resposta")
	}
}

func TestOpenCodeFoldStreamSurfacesMidStreamError(t *testing.T) {
	stream := "data: {\"error\":\"boom\"}\n\n"
	if _, err := openCodeFoldStream(strings.NewReader(stream), "m", nil); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("erro do stream engolido: %v", err)
	}
}

func TestOpenCodeClientHelloSpecShape(t *testing.T) {
	spec := openCodeClientHelloSpec()
	// 17 ciphers BoringSSL do Bun 1.3.14, sem GREASE, key_share único x25519.
	if len(spec.CipherSuites) != 17 {
		t.Fatalf("ciphers %d", len(spec.CipherSuites))
	}
	if len(spec.Extensions) != 14 {
		t.Fatalf("extensions %d", len(spec.Extensions))
	}
}
