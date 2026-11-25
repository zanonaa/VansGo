package handlers

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTargetConversionPreservesConversationAndTools(t *testing.T) {
	body := map[string]any{"stream": false, "temperature": 0.2, "max_tokens": 777, "messages": []any{map[string]any{"role": "system", "content": "instructions"}, map[string]any{"role": "user", "content": "first"}, map[string]any{"role": "assistant", "content": "answer"}, map[string]any{"role": "user", "content": "followup"}}, "tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "description": "search", "parameters": map[string]any{"type": "object"}}}}}
	for _, format := range []string{"claude", "gemini"} {
		out, err := translateTarget(body, format, "native-model")
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(out)
		text := string(encoded)
		for _, value := range []string{"instructions", "first", "answer", "followup", "lookup", "777"} {
			if !strings.Contains(text, value) {
				t.Fatalf("%s lost %s: %s", format, value, text)
			}
		}
		if strings.Contains(text, `"role":"system"`) {
			t.Fatal("system turn not extracted")
		}
	}
	if body["model"] != nil {
		t.Fatal("conversion mutated source")
	}
}
func TestTranslatorStepThreeUsesAccountEndpointAndNativeBody(t *testing.T) {
	h := &TranslatorTranslate{Connections: sendConnections{items: []map[string]any{{"isActive": true, "apiKey": "account-key"}}}}
	r := httptest.NewRequest("POST", "/", strings.NewReader(`{"step":3,"body":{"provider":"glm","model":"glm-model","body":{"stream":false,"messages":[{"role":"system","content":"instructions"},{"role":"user","content":"hello"}],"max_tokens":4321}}}`))
	w := httptest.NewRecorder()
	h.Post(w, r)
	var result struct {
		Success bool `json:"success"`
		Result  struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
			Body    map[string]any    `json:"body"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || !strings.HasPrefix(result.Result.URL, "https://api.z.ai/") || result.Result.Headers["x-api-key"] != "account-key" || result.Result.Body["system"] == nil || result.Result.Body["max_tokens"] != float64(4321) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}
func TestUnsupportedBlocksFailInsteadOfDisappearing(t *testing.T) {
	_, err := translateTarget(map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "unmapped", "data": "must not vanish"}}}}}, "claude", "model")
	if err == nil {
		t.Fatal("silently discarded unsupported block")
	}
}
func TestTargetConversionGeminiResolvesToolResultByName(t *testing.T) {
	body := map[string]any{"stream": true, "messages": []any{
		map[string]any{"role": "user", "content": "read the file"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "call_42", "type": "function", "function": map[string]any{"name": "read_file", "arguments": "{\"path\":\"main.go\"}"}}}},
		map[string]any{"role": "tool", "tool_call_id": "call_42", "content": "{\"line_count\":3}"},
	}}
	out, err := translateTarget(body, "gemini", "native-model")
	if err != nil {
		t.Fatal(err)
	}
	contents, _ := out["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("expected 3 turns, got %d: %v", len(contents), out["contents"])
	}
	response, _ := contents[2].(map[string]any)
	if response["role"] != "user" {
		t.Fatalf("tool result turn must be role user, got %v", response["role"])
	}
	parts, _ := response["parts"].([]any)
	part, _ := parts[0].(map[string]any)
	function, _ := part["functionResponse"].(map[string]any)
	if function["name"] != "read_file" || function["id"] != "call_42" {
		t.Fatalf("functionResponse not resolved: %v", function)
	}
	responseData, _ := function["response"].(map[string]any)
	if responseData["result"] == nil {
		t.Fatalf("functionResponse.response.result missing: %v", function)
	}
	encoded, _ := json.Marshal(out)
	if strings.Contains(string(encoded), `"role":"tool"`) {
		t.Fatalf("raw tool role leaked into gemini contents: %s", encoded)
	}
	// Sem o assistant declarando o tool_call, o id não resolve e falha explícito.
	_, err = translateTarget(map[string]any{"messages": []any{map[string]any{"role": "tool", "tool_call_id": "call_missing", "content": "orphan"}}}, "gemini", "model")
	if err == nil || !strings.Contains(err.Error(), "call_missing") {
		t.Fatalf("expected explicit unresolved-id error, got %v", err)
	}
	// Texto de resultado que não é JSON segue como string em result.
	out, err = translateTarget(map[string]any{"messages": []any{
		map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "call_7", "type": "function", "function": map[string]any{"name": "lookup", "arguments": "{}"}}}},
		map[string]any{"role": "tool", "tool_call_id": "call_7", "content": "plain text result"},
	}}, "gemini", "model")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(out)
	if !strings.Contains(string(encoded), `"result":"plain text result"`) {
		t.Fatalf("non-JSON tool result lost: %s", encoded)
	}
}
