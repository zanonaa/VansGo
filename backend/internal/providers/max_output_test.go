package providers

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClampMaxOutputTokens(t *testing.T) {
	// builtin cobre glm mesmo sem snapshot (z.ai 131072)
	if got := ClampMaxOutputTokens("glm", "glm-5.3-flash", float64(256000)); got != float64(131072) {
		t.Fatalf("glm deve clampar 256000 para 131072, veio %v", got)
	}
	if got := ClampMaxOutputTokens("glm", "glm-5.3-flash", float64(50000)); got != float64(50000) {
		t.Fatalf("abaixo do teto nao deve mexer, veio %v", got)
	}
	// sem pedido do cliente: devolve o teto (nunca inventa 1024)
	if got := ClampMaxOutputTokens("glm", "glm-5.3-flash", nil); got != float64(131072) {
		t.Fatalf("sem max_tokens deve devolver o teto, veio %v", got)
	}
	// provider sem teto conhecido: nil (campo omitido, upstream decide)
	if got := ClampMaxOutputTokens("desconhecido", "modelo-x", float64(256000)); got != nil {
		t.Fatalf("provider sem teto nao deve mexer, veio %v", got)
	}
}

// O snapshot do models.dev alimenta os tetos sem deploy (DATA_DIR/model-catalog.json).
func TestClampUsesCatalogSnapshot(t *testing.T) {
	dir := t.TempDir()
	snapshot := `{"providers":{"kimi":{"kimi-k2.7-code":{"maxOutput":262144}}}}`
	if err := os.WriteFile(filepath.Join(dir, "model-catalog.json"), []byte(snapshot), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATA_DIR", dir)
	maxOutputState.loaded = maxOutputState.loaded.Add(-2 * timeMinute) // expira cache
	if got := ClampMaxOutputTokens("kimi", "kimi-k2.7-code", float64(300000)); got != float64(262144) {
		t.Fatalf("snapshot deve clampar kimi a 262144, veio %v", got)
	}
	// alias: glm resolve no snapshot como zai quando existe
	snapshotZai := `{"providers":{"zai":{"glm-5.3":{"maxOutput":131072}}}}`
	if err := os.WriteFile(filepath.Join(dir, "model-catalog.json"), []byte(snapshotZai), 0o644); err != nil {
		t.Fatal(err)
	}
	maxOutputState.loaded = maxOutputState.loaded.Add(-2 * timeMinute)
	if got := ClampMaxOutputTokens("glm", "glm-5.3", float64(999999)); got != float64(131072) {
		t.Fatalf("alias glm->zai deve clampar, veio %v", got)
	}
}

const timeMinute = 60e9 // ns
