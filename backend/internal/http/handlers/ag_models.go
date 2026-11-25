package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zanonaa/VansGo/backend/internal/database"
)

// Catálogo 100% dinâmico do antigravity: o listing vem do próprio upstream
// (v1internal:fetchAvailableModels — POST no host daily; o endpoint responde
// 404 em GET). Nada de snapshot embutido: model novo que o Google anunciar
// aparece no /v1/models e responde no chat sem deploy. A ordenação oficial é
// a do IDE (agentModelSorts, depois alfabética); famílias tiered
// (tieredModelIds) viram três ids -low/-medium/-high mapeados pro id base;
// helpers do IDE (tab completion, mquery, web search, commit, áudio),
// image-gen e comandos ficam fora do chat LLM.
const (
	agModelsURL     = "https://daily-cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels"
	agModelsTTL     = 5 * time.Minute
	agModelsTimeout = 15 * time.Second
)

var (
	agModelsMu     sync.Mutex
	agModelsCache  = map[string]agModelsEntry{}
	agModelsClient = &http.Client{Timeout: agModelsTimeout}
	// Buckets de models auxiliares do IDE, nunca expostos como chat LLM.
	agHelperBuckets = []string{
		"mqueryModelIds", "webSearchModelIds", "commitMessageModelIds",
		"audioTranscriptionModelIds", "commandModelIds",
		"imageGenerationModelIds", "tabCompletionModelIds",
	}
)

type agModelsEntry struct {
	at       time.Time
	models   []map[string]any  // entradas prontas pro /v1/models (id já com prefixo ag/)
	upstream map[string]string // id público ag → id real aceito no wrapper do upstream
	ids      []string          // ids públicos sem prefixo, na ordem oficial do IDE
}

// agUpstreamOverride resolve o id do upstream dinamicamente: aceita qualquer
// id anunciado pelo fetchAvailableModels da conexão (fanout tiered resolve
// pro id base; os demais passam verbatim). Sem o fetch (offline/quota), cai
// no catálogo estático via agUpstreamModel.
func agUpstreamOverride(ctx context.Context, connection map[string]any, model string, client *http.Client) (string, bool) {
	entry, err := agAvailableModels(ctx, connection, client)
	if err != nil {
		return "", false
	}
	upstream, ok := entry.upstream[model]
	return upstream, ok
}

// agAvailableModels busca (com cache por conta) e converte o payload do
// fetchAvailableModels. connection traz accessToken/email para o cache key;
// client nil usa o default com timeout próprio.
func agAvailableModels(ctx context.Context, connection map[string]any, client *http.Client) (agModelsEntry, error) {
	token := firstNonEmptyString(connection["accessToken"], connection["apiKey"])
	cacheKey := firstNonEmptyString(connection["email"], connection["id"])
	agModelsMu.Lock()
	cached, ok := agModelsCache[cacheKey]
	if ok && time.Since(cached.at) < agModelsTTL {
		agModelsMu.Unlock()
		return cached, nil
	}
	agModelsMu.Unlock()
	if token == "" {
		return agModelsEntry{}, fmt.Errorf("antigravity: no access token")
	}
	metadata, _ := connection["providerSpecificData"].(map[string]any)
	project := firstNonEmptyString(metadata["projectId"], metadata["quota_project_id"], connection["projectId"])
	payload, err := agFetchAvailableModels(ctx, token, project, client)
	if err != nil {
		return agModelsEntry{}, err
	}
	entry := agParseAvailableModels(payload)
	if len(entry.ids) == 0 {
		return agModelsEntry{}, fmt.Errorf("antigravity: fetchAvailableModels sem models")
	}
	entry.at = time.Now()
	agModelsMu.Lock()
	agModelsCache[cacheKey] = entry
	agModelsMu.Unlock()
	return entry, nil
}

func agFetchAvailableModels(ctx context.Context, token, project string, client *http.Client) (map[string]any, error) {
	if client == nil {
		client = agModelsClient
	}
	// O gate do discovery são dois fatores (validado contra produção,
	// 2026-11-25): User-Agent do CLI + project no corpo. Mesma conta/token:
	// {"project":"aicode-consumers"} devolve 37 models (família gemini-4
	// inteira); `{}` devolve 21, sem gemini-4.
	body := map[string]any{}
	if project != "" {
		body["project"] = project
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, agModelsURL, strings.NewReader(string(encoded)))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("User-Agent", agUserAgent)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("antigravity: fetchAvailableModels %d", response.StatusCode)
	}
	var payload map[string]any
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return nil, err
	}
	return payload, nil
}

