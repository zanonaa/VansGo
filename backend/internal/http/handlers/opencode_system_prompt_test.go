package handlers

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOpenCodeSystemPromptEmbedded(t *testing.T) {
	if !strings.HasPrefix(openCodeSystemPrompt, "You are an AI agent running in OpenCode") {
		t.Fatalf("template embutido inválido: %q", openCodeSystemPrompt[:40])
	}
	if len(openCodeSystemPrompt) < 1000 {
		t.Fatalf("template curto demais: %d bytes", len(openCodeSystemPrompt))
	}
}

func TestOpenCodeInjectSystemPrefix(t *testing.T) {
	template := openCodeSystemPrompt

	t.Run("sem system: injeta na frente", func(t *testing.T) {
		body := map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": "oi"},
		}}
		openCodeInjectSystemPrefix(body)
		encoded, _ := json.Marshal(body["messages"])
		var messages []map[string]any
		if err := json.Unmarshal(encoded, &messages); err != nil {
			t.Fatal(err)
		}
		if len(messages) != 2 || messages[0]["role"] != "system" || messages[0]["content"] != template {
			t.Fatalf("injeção falhou: %s", encoded)
		}
		if messages[1]["role"] != "user" || messages[1]["content"] != "oi" {
			t.Fatalf("user perdido: %s", encoded)
		}
	})

	t.Run("system estranho: template entra como primeiro", func(t *testing.T) {
		body := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
			map[string]any{"role": "user", "content": "oi"},
		}}
		openCodeInjectSystemPrefix(body)
		encoded, _ := json.Marshal(body["messages"])
		var messages []map[string]any
		if err := json.Unmarshal(encoded, &messages); err != nil {
			t.Fatal(err)
		}
		if len(messages) != 3 || messages[0]["content"] != template {
			t.Fatalf("esperado 3 mensagens com template na frente: %s", encoded)
		}
	})

	t.Run("cliente oficial: idempotente", func(t *testing.T) {
		original := []any{
			map[string]any{"role": "system", "content": template + "\n\nextra"},
			map[string]any{"role": "user", "content": "oi"},
		}
		body := map[string]any{"messages": original}
		openCodeInjectSystemPrefix(body)
		if len(body["messages"].([]any)) != 2 {
			t.Fatalf("injetou em cliente oficial: %d mensagens", len(body["messages"].([]any)))
		}
	})

	t.Run("system depois do user: injeta na frente mesmo assim", func(t *testing.T) {
		body := map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": "oi"},
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
		}}
		openCodeInjectSystemPrefix(body)
		messages := body["messages"].([]any)
		if len(messages) != 3 {
			t.Fatalf("esperado 3 mensagens: %d", len(messages))
		}
		first, _ := messages[0].(map[string]any)
		if first["content"] != template {
			t.Fatalf("template não está na primeira posição")
		}
	})
}
