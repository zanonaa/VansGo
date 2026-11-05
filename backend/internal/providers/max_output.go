package providers

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

//go:embed max-output.json
var maxOutputJSON []byte

// maxOutputEntry é o recorte do snapshot model-catalog.json usado aqui:
// providers -> modelo -> maxOutput.
type maxOutputSnapshot struct {
	sync.Mutex
	loaded  time.Time
	builtin map[string]int
	limits  map[string]map[string]float64
}

var maxOutputState = &maxOutputSnapshot{}

// providerUpstreamAliases mapeia provider local -> upstream no models.dev
// (mesma tabela que o sync usa; duplicada aqui porque vive em outro pacote).
var providerUpstreamAliases = map[string]string{
	"glm": "zai", "glm-cn": "zhipuai",
}

// maxOutputLimit devolve o teto de saída conhecido para (provider, model):
// primeiro o snapshot sincronizado do models.dev, depois o embutido.
func maxOutputLimit(provider, model string) float64 {
	maxOutputState.Lock()
	defer maxOutputState.Unlock()
	if time.Since(maxOutputState.loaded) > time.Minute || maxOutputState.limits == nil {
		maxOutputState.reloadLocked()
	}
	if group, ok := maxOutputState.limits[provider]; ok {
		if limit, ok := group[catalogModelKey(model)]; ok && limit > 0 {
			return limit
		}
	}
	if cap, ok := maxOutputState.builtin[provider]; ok && cap > 0 {
		return float64(cap)
	}
	return 0
}

func (s *maxOutputSnapshot) reloadLocked() {
	if s.builtin == nil {
		s.builtin = map[string]int{}
		if err := json.Unmarshal(maxOutputJSON, &s.builtin); err != nil {
			panic(err)
		}
	}
	s.limits = map[string]map[string]float64{}
	dir := os.Getenv("DATA_DIR")
	if dir == "" {
		return
	}
	raw, err := os.ReadFile(filepath.Join(dir, "model-catalog.json"))
	if err != nil {
		return
	}
	var snapshot struct {
		Providers map[string]map[string]any `json:"providers"`
	}
	if json.Unmarshal(raw, &snapshot) != nil {
		return
	}
	for provider, models := range snapshot.Providers {
		group := map[string]float64{}
		for model, value := range models {
			entry, ok := value.(map[string]any)
			if !ok {
				continue
			}
			if limit := pxpipeNumLike(entry["maxOutput"]); limit > 0 {
				group[catalogModelKey(model)] = limit
			}
		}
		if len(group) > 0 {
			s.limits[provider] = group
		}
	}
	s.loaded = time.Now()
}

func catalogModelKey(model string) string {
	// igual ao catalogBaseId do sync: último segmento após "/", minúsculo,
	// sem sufixo após ":" (ex.: gemini:free -> gemini).
	value := model
	if index := indexByteFromEnd(value, '/'); index >= 0 {
		value = value[index+1:]
	}
	if index := indexByteString(value, ':'); index >= 0 {
		value = value[:index]
	}
	return lower(value)
}

func indexByteFromEnd(s string, c byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func indexByteString(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func lower(s string) string {
	out := []byte(s)
	for i := range out {
		if out[i] >= 'A' && out[i] <= 'Z' {
			out[i] += 'a' - 'A'
		}
	}
	return string(out)
}

// ClampMaxOutputTokens ajusta o limite de saída ao teto real do modelo antes
// do envio. Regra única para todos os providers:
//   - cliente pediu mais que o teto  -> envia o teto (z.ai recusa >131072);
//   - cliente não pediu nada         -> envia o teto (nunca inventa 1024);
//   - sem teto conhecido             -> devolve nil (campo omitido).
//
// O teto vem do catálogo sincronizado do models.dev (auto-atualizado) com
// fallback embutido (max-output.json). Nenhum provider é hardcoded aqui.
func ClampMaxOutputTokens(provider, model string, value any) any {
	if alias, ok := providerUpstreamAliases[provider]; ok {
		if maxOutputLimit(alias, model) > 0 {
			provider = alias
		}
	}
	limit := maxOutputLimit(provider, model)
	if limit <= 0 {
		return nil
	}
	number := pxpipeNumLike(value)
	if number > 0 && number <= limit {
		return number
	}
	return limit
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
