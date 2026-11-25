package handlers

import (
	"encoding/json"
	"strings"
	"testing"
)

// Repro do e2e com opencode: turn 1 (functionCall do read) recebe a assinatura
// default; a partir do 2º turno de tools os functionCalls históricos chegam sem
// thoughtSignature (clientes OpenAI não carregam) e o upstream os rejeita — a
// origem cobre com o sentinel de bypass em qualquer functionCall sem assinatura
// (issue #1410, que supersedeu a #725 do backfill só no primeiro).
func TestAgFixContentsBackfillsSignatureOnEveryFunctionCall(t *testing.T) {
	contents := []any{
		map[string]any{"role": "user", "parts": []any{map[string]any{"text": "read the file"}}},
		map[string]any{"role": "model", "parts": []any{map[string]any{"functionCall": map[string]any{"name": "read", "args": map[string]any{"path": "teste.py"}}}}},
		map[string]any{"role": "user", "parts": []any{map[string]any{"functionResponse": map[string]any{"id": "call_1", "name": "read", "response": map[string]any{"result": "def soma(a, b):"}}}}},
		map[string]any{"role": "model", "parts": []any{map[string]any{"functionCall": map[string]any{"name": "edit", "args": map[string]any{"path": "teste.py"}}}}},
		map[string]any{"role": "user", "parts": []any{map[string]any{"functionResponse": map[string]any{"id": "call_2", "name": "edit", "response": map[string]any{"result": "ok"}}}}},
	}
	out := agFixContents(jsonRoundTrip(contents))
	calls := []string{}
	for _, item := range out {
		content, _ := item.(map[string]any)
		parts, _ := content["parts"].([]any)
		for _, rawPart := range parts {
			part, _ := rawPart.(map[string]any)
			if call, ok := part["functionCall"].(map[string]any); ok {
				signature, _ := part["thoughtSignature"].(string)
				calls = append(calls, ttsString(call["name"])+"="+signature)
			}
		}
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 functionCalls, got %d: %v", len(calls), calls)
	}
	if calls[0] != "read="+agDefaultSignature {
		t.Fatalf("first functionCall must keep default signature, got %v", calls[0])
	}
	if calls[1] != "edit="+agSignatureBypass {
		t.Fatalf("subsequent functionCall must get bypass sentinel, got %v", calls[1])
	}
	// Assinatura real do cliente nunca é sobrescrita.
	contents = []any{
		map[string]any{"role": "model", "parts": []any{map[string]any{"thoughtSignature": "real-sig", "functionCall": map[string]any{"name": "read", "args": map[string]any{}}}}},
		map[string]any{"role": "model", "parts": []any{map[string]any{"functionCall": map[string]any{"name": "edit", "args": map[string]any{}}}}},
	}
	out = agFixContents(jsonRoundTrip(contents))
	signatures := []string{}
	for _, item := range out {
		content, _ := item.(map[string]any)
		parts, _ := content["parts"].([]any)
		for _, rawPart := range parts {
			part, _ := rawPart.(map[string]any)
			if _, ok := part["functionCall"]; ok {
				signature, _ := part["thoughtSignature"].(string)
				signatures = append(signatures, signature)
			}
		}
	}
	if len(signatures) != 2 || signatures[0] != "real-sig" || signatures[1] != agSignatureBypass {
		t.Fatalf("real signature must be preserved: %v", signatures)
	}
	if !strings.Contains(agSignatureBypass, "skip_thought_signature_validator") {
		t.Fatal("sentinel value drifted from upstream contract")
	}
}

// jsonRoundTrip simula o caminho real: contents chegam do JSON do cliente
// (numbers viram float64, maps viram map[string]any).
func jsonRoundTrip(value any) any {
	encoded, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var out any
	if err := json.Unmarshal(encoded, &out); err != nil {
		return value
	}
	return out
}
