package handlers

// Executor do free tier do OpenCode Zen (porta do contrato do main do 9router
// v0.5.119 — open-sse/executors/opencode.js e utils/opencodeFingerprint.js).
//
//   - User-Agent opencode/latest/2.0.26/cli (versão >= 2; 1.x é recusado).
//   - x-opencode-client: cli; x-opencode-project: global.
//   - 4 headers de sessão com o mesmo id: x-opencode-session, -session-id,
//     x-session-affinity e x-session-id. Nenhum x-opencode-request (v1-only;
//     upstream responde 403).
//   - Session: ses_ + 12 hex + 14 base62; hex descendente ~(ms<<12+c). Uma
//     session estável por identidade (memoizada), porque a quota upstream é
//     contabilizada por session; rejeitada, esquecida para mint de nova.
//   - Corpo: stream forçado true, tools read/shell com definições verbatim do
//     CLI 2.0.26 anexadas quando ausentes, caller tools com nome terminal-gated
//     ou colidente recebem alias e tool_choice é retargetado; bloco <env>
//     anexado ao system (o gate lê texto, não prompt oficial).
//   - TLS é o primeiro gate: ClientHello espelhado do Bun (BoringSSL) via uTLS;
//     Go TLS nativo responde 429 com o mesmo corpo.
//   - Credencial: Bearer public quando a conexão não tem chave.
//   - Cliente que pediu JSON recebe o stream upstream dobrado de volta em um
//     chat.completion único, com os nomes originais das tools restaurados.

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
)

// ---- IDs canônicos (ses_ + 12 hex + 14 base62, hex descendente) ----

const openCodeBase62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func openCodeTimeHex(value uint64) string {
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 12)
	for i := 0; i < 6; i++ {
		byteVal := byte((value >> uint(40-8*i)) & 0xff)
		out[i*2] = hexDigits[byteVal>>4]
		out[i*2+1] = hexDigits[byteVal&0x0f]
	}
	return string(out)
}

func openCodeRandomPart() string {
	raw := make([]byte, 14)
	_, _ = rand.Read(raw)
	out := make([]byte, 14)
	for i, b := range raw {
		out[i] = openCodeBase62[int(b)%62]
	}
	return string(out)
}

// openCodeSessionID minta a session com o prefixo de tempo descendente do CLI
// v2 (counter 1 na primeira mint de um processo; o valor exato do counter só
// desloca o hex, o shape permanece canônico).
func openCodeSessionID() string {
	return openCodeSessionIDCounter(1)
}

func openCodeSessionIDCounter(counter uint64) string {
	ms := uint64(time.Now().UnixMilli())
	const mask48 = (1 << 48) - 1
	return "ses_" + openCodeTimeHex(^(ms<<12+counter)&mask48) + openCodeRandomPart()
}