// agParseAvailableModels replica o PROPAGATE do IDE: ids do agente na ordem
// oficial (agentModelSorts), tiered fanout low/medium/high, metadados de
// contexto/output do payload; helper/command/image/internal ficam de fora.
func agParseAvailableModels(payload map[string]any) agModelsEntry {
	models, _ := payload["models"].(map[string]any)
	entry := agModelsEntry{upstream: map[string]string{}}
	if len(models) == 0 {
		return entry
	}
	excluded := map[string]bool{}
	for _, bucket := range agHelperBuckets {
		for _, item := range agStringList(payload[bucket]) {
			excluded[item] = true
		}
	}
	// tieredModelIds: mapa categoria → lista (flash/flashLite/pro...). Cada
	// família tiered vira três ids -low/-medium/-high; o upstream aceita o id
	// base no wrapper (strip -tiered(...) → -tiered). Bases que JÁ terminam
	// num nível (gemini-4-hydrogen-tiered-high/-xhigh do catálogo atual) não
	// podem ganhar fanout: o id base sem nível não existe no upstream (404) —
	// o id completo passa verbatim pelo caminho normal do listing.
	bases := []string{}
	for _, base := range agTieredBases(payload, models, excluded) {
		if agTierLevel(base) == "" {
			bases = append(bases, base)
		}
	}
	fanout := map[string][]string{}
	levels := []string{"low", "medium", "high"}
	for _, base := range bases {
		for _, level := range levels {
			fanout[base] = append(fanout[base], base+"-"+level)
		}
		excluded[base] = true
	}
	order := []string{}
	for _, sortEntry := range payload["agentModelSorts"].([]any) {
		group, _ := sortEntry.(map[string]any)
		for _, groupEntry := range group["groups"].([]any) {
			groupModel, _ := groupEntry.(map[string]any)
			order = append(order, agStringList(groupModel["modelIds"])...)
		}
	}
	for _, name := range order {
		if models[name] != nil && !excluded[name] {
			excluded[name] = true
			entry.ids = append(entry.ids, name)
		}
	}
	// Fanouts em ordem fixa (bases ordenadas), nunca na ordem do mapa.
	sort.Strings(bases)
	for _, base := range bases {
		entry.ids = append(entry.ids, fanout[base]...)
	}
	remaining := []string{}
	for name := range models {
		if !excluded[name] && !agExcludeID(name) {
			remaining = append(remaining, name)
		}
	}
	sort.Strings(remaining)
	entry.ids = append(entry.ids, remaining...)
	entry.models, entry.upstream = agConvertModels(models, fanout, entry.ids)
	return entry
}

// agTieredBases coleta e ordena as famílias tiered de tieredModelIds (mapa
// categoria → lista na resposta real; lista simples aceita por robustez),
// descartando helpers e ids ausentes do payload.
func agTieredBases(payload map[string]any, models map[string]any, excluded map[string]bool) []string {
	bases := []string{}
	switch value := payload["tieredModelIds"].(type) {
	case []any:
		bases = agStringList(value)
	case map[string]any:
		seen := map[string]bool{}
		for _, list := range value {
			for _, id := range agStringList(list) {
				if !seen[id] {
					seen[id] = true
					bases = append(bases, id)
				}
			}
		}
	}
	kept := []string{}
	for _, base := range bases {
		if models[base] != nil && !excluded[base] && !agExcludeID(base) {
			kept = append(kept, base)
		}
	}
	return kept
}

func agConvertModels(models map[string]any, fanout map[string][]string, ids []string) ([]map[string]any, map[string]string) {
	list, upstream := []map[string]any{}, map[string]string{}
	for _, public := range ids {
		base := public
		for fanBase, fanIDs := range fanout {
			for _, item := range fanIDs {
				if item == public {
					base = fanBase
				}
			}
		}
		meta, _ := models[base].(map[string]any)
		entry := map[string]any{
			"id":       "ag/" + public,
			"object":   "model",
			"kind":     "llm",
			"owned_by": "ag",
			"endpoint": "/v1/chat/completions",
			"provider": "antigravity",
			"name":     agDisplayName(public, meta),
			"upstream": base,
		}
		if value := agNumber(meta["maxTokens"]); value > 0 {
			entry["context_length"] = value
		}
		if value := agNumber(meta["maxOutputTokens"]); value > 0 {
			entry["max_output_tokens"] = value
		}
		list = append(list, entry)
		upstream[public] = base
	}
	return list, upstream
}

// agExcludeID descarta os ids de autocompletar do IDE, que não são chat LLM.
func agExcludeID(id string) bool {
	return strings.HasPrefix(id, "tab_") || strings.HasPrefix(id, "models/")
}

// agDisplayName prefere o displayName do upstream; no fanout tiered adiciona
// o sufixo do nível ("Gemini 3.8 Flash (High)"), igual aos nomes do IDE —
// mas só quando o upstream ainda não inclui o nível no displayName (os ids
// claude-* já vêm com "(Low)" do fetchAvailableModels).
func agDisplayName(public string, meta map[string]any) string {
	name := strings.TrimSpace(ttsString(meta["displayName"]))
	if name == "" {
		name = public
	}
	if level := agTierLevel(public); level != "" {
		capitalized := strings.ToUpper(level[:1]) + level[1:]
		if !strings.Contains(strings.ToLower(name), strings.ToLower("("+capitalized+")")) {
			name += " (" + capitalized + ")"
		}
	}
	return name
}

// agTierLevel devolve o nível terminal do id ("low"/"medium"/"high"/"xhigh").
// xhigh existe na família gemini-4 (neon/carbon/argon/hydrogen) e no tiered.
func agTierLevel(public string) string {
	for _, level := range []string{"low", "medium", "high", "xhigh"} {
		if strings.HasSuffix(public, "-"+level) {
			return level
		}
	}
	return ""
}

func agStringList(value any) []string {
	items, _ := value.([]any)
	out := []string{}
	for _, item := range items {
		if text := ttsString(item); text != "" {
			out = append(out, text)
		}
	}
	return out
}

func agNumber(value any) float64 {
	number, _ := value.(float64)
	return number
}

// agActiveConnection devolve a primeira conexão antigravity ativa (o listing
// segue a ACL global da origem: provider com conexão ativa aparece no /v1/models).
func agActiveConnection(r *http.Request, g *Gateway) map[string]any {
	active := true
	connections, err := (database.Connections{DB: g.DB}).List(r.Context(), "antigravity", &active)
	if err != nil {
		return nil
	}
	return comboConnectionFor(r.Context(), "antigravity", connections)
}
