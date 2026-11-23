package handlers

import (
	"context"
	"net/http"
	"strings"

	"github.com/zanonaa/VansGo/backend/internal/database"
	"github.com/zanonaa/VansGo/backend/internal/providers"
)

type comboConnectionKey struct{}

// comboConnectionFor fixa a conexão que o candidato do combo deve usar.
// chatCombo expande cada model em N candidatos (um por conexão ativa do
// provider, em ordem de prioridade); sem fixação (chamada direta), cada
// executor mantém o comportamento de "primeira conexão ativa".
func comboConnectionFor(ctx context.Context, provider string, connections []map[string]any) map[string]any {
	if fixed, ok := ctx.Value(comboConnectionKey{}).(map[string]any); ok && ttsString(fixed["provider"]) == provider {
		return fixed
	}
	for _, candidate := range connections {
		if candidate["isActive"] != false {
			return candidate
		}
	}
	return nil
}

// expandComboConnections replica cada model uma vez por conexão ativa do
// provider: o fallback do combo esgota todas as contas antes de saltar para
// o próximo model (ex.: 3 codex, 2 glm, 5 deepseek → 10 tentativas).
// Devolve também, por model, as conexões ativas em ordem de prioridade.
func (g *Gateway) expandComboConnections(r *http.Request, models []string) ([]string, map[string][]map[string]any, error) {
	byProvider := map[string][]map[string]any{}
	expanded := make([]string, 0, len(models))
	for _, model := range models {
		prefix := ""
		if parts := strings.SplitN(model, "/", 2); len(parts) == 2 {
			prefix = parts[0]
		}
		provider := prefix
		if resolved, ok := providers.AliasMap()[prefix]; ok && prefix != "" {
			provider = resolved
		}
		if provider == "" {
			expanded = append(expanded, model)
			continue
		}
		connections, ok := byProvider[provider]
		if !ok {
			var err error
			connections, err = (database.Connections{DB: g.DB}).List(r.Context(), provider, nil)
			if err != nil {
				return nil, nil, err
			}
			byProvider[provider] = connections
		}
		active := 0
		for _, connection := range connections {
			if connection["isActive"] != false {
				expanded = append(expanded, model)
				active++
			}
		}
		// Sem conexão ativa o model entra uma vez; o executor reporta o
		// "No active connection" original e o fallback segue.
		if active == 0 {
			expanded = append(expanded, model)
		}
	}
	return expanded, byProvider, nil
}

// comboConnectionCandidate devolve a conexão da ocorrência: a 1ª repetição do
// model usa a 1ª conexão ativa, a 2ª repetição a 2ª, e assim por diante.
func (g *Gateway) comboConnectionCandidate(r *http.Request, model string, occurrences int, byProvider map[string][]map[string]any) map[string]any {
	if occurrences < 1 {
		return nil
	}
	prefix := ""
	if parts := strings.SplitN(model, "/", 2); len(parts) == 2 {
		prefix = parts[0]
	}
	provider := prefix
	if resolved, ok := providers.AliasMap()[prefix]; ok && prefix != "" {
		provider = resolved
	}
	var active []map[string]any
	for _, connection := range byProvider[provider] {
		if connection["isActive"] != false {
			active = append(active, connection)
		}
	}
	if len(active) == 0 {
		return nil
	}
	return active[(occurrences-1)%len(active)]
}