func openCodePlausibleID(value string) bool {
	if len(value) != 30 || !strings.HasPrefix(value, "ses_") {
		return false
	}
	for _, c := range value[4:16] {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// Sessões estáveis por identidade (quota upstream é por session; mint nova por
// request queima a cota e vira 429 com reset crescente). LRU simples com teto.
var (
	openCodeSessionsMu sync.Mutex
	openCodeSessions   = map[string]*openCodeSessionEntry{}
)

type openCodeSessionEntry struct {
	sessionID string
	lastUsed  time.Time
}

const openCodeMaxSessions = 1000

// stableOpenCodeSession devolve a session estável da identidade, mintando e
// memoizando quando não existe.
func stableOpenCodeSession(identity string) string {
	openCodeSessionsMu.Lock()
	defer openCodeSessionsMu.Unlock()
	if entry, ok := openCodeSessions[identity]; ok {
		entry.lastUsed = time.Now()
		return entry.sessionID
	}
	session := openCodeSessionID()
	if len(openCodeSessions) >= openCodeMaxSessions {
		oldest := ""
		var oldestTime time.Time
		for id, entry := range openCodeSessions {
			if oldest == "" || entry.lastUsed.Before(oldestTime) {
				oldest, oldestTime = id, entry.lastUsed
			}
		}
		delete(openCodeSessions, oldest)
	}
	openCodeSessions[identity] = &openCodeSessionEntry{sessionID: session, lastUsed: time.Now()}
	return session
}

// forgetOpenCodeSession descarta por valor (a request concorrente pode ter
// mintado substituta sob outra chave; só o valor rejeitado sai).
func forgetOpenCodeSession(sessionID string) {
	if sessionID == "" {
		return
	}
	openCodeSessionsMu.Lock()
	defer openCodeSessionsMu.Unlock()
	for id, entry := range openCodeSessions {
		if entry.sessionID == sessionID {
			delete(openCodeSessions, id)
		}
	}
}

// ---- Headers de identidade ----

const openCodeUserAgent = "opencode/latest/2.0.26/cli"

// openCodeHeaders monta os headers de identidade. A session canônica do próprio
// cliente opencode é reaproveitada; caso contrário minta estável por conexão.
// Sem x-opencode-request: o upstream v2 o rejeita com 403.
func openCodeHeaders(client http.Header, token string, connectionID string) map[string]string {
	session := strings.TrimSpace(client.Get("x-opencode-session"))
	if !openCodePlausibleID(session) {
		identity := connectionID
		if identity == "" {
			if auth := client.Get("Authorization"); auth != "" {
				digest := sha1.Sum([]byte(auth))
				identity = "auth:" + hex.EncodeToString(digest[:])[:32]
			} else {
				identity = "default"
			}
		}
		session = stableOpenCodeSession("conn:" + identity)
	}
	headers := map[string]string{
		"Content-Type":          "application/json",
		"User-Agent":            openCodeUserAgent,
		"x-opencode-client":     "cli",
		"x-opencode-project":    "global",
		"x-opencode-session":    session,
		"x-opencode-session-id": session,
		"x-session-affinity":    session,
		"x-session-id":          session,
		"Accept":                "text/event-stream",
	}
	if clientTool := strings.TrimSpace(client.Get("x-opencode-client")); clientTool != "" {
		headers["x-opencode-client"] = clientTool
	}
	if token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	return headers
}

// ---- Tools fingerprint (defs verbatim do CLI 2.0.26; não reformatar) ----

var openCodeFingerprintDefinitions = map[string]map[string]any{
	"read": {
		"description": "Read the contents of a file or directory. Supports text files, images, and PDFs. Images and PDFs are presented directly to the model. Each text line is prefixed by its 1-based line number as <line>: <content>. The prefix is for reference and is not part of the file content. Directory entries are returned one per line. Use offset and limit to read large files or directories in sections. Prefer one larger read over many small slices, and use grep to find specific content in large files.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":   map[string]any{"type": "string", "description": "File or directory to read"},
				"offset": map[string]any{"type": "integer", "minimum": 0, "description": "The line or directory entry to start reading from (1-based)"},
				"limit":  map[string]any{"type": "integer", "minimum": 0, "description": "The maximum number of lines or directory entries to read (defaults to and capped at 2000)"},
			},
			"required":             []string{"path"},
			"additionalProperties": false,
		},
	},
	"shell": {
		"description": "Execute a shell command and return its output. Commands run on Linux using bash. Quote file paths containing spaces or special characters. Prefer dedicated tools over shell commands when possible. When output is large, the full result is saved to a file and a truncated preview is returned. Rely on automatic truncation unless filtering the output is more useful. Commands accept an optional timeout, background commands have no timeout by default. Background commands return immediately, and you will be notified when they complete.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command":    map[string]any{"type": "string", "description": "Shell command string to execute"},
				"workdir":    map[string]any{"type": "string", "description": "Working directory to execute the command in. Defaults to the current working directory. When possible, avoid changing directories in the command and set the working directory here instead."},
				"timeout":    map[string]any{"type": "integer", "minimum": 0, "description": "Timeout in milliseconds. Set to 0 to disable the timeout. Defaults to 120000 for foreground commands. Background commands have no timeout by default."},
				"background": map[string]any{"type": "boolean", "description": "Run the command in the background and return immediately (useful for dev servers and long-running builds). You do not need to use '&' at the end of the command when using this parameter. You will be notified when it completes. DO NOT poll for completion."},
			},
			"required":             []string{"command"},
			"additionalProperties": false,
		},
	},
}

