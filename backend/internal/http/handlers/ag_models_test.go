package handlers

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// agDiscoveryPayload espelha o payload real do v1internal:fetchAvailableModels
// (POST no daily), em versão mínima: sorts oficiais, família tiered, buckets
// de helpers do IDE e um model fora de qualquer lista (vai pro fim, alfabético).
func agDiscoveryPayload() map[string]any {
	return map[string]any{
		"models": map[string]any{
			"claude-sonnet-5-5-medium": map[string]any{"displayName": "Claude Sonnet 5.5 (Medium)", "maxTokens": 1000000.0, "maxOutputTokens": 128000.0, "recommended": true},
			"claude-sonnet-5-5-low":    map[string]any{"displayName": "Claude Sonnet 5.5 (Low)", "maxTokens": 1000000.0, "maxOutputTokens": 128000.0},
			"claude-sonnet-5-5-high":   map[string]any{"displayName": "Claude Sonnet 5.5 (High)", "maxTokens": 1000000.0, "maxOutputTokens": 128000.0},
			"gemini-3.8-flash-tiered":  map[string]any{"maxTokens": 1048576.0, "maxOutputTokens": 65536.0, "recommended": true},
			"gpt-oss-120b-medium":      map[string]any{"displayName": "GPT-OSS 120B (Medium)", "maxTokens": 131072.0, "maxOutputTokens": 32768.0},
			"gemini-2.5-flash":         map[string]any{"displayName": "Gemini 2.5 Flash", "maxTokens": 1048576.0, "maxOutputTokens": 65535.0},
			"gemini-3.5-flash-lite":    map[string]any{"displayName": "Gemini 3.5 Flash Lite"},
			"gemini-3.1-flash-image":   map[string]any{"displayName": "Gemini 3.1 Flash (Image)"},
			"tab_flash_lite_preview":   map[string]any{"maxOutputTokens": 4096.0},
		},
		"tieredModelIds": map[string]any{"flash": []any{"gemini-3.8-flash-tiered"}},
		"agentModelSorts": []any{map[string]any{"displayName": "Recommended", "groups": []any{map[string]any{
			"modelIds": []any{"claude-sonnet-5-5-medium", "claude-sonnet-5-5-low", "claude-sonnet-5-5-high", "gpt-oss-120b-medium"},
		}}}},
		"mqueryModelIds":             []any{"gemini-3.5-flash-lite"},
		"webSearchModelIds":          []any{"gemini-3.5-flash-lite"},
		"commitMessageModelIds":      []any{"gemini-3.5-flash-lite"},
		"audioTranscriptionModelIds": []any{"models/proactive-observer-v10"},
		"imageGenerationModelIds":    []any{"gemini-3.1-flash-image"},
		"commandModelIds":            []any{"gemini-3-flash"},
	}
}

// agModelsTransport roteia fetchAvailableModels (discovery dinâmico) e
// repassa o restante pro handler de chat do teste. fetched conta os fetches.
func agModelsTransport(t *testing.T, chat func(*http.Request) (*http.Response, error), fetched *int) kiroImportTransport {
	encoded, err := json.Marshal(agDiscoveryPayload())
	if err != nil {
		t.Fatal(err)
	}
	return kiroImportTransport(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.String(), "fetchAvailableModels") {
			if r.Method != http.MethodPost {
				t.Fatalf("discovery method: %s", r.Method)
			}
			if r.Header.Get("Authorization") != "Bearer oauth-token" || r.Header.Get("User-Agent") != "antigravity/ide/2.3.0 darwin/arm64" {
				t.Fatalf("discovery headers: %v", r.Header)
			}
			if fetched != nil {
				*fetched++
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(encoded)))}, nil
		}
		return chat(r)
	})
}

func TestAGParseAvailableModels(t *testing.T) {
	entry := agParseAvailableModels(agDiscoveryPayload())
	wantIDs := []string{
		"claude-sonnet-5-5-medium", "claude-sonnet-5-5-low", "claude-sonnet-5-5-high", "gpt-oss-120b-medium",
		"gemini-3.8-flash-tiered-low", "gemini-3.8-flash-tiered-medium", "gemini-3.8-flash-tiered-high",
		"gemini-2.5-flash",
	}
	if !reflect.DeepEqual(entry.ids, wantIDs) {
		t.Fatalf("ids: %v", entry.ids)
	}
	// Fanout tiered aponta pro id base; os demais passam verbatim.
	if entry.upstream["gemini-3.8-flash-tiered-high"] != "gemini-3.8-flash-tiered" || entry.upstream["claude-sonnet-5-5-medium"] != "claude-sonnet-5-5-medium" {
		t.Fatalf("upstream: %v", entry.upstream)
	}
	// Helpers (mquery/image/tab) nunca entram no chat LLM.
	for _, model := range entry.models {
		id := model["id"].(string)
		if strings.Contains(id, "lite") || strings.Contains(id, "image") || strings.Contains(id, "tab_") {
			t.Fatalf("helper vazou no listing: %s", id)
		}
	}
	byID := map[string]map[string]any{}
	for _, model := range entry.models {
		byID[model["id"].(string)] = model
	}
	// Metadados de contexto/output vindos do payload; tiered base tem displayName
	// vazio, então o nome é o id público + nível capitalizado.
	if byID["ag/claude-sonnet-5-5-medium"]["context_length"] != 1000000.0 || byID["ag/claude-sonnet-5-5-medium"]["max_output_tokens"] != 128000.0 {
		t.Fatalf("metadados: %v", byID["ag/claude-sonnet-5-5-medium"])
	}
	if byID["ag/gemini-3.8-flash-tiered-high"]["name"] != "gemini-3.8-flash-tiered-high (High)" {
		t.Fatalf("nome tiered: %v", byID["ag/gemini-3.8-flash-tiered-high"]["name"])
	}
	if byID["ag/gemini-3.8-flash-tiered-high"]["upstream"] != "gemini-3.8-flash-tiered" {
		t.Fatalf("upstream tiered: %v", byID["ag/gemini-3.8-flash-tiered-high"])
	}
}
