package handlers

// Teste de integração do parser contra o payload REAL capturado do
// fetchAvailableModels (UA CLI + project, 2026-11-25): 37 models incluindo a
// família gemini-4 (neon/carbon/argon/hydrogen com xhigh e tiered-nível) e
// sonar. Ignorado por default pra não depender de arquivo externo; rode com
// go test -run TestAGParserRealPayload
import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestAGParserRealPayload(t *testing.T) {
	raw, err := os.ReadFile("/tmp/opencode/fam-0.json")
	if err != nil {
		t.Skip("payload real não disponível:", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	entry := agParseAvailableModels(payload)
	t.Logf("total ids: %d", len(entry.ids))
	names := map[string]string{}
	for _, m := range entry.models {
		names[ttsString(m["id"])] = ttsString(m["name"])
	}
	for _, id := range entry.ids {
		t.Logf("  ag/%s -> %s (%s)", id, entry.upstream[id], names["ag/"+id])
	}
	// ids críticos do payload real de 2026-11-25 (37 models, 2 tab_ excluídos,
	// 3 em helper buckets, tiered base vira fanout): família gemini-4 com
	// xhigh onde o upstream anuncia, tiered-nível verbatim e sonar.
	for _, want := range []string{
		"gemini-4-neon-xhigh", "gemini-4-neon-high",
		"gemini-4-carbon-high", "gemini-4-argon-high",
		"gemini-4-hydrogen-xhigh", "gemini-4-hydrogen-sonar",
		"gemini-4-hydrogen-tiered-high", "gemini-4-hydrogen-tiered-xhigh",
		"claude-opus-5-5-high", "gpt-oss-120b-medium",
	} {
		if entry.upstream[want] == "" {
			t.Errorf("id esperado ausente: %s", want)
		}
	}
	// Bases tiered sem nível não podem existir (404 no upstream) e helpers
	// (tab_*, mquery/web search/image — ex. gemini-3-flash, gemini-3.1-flash-
	// image) ficam fora do chat LLM.
	for _, id := range entry.ids {
		if id == "gemini-4-hydrogen-tiered" || id == "gemini-3.8-flash-tiered" {
			t.Errorf("base tiered sem nível vazou pro listing: %s", id)
		}
		if strings.HasPrefix(id, "tab_") || id == "gemini-3-flash" || id == "gemini-3.1-flash-image" {
			t.Errorf("helper vazou: %s", id)
		}
	}
	// upstream do tiered-nível aponta pro próprio id (verbatim).
	if got := entry.upstream["gemini-4-hydrogen-tiered-xhigh"]; got != "gemini-4-hydrogen-tiered-xhigh" {
		t.Errorf("tiered-xhigh upstream: %s", got)
	}
}