var openCodeFingerprintTools = []string{"read", "shell"}

// Nome com substring "terminal" é recusado pelo gate textual (429) — viaja
// sempre sob alias.
func openCodeGatedToolName(name string) bool {
	return strings.Contains(strings.ToLower(name), "terminal")
}

func openCodeFingerprintKey(name string) string {
	lower := strings.ToLower(strings.TrimSpace(name))
	for _, tool := range openCodeFingerprintTools {
		if tool == lower {
			return lower
		}
	}
	return ""
}

func openCodeToolName(tool map[string]any) string {
	if name := strings.TrimSpace(ttsString(tool["name"])); name != "" {
		return name
	}
	function, _ := tool["function"].(map[string]any)
	if function != nil {
		return strings.TrimSpace(ttsString(function["name"]))
	}
	return ""
}

func openCodeWithToolName(tool map[string]any, name string) map[string]any {
	if function, ok := tool["function"].(map[string]any); ok {
		fn := map[string]any{}
		for k, v := range function {
			fn[k] = v
		}
		fn["name"] = name
		copied := map[string]any{}
		for k, v := range tool {
			copied[k] = v
		}
		copied["function"] = fn
		return copied
	}
	copied := map[string]any{}
	for k, v := range tool {
		copied[k] = v
	}
	copied["name"] = name
	return copied
}

func openCodeToolDefinition(tool map[string]any) map[string]any {
	if function, ok := tool["function"].(map[string]any); ok {
		return function
	}
	return tool
}

// openCodeFingerprintParams compara só os parâmetros (chaves estáveis em Go;
// sem JSON.stringify). Definição idêntica = passthrough.
func openCodeFingerprintParams(def map[string]any, key string) bool {
	want, ok := openCodeFingerprintDefinitions[key]["parameters"].(map[string]any)
	if !ok {
		return false
	}
	got, ok := def["parameters"].(map[string]any)
	if !ok {
		return false
	}
	return fmt.Sprint(got) == fmt.Sprint(want)
}

func openCodeIsFingerprintDefinition(tool map[string]any, key string) bool {
	def := openCodeToolDefinition(tool)
	return ttsString(def["description"]) == ttsString(openCodeFingerprintDefinitions[key]["description"]) &&
		openCodeFingerprintParams(def, key)
}

// openCodeCanNormalizeInPlace: true quando os parâmetros do caller falam os
// mesmos nomes do fingerprint (subset com required presentes), então a definição
// oficial pode substituir a dele sem esconder/add argumento que o handler dele
// não conheça. Caso comum: o CLI no Windows gera descrição do shell com
// "pwsh" — diff de duas palavras que o compare exato rejeitaria.
func openCodeCanNormalizeInPlace(tool map[string]any, key string) bool {
	def := openCodeToolDefinition(tool)
	params, ok := def["parameters"].(map[string]any)
	if !ok {
		return false
	}
	props, ok := params["properties"].(map[string]any)
	if !ok {
		return false
	}
	want, _ := openCodeFingerprintDefinitions[key]["parameters"].(map[string]any)["properties"].(map[string]any)
	for name := range props {
		if _, ok := want[name]; !ok {
			return false
		}
	}
	required, _ := openCodeFingerprintDefinitions[key]["parameters"].(map[string]any)["required"].([]string)
	for _, name := range required {
		if _, ok := props[name]; !ok {
			return false
		}
	}
	return true
}

