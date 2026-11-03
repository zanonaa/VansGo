package handlers

// Teste individual do combo (POST /api/combos/{id}/test): executa sondas
// vivas de baixo custo ("hi", como o ModelPing) por candidato, na ORDEM da
// estratégia configurada, e devolve diagnóstico por tentativa — é exatamente
// o mesmo probe que o dashboard já usa para validar providers. O Middleware
// geral (sessão/token CLI) basta; nunca é marcado como LocalOnly.
import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

type comboProbeAttempt struct {
	model     string
	status    int
	ok        bool
	latencyMs int64
	error     string
}

func (h Combos) attemptModel(ctx context.Context, model string) comboProbeAttempt {
	out := h.Ping(ctx, model, "llm")
	attempt := comboProbeAttempt{model: model}
	attempt.ok, _ = out["ok"].(bool)
	if raw, ok := out["latencyMs"]; ok {
		switch v := raw.(type) {
		case float64:
			attempt.latencyMs = int64(v)
		case int:
			attempt.latencyMs = int64(v)
		case int64:
			attempt.latencyMs = v
		}
	}
	switch raw := out["status"].(type) {
	case float64:
		attempt.status = int(raw)
	case int:
		attempt.status = raw
	}
	attempt.error = ttsString(out["error"])
	return attempt
}

func comboAttemptJSON(a comboProbeAttempt) map[string]any {
	return map[string]any{
		"model":     a.model,
		"ok":        a.ok,
		"status":    a.status,
		"latencyMs": a.latencyMs,
		"error":     a.error,
	}
}

// ComboTest valida um combo executando a estratégia definida na configuração
// (global `comboStrategy` com override por combo) e devolve resultado por
// tentativa. Chamadas pequenas e reais ao upstream — sem necessidade de token
// de câmbio (é o ModelPing).
func (h Combos) Test(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	ctx := r.Context()
	if h.Ping == nil || h.DB == nil {
		nodeReply(w, 503, map[string]any{"error": "Combo test not configured", "code": "NOT_CONFIGURED"})
		return
	}
	id := chi.URLParam(r, "id")
	combo, err := h.Store.Get(ctx, id)
	if err != nil {
		nodeReply(w, 500, map[string]any{"error": "Failed to fetch combo"})
		return
	}
	if combo == nil {
		nodeReply(w, 404, map[string]any{"error": "Combo not found"})
		return
	}
	name, _ := combo["name"].(string)
	if kind := ttsString(combo["kind"]); kind != "" && kind != "llm" {
		nodeReply(w, 400, map[string]any{"error": "Combo test only supports LLM combos", "code": "UNSUPPORTED_KIND"})
		return
	}
	entries, _ := combo["models"].([]any)
	settings := map[string]any{}
	if h.Read != nil {
		if value, err := h.Read(ctx); err == nil && value != nil {
			settings = value
		}
	}
	strategy := comboStrategy(settings, name)
	candidates, err := expandComboCandidates(ctx, h.DB, name, entries)
	if err != nil {
		nodeReply(w, 400, map[string]any{"error": err.Error(), "code": "EXPANSION_FAILED"})
		return
	}
	if len(candidates) == 0 {
		nodeReply(w, 400, map[string]any{"error": "No models configured for combo: " + name, "code": "EMPTY_COMBO"})
		return
	}

	attempts := []map[string]any{}
	ok := false
	stopped := false // parada legítima da estratégia (pedido não retentável)
	var decisive comboProbeAttempt
	if strategy == "fusion" {
		// Fusion executa N+1 chamadas dentro do próprio gateway; uma sonda
		// através dele exerce exatamente esse caminho.
		decisive = h.attemptModel(ctx, name)
		attempts = append(attempts, comboAttemptJSON(decisive))
		ok = decisive.ok
	} else {
		roundRobin := strategy == "round-robin"
		for _, model := range candidates {
			decisive = h.attemptModel(ctx, model)
			attempts = append(attempts, comboAttemptJSON(decisive))
			if decisive.ok {
				ok = true
				break
			}
			if !roundRobin && !comboShouldFallback(decisive.status, decisive.error) {
				stopped = true
				break
			}
		}
	}

	out := map[string]any{
		"ok":         ok,
		"combo":      name,
		"strategy":   strategy,
		"models":     candidates,
		"attempts":   attempts,
		"stoppedBy":  stopped,
		"durationMs": time.Since(started).Milliseconds(),
		"testedAt":   time.Now().UTC().Format(time.RFC3339Nano),
	}
	if !ok {
		if decisive.error != "" {
			out["error"] = decisive.error
		} else {
			out["error"] = "All candidates failed"
		}
	}
	nodeReply(w, 200, out)
}
