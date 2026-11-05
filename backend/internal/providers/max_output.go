package providers

import (
	_ "embed"
	"encoding/json"
)

//go:embed max-output.json
var maxOutputJSON []byte

// ClampMaxOutputTokens limita max_tokens ao teto do provider antes do envio.
// O opencode pede 256000 (contexto 1M) e upstreams como a z.ai rejeitam
// max_tokens > 131072 com 400 (erro 1210) em TODA request, derrubando o
// modelo pro próximo do combo sem que pareça erro de configuração.
// Atualizado por catálogo (max-output.json); sem entrada = sem clamp.
func ClampMaxOutputTokens(provider string, value any) any {
	var caps map[string]int
	if err := json.Unmarshal(maxOutputJSON, &caps); err != nil {
		panic(err)
	}
	cap, ok := caps[provider]
	if !ok {
		return value
	}
	number := pxpipeNumLike(value)
	if number <= 0 || number <= float64(cap) {
		return value
	}
	return float64(cap)
}

func pxpipeNumLike(value any) float64 {
	switch n := value.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		v, _ := n.Float64()
		return v
	}
	return 0
}