func openCodeWithFingerprintDefinition(tool map[string]any, key string) map[string]any {
	copied := openCodeWithToolName(tool, openCodeToolName(tool))
	def := openCodeToolDefinition(copied)
	def["description"] = openCodeFingerprintDefinitions[key]["description"]
	def["parameters"] = openCodeFingerprintDefinitions[key]["parameters"]
	return copied
}

// openCodeConcealToolNames move caller tools para fora dos nomes fingerprint
// (alias `_client` para colisões com schema divergente, alias `run_command`
// para nomes terminal-gated) e devolve o mapa enviado -> original.
func openCodeConcealToolNames(tools []any) ([]any, map[string]string) {
	renames := map[string]string{}
	if len(tools) == 0 {
		return tools, renames
	}
	taken := map[string]bool{}
	for _, raw := range tools {
		if tool, ok := raw.(map[string]any); ok {
			taken[openCodeToolName(tool)] = true
		}
	}
	seen := map[string]bool{}
	out := make([]any, 0, len(tools)+2)
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		name := openCodeToolName(tool)
		if openCodeGatedToolName(name) {
			alias := "run_command"
			for i := 2; taken[alias]; i++ {
				alias = fmt.Sprintf("run_command%d", i)
			}
			taken[alias] = true
			renames[alias] = name
			out = append(out, openCodeWithToolName(tool, alias))
			continue
		}
		key := openCodeFingerprintKey(name)
		if key == "" {
			out = append(out, tool)
			continue
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		if openCodeIsFingerprintDefinition(tool, key) {
			out = append(out, tool)
			continue
		}
		if openCodeCanNormalizeInPlace(tool, key) {
			out = append(out, openCodeWithFingerprintDefinition(tool, key))
			continue
		}
		alias := key + "_client"
		for i := 2; taken[alias]; i++ {
			alias = fmt.Sprintf("%s_client%d", key, i)
		}
		taken[alias] = true
		renames[alias] = name
		out = append(out, openCodeWithToolName(tool, alias))
	}
	return out, renames
}

func openCodeAppendMissingFingerprintTools(tools []any) ([]any, []string) {
	appended := []string{}
	for _, name := range openCodeFingerprintTools {
		found := false
		for _, raw := range tools {
			if tool, ok := raw.(map[string]any); ok && openCodeFingerprintKey(openCodeToolName(tool)) == name {
				found = true
				break
			}
		}
		if found {
			continue
		}
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": openCodeFingerprintDefinitions[name]["description"],
				"parameters":  openCodeFingerprintDefinitions[name]["parameters"],
			},
		})
		appended = append(appended, name)
	}
	return tools, appended
}

func openCodeRetargetToolChoice(body map[string]any, renames map[string]string) {
	choice, ok := body["tool_choice"].(map[string]any)
	if !ok {
		return
	}
	if name := strings.TrimSpace(ttsString(choice["name"])); name != "" {
		if alias := openCodeAliasOf(renames, name); alias != "" {
			choice["name"] = alias
		}
		return
	}
	if function, ok := choice["function"].(map[string]any); ok {
		if name := strings.TrimSpace(ttsString(function["name"])); name != "" {
			if alias := openCodeAliasOf(renames, name); alias != "" {
				function["name"] = alias
			}
		}
	}
}

// openCodeAliasOf inverte o mapa enviado->original para achar o alias de um
// nome original.
func openCodeAliasOf(renames map[string]string, original string) string {
	for alias, orig := range renames {
		if orig == original {
			return alias
		}
	}
	return ""
}

// openCodeAliasHistoryToolCalls renomeia tool_calls de turnos anteriores para o
// alias; senão o modelo lê as próprias chamadas anteriores como chamadas à tool
// oficial de mesmo nome.
func openCodeAliasHistoryToolCalls(body map[string]any, renames map[string]string) {
	messages, _ := body["messages"].([]any)
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		calls, _ := message["tool_calls"].([]any)
		for _, rawCall := range calls {
			call, ok := rawCall.(map[string]any)
			if !ok {
				continue
			}
			function, _ := call["function"].(map[string]any)
			if function == nil {
				continue
			}
			if alias := openCodeAliasOf(renames, ttsString(function["name"])); alias != "" {
				function["name"] = alias
			}
		}
	}
}

