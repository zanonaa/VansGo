package handlers

import (
	"runtime"
)

// Perfil de cliente CLI do Antigravity (porte do OmniRoute —
// open-sse/services/antigravityHeaders.ts): o User-Agent do CLI `agy` está na
// allowlist dinâmica do upstream tanto pro discovery
// (v1internal:fetchAvailableModels) quanto pro chat (streamGenerateContent), e
// é ele que libera o catálogo completo — família gemini-4 inteira (neon,
// carbon, argon, hydrogen, com xhigh e tiered-xhigh) além de claude-opus/sonnet-5-5.
// O UA do IDE standalone (antigravity/<versão> <os>/<arch>) ainda é aceito no
// chat, mas corta o catálogo pra ~21 models. Validado contra produção
// (mesma conta/token/client, 2026-11-25): UA CLI + {"project":...} → 37 models.
//
// Formato capturado do binário agy 1.5.17 (é um binário Go: os_type/arch vêm
// de runtime.GOOS/GOARCH, com os mesmos tokens linux/darwin/windows e
// amd64/arm64). O `cl=` do UA é o changelist Piper interno do build — só é
// conhecido pra versões antigas (1.4.4 = 1003595037) e o OmniRoute omite
// quando desconhecido: changelist velho em UA novo é pior que nenhum.
const agCliVersion = "1.5.17"

func agCliUserAgent() string {
	return "antigravity/cli/" + agCliVersion +
		" (aidev_client; os_type=" + runtime.GOOS +
		"; arch=" + runtime.GOARCH + "; auth_method=consumer)"
}
