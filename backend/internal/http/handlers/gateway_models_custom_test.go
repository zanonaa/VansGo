package handlers

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/zanonaa/VansGo/backend/internal/database"
)

// CustomModels importados via dashboard (kv scope='customModels') precisam
// aparecer no /v1/models com a mesma ACL do chat: provider com conexão ativa
// e providerAllowed (id/alias/prefixo), como em allowedModels.js da origem.
func TestGatewayModelsListsCustomModels(t *testing.T) {
	g := gatewayModelsFixture(t)
	customs := []map[string]any{
		{"providerAlias": "openai", "id": "custom-live", "type": "llm", "name": "Custom Live"},
		{"providerAlias": "cx", "id": "custom-no-connection", "type": "llm", "name": "No Connection"},
		{"providerAlias": "openai", "id": "custom-embedding", "type": "embedding", "name": "Embedding"},
	}
	for _, item := range customs {
		encoded, _ := json.Marshal(item)
		if _, err := g.DB.Exec(`INSERT INTO kv(scope,key,value) VALUES('customModels', ?, ?)`, ttsString(item["providerAlias"])+"|"+ttsString(item["id"]), string(encoded)); err != nil {
			t.Fatal(err)
		}
	}
	key, err := database.CreateApiKey(context.Background(), g.DB, "test", "machine")
	if err != nil {
		t.Fatal(err)
	}
	list := func() map[string]map[string]any {
		request := httptest.NewRequest("GET", "/api/v1/models", nil)
		request.Header.Set("Authorization", "Bearer "+ttsString(key["key"]))
		w := httptest.NewRecorder()
		g.Models(w, request)
		if w.Code != 200 {
			t.Fatalf("models %d: %s", w.Code, w.Body.String())
		}
		var body struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		out := map[string]map[string]any{}
		for _, entry := range body.Data {
			out[ttsString(entry["id"])] = entry
		}
		return out
	}

	all := list()
	entry, ok := all["openai/custom-live"]
	if !ok {
		t.Fatalf("custom de provider com conexão ausente: %v", all)
	}
	if entry["name"] != "Custom Live" || entry["owned_by"] != "openai/custom-live" {
		t.Fatalf("shape inesperado: %v", entry)
	}
	for _, blocked := range []string{"cx/custom-no-connection", "openai/custom-embedding"} {
		if _, ok := all[blocked]; ok {
			t.Fatalf("custom indevido listado: %s", blocked)
		}
	}

	// ACL de provider vazio nega todos os customs, como no chat.
	if _, err := database.UpdateApiKey(context.Background(), g.DB, ttsString(key["id"]), map[string]any{"allowedProviders": []any{}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := list()["openai/custom-live"]; ok {
		t.Fatal("ACL vazia expôs custom")
	}
}
