package handlers

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
)

func TestKimiWebFrameRoundTrip(t *testing.T) {
	body := map[string]any{"chat_id": "", "scenario": "SCENARIO_CHAT"}
	framed := kimiWebFrame(body)
	if framed[0] != 0 {
		t.Fatalf("flags deve ser 0, veio %d", framed[0])
	}
	length := binary.BigEndian.Uint32(framed[1:5])
	if int(length) != len(framed)-5 {
		t.Fatalf("comprimento do frame %d não bate com payload %d", length, len(framed)-5)
	}
	var decoded map[string]any
	if err := json.Unmarshal(framed[5:], &decoded); err != nil {
		t.Fatalf("payload deve ser JSON válido: %v", err)
	}
}

func TestKimiWebDeltaExtraction(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		text    string
		kind    string
		ok      bool
	}{
		{"append text", `{"op":"append","mask":"block.text.content","block":{"text":{"content":"ola"}}}`, "ola", "text", true},
		{"set text", `{"op":"set","mask":"block.text","block":{"text":{"content":"oi"}}}`, "oi", "text", true},
		{"append think", `{"op":"append","mask":"block.think.content","block":{"think":{"content":"hmm"}}}`, "hmm", "think", true},
		{"set think", `{"op":"set","mask":"block.think","block":{"think":{"content":"h"}}}`, "h", "think", true},
		{"heartbeat", `{"heartbeat":{}}`, "", "", false},
		{"chat metadata", `{"op":"set","mask":"chat.name","chat":{"name":"x"}}`, "", "", false},
		{"stage", `{"op":"set","mask":"block.stage","block":{"stage":{}}}`, "", "", false},
		{"empty append", `{"op":"append","mask":"block.text.content","block":{"text":{"content":""}}}`, "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text, kind, ok := kimiWebDelta([]byte(c.payload))
			if ok != c.ok || text != c.text || (ok && kind != c.kind) {
				t.Fatalf("kimiWebDelta(%s) = (%q,%q,%v), queria (%q,%q,%v)", c.payload, text, kind, ok, c.text, c.kind, c.ok)
			}
		})
	}
}

func TestKimiWebAccumulateEndStreamError(t *testing.T) {
	payload := kimiWebFrame(map[string]any{"ignored": true})
	// EndStream com erro embutido
	endError := kimiWebFrame(map[string]any{})
	endError[0] = 0x02
	stream := append(payload, endError...)
	if _, _, err := kimiWebAccumulate(bufio.NewReader(bytes.NewReader(stream))); err != nil {
		t.Fatalf("EndStream vazio não deve falhar: %v", err)
	}
}

func TestKimiWebAccumulateWithoutEndStreamFails(t *testing.T) {
	if _, _, err := kimiWebAccumulate(bufio.NewReader(bytes.NewReader(kimiWebFrame(map[string]any{})))); err == nil {
		t.Fatal("stream sem EndStream deve falhar")
	}
}

func TestKimiWebFoldMessages(t *testing.T) {
	messages := []any{
		map[string]any{"role": "system", "content": "Seja breve"},
		map[string]any{"role": "user", "content": "primeira"},
		map[string]any{"role": "assistant", "content": "resposta"},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "última"}}},
	}
	prompt, system, err := kimiWebFoldMessages(messages)
	if err != nil {
		t.Fatalf("fold falhou: %v", err)
	}
	if system != "Seja breve" {
		t.Fatalf("system_prompt errado: %q", system)
	}
	if prompt != "primeira\n\nAssistant: resposta\n\núltima" {
		t.Fatalf("prompt dobrado errado: %q", prompt)
	}
}

func TestKimiWebFoldMessagesToolResult(t *testing.T) {
	messages := []any{
		map[string]any{"role": "user", "content": "liste arquivos"},
		map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "call_0", "function": map[string]any{"name": "list_files", "arguments": "{}"}}}, "content": ""},
		map[string]any{"role": "tool", "tool_call_id": "call_0", "content": "a.txt\nb.txt"},
	}
	prompt, _, err := kimiWebFoldMessages(messages)
	if err != nil {
		t.Fatalf("fold com tool result falhou: %v", err)
	}
	if !strings.Contains(prompt, "Assistant: invoked tools") || !strings.Contains(prompt, "a.txt") {
		t.Fatalf("tool result não foi dobrado: %q", prompt)
	}
}