// Sem decoys "unavailable" no corpo: o upstream não gateia o texto. A nota no
// system evita que o modelo chame as tools appended; "not available" sozinho
// fez o modelo recusar também as tools do caller (live probe 9router) — a
// redação "placeholders ... use the other tools" mantém ambas funcionando.
func openCodeNoteUnavailableTools(body map[string]any, names []string) {
	if len(names) == 0 {
		return
	}
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = "`" + name + "`"
	}
	list := strings.Join(quoted, " or ")
	verb := "tools"
	if len(names) == 1 {
		verb = "tool"
	}
	note := fmt.Sprintf("Do not call the %s %s; they are placeholders that do nothing. Use the other tools you have been given instead.", list, verb)
	if len(names) == 1 {
		note = fmt.Sprintf("Do not call the %s %s; it is a placeholder that does nothing. Use the other tools you have been given instead.", quoted[0], verb)
	}
	openCodeAppendSystemText(body, note)
}

// ---- Bloco <env> (marker textual; append, não replace) ----

const openCodeEnvTag = "<env>"

var openCodeEnvBlock = strings.Join([]string{
	"Here is some useful information about the environment you are running in:",
	"<env>",
	"  Working directory: /home/ubuntu/app",
	"  Workspace root folder: /home/ubuntu/app",
	"  Is directory a git repo: no",
	"  Platform: linux",
	"  Prefer /tmp/opencode over generic system temporary directories such as /tmp; it is pre-created and approved for external access.",
	"</env>",
}, "\n")

// openCodeSystemCarriesEnv: verdadeiro quando o marker já está presente
// (cliente opencode genuíno é passthrough).
func openCodeSystemCarriesEnv(system any) bool {
	switch value := system.(type) {
	case string:
		return strings.Contains(value, openCodeEnvTag)
	case []any:
		for _, raw := range value {
			if block, ok := raw.(map[string]any); ok && strings.Contains(ttsString(block["text"]), openCodeEnvTag) {
				return true
			}
		}
	}
	return false
}

// openCodeAppendSystemText anexa text ao system: string, text blocks (Claude
// shape) ou primeiro system message.
func openCodeAppendSystemText(body map[string]any, text string) {
	if existing, ok := body["system"]; ok {
		switch value := existing.(type) {
		case string:
			body["system"] = strings.TrimSpace(value) + "\n\n" + text
		case []any:
			for i := len(value) - 1; i >= 0; i-- {
				if block, ok := value[i].(map[string]any); ok {
					if _, has := block["text"]; has {
						joined := strings.TrimSpace(ttsString(block["text"]))
						block["text"] = joined + "\n\n" + text
						return
					}
				}
			}
			body["system"] = append(value, map[string]any{"type": "text", "text": text})
		}
		return
	}
	messages, _ := body["messages"].([]any)
	if len(messages) > 0 {
		if first, ok := messages[0].(map[string]any); ok && ttsString(first["role"]) == "system" {
			if content, ok := first["content"].(string); ok {
				first["content"] = strings.TrimSpace(content) + "\n\n" + text
				return
			}
		}
	}
	body["messages"] = append([]any{map[string]any{"role": "system", "content": text}}, messages...)
}

