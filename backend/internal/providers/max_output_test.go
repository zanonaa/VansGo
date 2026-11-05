package providers

import "testing"

func TestClampMaxOutputTokens(t *testing.T) {
	if got := ClampMaxOutputTokens("glm", float64(256000)); got != float64(131072) {
		t.Fatalf("glm deve clampar 256000 para 131072, veio %v", got)
	}
	if got := ClampMaxOutputTokens("glm", float64(50000)); got != float64(50000) {
		t.Fatalf("abaixo do teto nao deve mexer, veio %v", got)
	}
	if got := ClampMaxOutputTokens("glm", nil); got != nil {
		t.Fatalf("sem max_tokens nao deve inventar valor, veio %v", got)
	}
	if got := ClampMaxOutputTokens("deepseek", float64(256000)); got != float64(256000) {
		t.Fatalf("provider sem teto nao deve mexer, veio %v", got)
	}
	if got := ClampMaxOutputTokens("glm-cn", 200000); got != float64(131072) {
		t.Fatalf("glm-cn deve clampar, veio %v", got)
	}
}