// openCodeApplyEnvFingerprint garante o marker <env> no system; idempotente.
func openCodeApplyEnvFingerprint(body map[string]any) {
	if system, ok := body["system"]; ok {
		if !openCodeSystemCarriesEnv(system) {
			openCodeAppendSystemText(body, openCodeEnvBlock)
		}
		return
	}
	messages, _ := body["messages"].([]any)
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if ttsString(message["role"]) == "system" {
			if !openCodeSystemCarriesEnv(message["content"]) {
				if content, ok := message["content"].(string); ok && strings.TrimSpace(content) != "" {
					message["content"] = strings.TrimRight(content, " \t\n") + "\n\n" + openCodeEnvBlock
				} else {
					message["content"] = openCodeEnvBlock
				}
			}
			return
		}
		break
	}
	// Sem system: injeta o bloco como primeiro message.
	body["messages"] = append([]any{map[string]any{"role": "system", "content": openCodeEnvBlock}}, messages...)
}

// openCodeApplyFingerprint compõe conceal + append + retarget + history alias +
// nota de placeholders + <env>. Devolve o mapa enviado -> original para a
// resposta restaurar os nomes do cliente.
func openCodeApplyFingerprint(body map[string]any) map[string]string {
	tools, _ := body["tools"].([]any)
	concealed, renames := openCodeConcealToolNames(tools)
	updated, appended := openCodeAppendMissingFingerprintTools(concealed)
	body["tools"] = updated
	openCodeRetargetToolChoice(body, renames)
	openCodeAliasHistoryToolCalls(body, renames)
	if len(appended) > 0 {
		openCodeNoteUnavailableTools(body, appended)
	}
	openCodeApplyEnvFingerprint(body)
	// tool_choice "none" é respondido com 403; sem escolha explícita o upstream
	// decide (auto).
	if body["tool_choice"] == "none" {
		delete(body, "tool_choice")
	}
	if len(renames) == 0 {
		return nil
	}
	return renames
}

// openCodeRestoreNames devolve ao cliente os nomes originais nas tool_calls
// (delta e message), na ida e na volta do stream dobrado.
func openCodeRestoreNames(payload map[string]any, renames map[string]string) {
	if len(renames) == 0 || payload == nil {
		return
	}
	choices, _ := payload["choices"].([]any)
	for _, raw := range choices {
		choice, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		for _, holder := range []string{"delta", "message"} {
			message, ok := choice[holder].(map[string]any)
			if !ok {
				continue
			}
			calls, _ := message["tool_calls"].([]any)
			for _, rawCall := range calls {
				call, ok := rawCall.(map[string]any)
				if !ok {
					continue
				}
				function, _ := call["function"].(map[string]any)
				if function == nil {
					continue
				}
				if original := renames[ttsString(function["name"])]; original != "" {
					function["name"] = original
				}
			}
		}
	}
}

// ---- TLS: ClientHello espelhado do Bun 1.3.14 (BoringSSL) ----

// Cipher list do Bun, ordem verbatim (JA4 cipher hash 5b57614c22b0; só o hash
// da lista é comparado upstream).
var openCodeCipherSuites = []uint16{
	utls.TLS_AES_128_GCM_SHA256,
	utls.TLS_AES_256_GCM_SHA384,
	utls.TLS_CHACHA20_POLY1305_SHA256,
	0xc02b, 0xc02f, 0xc02c, 0xc030, 0xcca9, 0xcca8,
	0xc009, 0xc013, 0xc00a, 0xc014,
	0x009c, 0x009d, 0x002f, 0x0035,
}

// openCodeClientHelloSpec espelha o ClientHello capturado do Bun 1.3.14
// (dump local 2026-11-19; probe Go com esta spec respondeu 200 no gate):
// sem GREASE, extensões em ordem fixa, key_share único x25519, padding
// BoringSSL. Qualquer divergência de bytes volta a tomar 429.
func openCodeClientHelloSpec() *utls.ClientHelloSpec {
	return &utls.ClientHelloSpec{
		CipherSuites:       openCodeCipherSuites,
		CompressionMethods: []byte{0},
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{},
			&utls.ExtendedMasterSecretExtension{},
			&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient},
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{utls.X25519, utls.CurveP256, utls.CurveP384}},
			&utls.SupportedPointsExtension{SupportedPoints: []byte{0}},
			&utls.SessionTicketExtension{},
			&utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}},
			&utls.StatusRequestExtension{},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
				0x0403, 0x0804, 0x0401, 0x0503, 0x0805, 0x0501, 0x0806, 0x0601, 0x0201,
			}},
			&utls.SCTExtension{},
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{{Group: utls.X25519}}},
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{utls.PskModeDHE}},
			&utls.SupportedVersionsExtension{Versions: []uint16{0x0304, 0x0303}},
			&utls.UtlsPaddingExtension{GetPaddingLen: utls.BoringPaddingStyle},
		},
	}
}

// openCodeTLSHandshake faz o handshake uTLS sobre um conn cru (direto ou depois
// de CONNECT de proxy).
func openCodeTLSHandshake(ctx context.Context, raw net.Conn, host string, base *utls.Config) (net.Conn, error) {
	config := &utls.Config{ServerName: host}
	if base != nil {
		config.InsecureSkipVerify = base.InsecureSkipVerify
		config.RootCAs = base.RootCAs
	}
	conn := utls.UClient(raw, config, utls.HelloCustom)
	if err := conn.ApplyPreset(openCodeClientHelloSpec()); err != nil {
		raw.Close()
		return nil, err
	}
	if err := conn.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, err
	}
	return conn, nil
}

// ---- Fold do stream (cliente JSON) ----

// openCodeFoldStream agrega um SSE OpenAI em um único chat.completion; erro no
// meio do stream é devolvido como erro, nunca como completion vazio.
func openCodeFoldStream(reader io.Reader, model string, renames map[string]string) (map[string]any, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 16<<20)
	message := map[string]any{"role": "assistant", "content": ""}
	content, reasoning := "", ""
	toolCalls := map[int]map[string]any{}
	finish := any(nil)
	usage := any(nil)
	id, created := "", any(nil)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		if failure := chunk["error"]; failure != nil {
			return nil, fmt.Errorf("upstream stream error: %s", ttsString(failure))
		}
		if value := ttsString(chunk["id"]); value != "" {
			id = value
		}
		if value, ok := chunk["created"]; ok {
			created = value
		}
		if value, ok := chunk["usage"].(map[string]any); ok && len(value) > 0 {
			usage = value
		}
		choices, _ := chunk["choices"].([]any)
		for _, raw := range choices {
			choice, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if value := ttsString(choice["finish_reason"]); value != "" {
				finish = value
			}
			delta, _ := choice["delta"].(map[string]any)
			if delta == nil {
				continue
			}
			content += ttsString(delta["content"])
			reasoning += ttsString(firstNonEmpty(delta["reasoning_content"], delta["reasoning"]))
			calls, _ := delta["tool_calls"].([]any)
			for _, rawCall := range calls {
				call, ok := rawCall.(map[string]any)
				if !ok {
					continue
				}
				index := int(pxpipeNum(call["index"]))
				entry := toolCalls[index]
				if entry == nil {
					entry = map[string]any{"id": ttsString(call["id"]), "type": "function", "function": map[string]any{"name": "", "arguments": ""}}
					toolCalls[index] = entry
				}
				if value := ttsString(call["id"]); value != "" {
					entry["id"] = value
				}
				function, _ := call["function"].(map[string]any)
				target := entry["function"].(map[string]any)
				if value := ttsString(function["name"]); value != "" {
					target["name"] = value
				}
				target["arguments"] = ttsString(target["arguments"]) + ttsString(function["arguments"])
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if content != "" {
		message["content"] = content
	}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	if len(toolCalls) > 0 {
		ordered := make([]any, 0, len(toolCalls))
		for i := 0; i < len(toolCalls); i++ {
			if entry, ok := toolCalls[i]; ok {
				ordered = append(ordered, entry)
			}
		}
		message["tool_calls"] = ordered
	}
	if finish == nil {
		finish = "stop"
	}
	completion := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
	}
	if usage != nil {
		completion["usage"] = usage
	}
	openCodeRestoreNames(completion, renames)
	return completion, nil
}
