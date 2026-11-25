package handlers

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/zanonaa/VansGo/backend/internal/providers"
)

// Porte de open-sse/executors/antigravity.js (Cloud Code Assist, host daily):
// o corpo Gemini é embrulhado em {project, model, userAgent, requestId,
// request} e enviado a :streamGenerateContent?alt=sse. Além do embrulho, o
// executor aplica as transformações que a IDE Antigravity faz antes de falar
// com o protobuf do Google: whitelist de campos do request, sanitização de
// function names, schema-clean das tools, reescrita de prompts de clientes
// concorrentes e backfill de thoughtSignature em functionCalls sem assinatura
// (Gemini 3+ rejeita functionCall sem assinatura; a origem usa a default no
// primeiro e o sentinel de bypass nos subsequentes).
const (
	// Identidade do cliente: User-Agent do CLI agy (agCliUserAgent, formato em
	// ag_headers.go). Allowlist do upstream é dinâmica/temporal: o UA do IDE
	// (antigravity/2.8.0 linux/amd64) ainda passa no chat, mas corta o
	// catálogo do fetchAvailableModels pra ~21 models (sem a família gemini-4).
	// O UA CLI libera os 37 (neon/carbon/argon/hydrogen com xhigh,
	// claude-5-5) — validado contra produção em 2026-11-25; revalidar contra
	// o streamGenerateContent antes de qualquer bump de versão.
	agMaxOutputTokens = 16384.0
)

// agUserAgent não pode ser const (agCliUserAgent é função, monta os_type/arch
// de runtime.GOOS/GOARCH); semântica de const via var de nível de pacote.
var agUserAgent = agCliUserAgent()

var (
	// COMPETITIVE_CLAUDE_AGENT_PROMPT: prompts de clientes concorrentes fazem o
	// upstream marcar o request com 429 Quota Exhausted mesmo com quota.
	agClaudeAgentPrompt = "You are a Claude agent, built on Anthropic's Claude Agent SDK."
	// DEFAULT_THINKING_AG_SIGNATURE de config/defaultThinkingSignature.js.
	agDefaultSignature = "EuwGCukGAXLI2nxwZIq54WWSoL/YN0P3TsDZ7zRnLi8g0S4aVr2HUGxvaHKySuY6HAVzcE0GPGjXrytLIldxthSvfxgUlJh6Qa9Z+Oj5QZBlYdg6HaJ6yuY5R7waE6rdwBsRf7Ft2j3DJ9rMi9qhWFqApewYtPhls3VHtuvND3l8Rm09+lbAXQs6KKWEWrxNLKTBkfpMgXhRERc/TQRMZu1twAablm6/Zk1tsYRvfWKLsNbeKF+CCojJdXJKvnR/8Ouuoa+Y2Ti20hcW7aZIIjZDFYPU//k6Ybmhg69J/imbFai2ckhfLaisqdDkdoIiBJScTOUvYqP6AE9d4MsydSC+UlhIMk4hoP76R8vUSCZRMkjOaDXstf/QoVZKbt94wyRZgAJ1G0BqI8L5ow86kLpA4wJEtxsRGymOE4bKUvApveBakYDNM9APkf+LbtbzWSseGjoZcSlycF9iN8Q2XNYKRrHbv3Lr5Y8JjdH/5y/6SHkNehTEZugaeGnSPSyCTWto1kQgHpxdWmhkLfJGNUGLmue7Mesj4TSms4J33mRpYVhNB/J333FCqIP0hr/E7BkkjEn7yZ4X7SQlh+xKPurapsnHRwiKmtsilmEFrnTE9iQr+pMr6M29qqFNv1tr5yumbaJw8JW9sB15tNsRv+dW6BjNanbsKz7HCgKUBc8tGy+7YuhXzAfViyRefcjK7eZW0Fbyt7AbybJTKz78W8NH7ye6LAwzOebXpeZ4D43fNIt8bKh26qgduSQv/7o+pAflkuqHZ99YWgHQ8h8OkZFi3eOiSYjsjhdZ/czWOdoPI/OnqIldzMPF5YlrKBLFX8VhRKVmqgsmWf5PHGulHhMkVlS+XG2UIseGy69ARa93D78Gsa+1n1kJr7EEB7Rh+27vUMxVYLdz1yMSvE5nalTAlg/ZeG8+XQ0cHuAI3KbQpHW2Q++RdXfm5JzD5WdJZUU+Zn8t8UUn85BH4RxZLeE0qJikgSsKoYVBc6YhiMjhPgkR95ReimY4Z0xCJdRo1gjexOFeODZMpQF6Yxnoic7IrdgsFA3iePTbFnPp3IAM1fAThWhXJUn3QInUOTd5o1qmTmn6REbL15g/JQNl+dqUoPkhleeb2V3kjqp1okmO3wMZbPknR3S1LZNmlS72/iBQUm+n2b/RCn4PjmM2"
	// Sentinel do bypass antigravity (origem: openai-to-gemini.ts, issue #1410
	// que supersedeu a #725): functionCalls históricos sem assinatura real —
	// clientes OpenAI-compatíveis não carregam thoughtSignature — são aceitos
	// pelo upstream quando carregam este valor.
	agSignatureBypass = "skip_thought_signature_validator"
)

var (
	agRequestIdRe      = regexp.MustCompile(`^agent/[^/]+/\d+/[^/]+/\d+$`)
	agTieredSuffixRe   = regexp.MustCompile(`-tiered\([^)]*\)$`)
	agOpenCodeRe       = regexp.MustCompile(`(?i)opencode`)
	agFunctionBadRe    = regexp.MustCompile(`[^a-zA-Z0-9_.:-]`)
	agFunctionLeadRe   = regexp.MustCompile(`^[a-zA-Z_]`)
	agNoThinkingRe     = regexp.MustCompile(`(?i)^(claude-|gpt-oss-|tab_)`)
	agPromptTriggers   = []string{agClaudeAgentPrompt, "Hermes Agent", "Nous Research"}
	agRequestFields    = []string{"contents", "systemInstruction", "generationConfig", "sessionId", "safetySettings"}
	agRequestBlacklist = []string{"output_config", "thinking", "reasoning_effort", "reasoning", "enable_thinking", "thinking_budget"}
)

// UNSUPPORTED_SCHEMA_CONSTRAINTS de translator/formats/gemini.js: keywords que
// o proto Gemini não tem; uma ocorrência rejeita o request inteiro com
// "Unknown name ...: Cannot find field".
var agUnsupportedSchema = map[string]bool{
	"minLength": true, "maxLength": true, "exclusiveMinimum": true, "exclusiveMaximum": true,
	"minItems": true, "maxItems": true, "format": true, "multipleOf": true,
	"uniqueItems": true, "contains": true,
	"unevaluatedProperties": true, "unevaluatedItems": true, "contentSchema": true,
	"default": true, "examples": true,
	"$schema": true, "$defs": true, "definitions": true, "const": true, "$ref": true, "$comment": true,
	"deprecated": true, "readOnly": true, "writeOnly": true,
	"additionalProperties": true, "propertyNames": true, "patternProperties": true, "enumDescriptions": true,
	"anyOf": true, "oneOf": true, "allOf": true, "not": true,
	"dependencies": true, "dependentSchemas": true, "dependentRequired": true,
	"title": true, "optional": true, "if": true, "then": true, "else": true, "contentMediaType": true, "contentEncoding": true,
	"cornerRadius": true, "fillColor": true, "fontFamily": true, "fontSize": true, "fontWeight": true,
	"gap": true, "padding": true, "strokeColor": true, "strokeThickness": true, "textColor": true,
}

// SendAntigravity porta o executor antigravity.js. Mesma família Cloud Code
// Assist do SendGeminiCLI (conversão de resposta idêntica, target "gemini"),
// com o transform de request da IDE.
func (t *TranslatorSend) SendAntigravity(w http.ResponseWriter, r *http.Request, provider, model string, body map[string]any, source string, stream bool) {
	fail := func(status int, message string) {
		nodeReply(w, status, map[string]any{"success": false, "error": message})
	}
	connections, err := t.Connections.List(r.Context(), provider, nil)
	if err != nil {
		fail(500, err.Error())
		return
	}
	connection := comboConnectionFor(r.Context(), provider, connections)
	if connection == nil {
		fail(400, "No active connection for provider: "+provider)
		return
	}
	usageSelected(r.Context(), connection)
	intermediate := body
	if source != "openai" {
		intermediate, err = translateSource(body, source)
		if err != nil {
			fail(400, err.Error())
			return
		}
	}
	geminiBody, err := translateTarget(intermediate, "gemini", model)
	if err != nil {
		fail(400, err.Error())
		return
	}
	metadata, _ := connection["providerSpecificData"].(map[string]any)
	project := firstNonEmptyString(metadata["projectId"], metadata["quota_project_id"], connection["projectId"])
	request := agTransformRequest(geminiBody, connection)
	// Upstream dinâmico primeiro (fetchAvailableModels da conexão); o
	// catálogo estático é só fallback quando o fetch falha.
	upstream := agUpstreamModel(provider, model)
	if dynamic, ok := agUpstreamOverride(r.Context(), connection, model, t.Client); ok {
		upstream = dynamic
	}
	wrapper := map[string]any{
		"model":     upstream,
		"userAgent": "antigravity",
		"requestId": agBuildRequestId(body, request, model),
		"request":   request,
	}
	if project != "" {
		wrapper["project"] = project
	}
	t.TokenSaver.Apply(r.Context(), r, wrapper, "antigravity", model, provider)
	encoded, err := json.Marshal(wrapper)
	if err != nil {
		fail(500, err.Error())
		return
	}
	target, headers, err := translatorPlan(provider, model, stream, connection)
	if err != nil {
		fail(501, err.Error())
		return
	}
	if stream {
		target = strings.TrimRight(target, "/") + ":streamGenerateContent?alt=sse"
	} else {
		target = strings.TrimRight(target, "/") + ":generateContent"
	}
	headers["User-Agent"] = agUserAgent
	if stream {
		headers["Accept"] = "text/event-stream"
	} else {
		headers["Accept"] = "application/json"
	}
	newRequest := func() (*http.Request, error) {
		request, err := http.NewRequestWithContext(r.Context(), "POST", target, bytes.NewReader(encoded))
		if err != nil {
			return nil, err
		}
		for key, value := range headers {
			request.Header.Set(key, value)
		}
		return request, nil
	}
	client, relay, cleanup, err := connectionHTTP(r.Context(), metadata, t.GetPool, t.Client)
	if err != nil {
		fail(500, err.Error())
		return
	}
	defer cleanup()
	refreshTried := false
	for {
		request, err := newRequest()
		if err != nil {
			fail(500, err.Error())
			return
		}
		if err := rewriteConnectionRelay(request, relay); err != nil {
			fail(500, err.Error())
			return
		}
		response, err := client.Do(request)
		if err != nil {
			fail(500, err.Error())
			return
		}
		if (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) && !refreshTried {
			refreshTried = true
			patch, refreshErr := t.refreshConnection(r.Context(), provider, connection)
			if refreshErr == nil && patch != nil {
				response.Body.Close()
				if ttsString(connection["apiKey"]) == "" {
					headers["Authorization"] = "Bearer " + ttsString(patch["accessToken"])
				}
				continue
			}
		}
		if contentType := response.Header.Get("Content-Type"); response.StatusCode >= 200 && response.StatusCode < 300 && isHTMLContentType(contentType) {
			response.Body.Close()
			nodeReply(w, 502, map[string]any{"success": false, "error": fmt.Sprintf("Provider %s returned HTML instead of a chat response (content-type: %s)", provider, contentType)})
			return
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			details, _ := io.ReadAll(response.Body)
			response.Body.Close()
			nodeReply(w, response.StatusCode, map[string]any{"success": false, "error": fmt.Sprintf("Provider error: %d", response.StatusCode), "details": string(details)})
			return
		}
		if stream {
			conversion := &streamConversion{destination: w, source: source, target: "gemini", model: model, textBlock: -1, thinkingBlock: -1, serverTool: -1}
			_, _ = io.Copy(conversion, response.Body)
			conversion.finish()
		} else {
			conversion := &convertedResponse{destination: w, source: source, target: "gemini", model: model}
			_, _ = io.Copy(conversion, response.Body)
			conversion.finish()
		}
		response.Body.Close()
		return
	}
}

// agUpstreamModel resolve o id que o upstream reconhece (UpstreamModelID) e
// aplica o strip -tiered(...) → -tiered da origem (resolveAntigravityUpstreamModel).
func agUpstreamModel(provider, model string) string {
	upstream := providers.UpstreamModelID(provider, model)
	return agTieredSuffixRe.ReplaceAllString(upstream, "-tiered")
}

// agTransformRequest replica o transformRequest do executor (caminho chat,
// sem image-gen): whitelist, blacklists, reescrita de prompts, cap de tokens,
// tools sanitizadas e sessionId.
func agTransformRequest(geminiBody map[string]any, connection map[string]any) map[string]any {
	request := map[string]any{}
	for _, key := range agRequestFields {
		if value, ok := geminiBody[key]; ok {
			request[key] = value
		}
	}
	agStripBlacklisted(request)
	agRewriteSystemPrompts(request)
	request["contents"] = agNormalizeContents(agFixContents(geminiBody["contents"]))
	tools := agCleanTools(geminiBody["tools"])
	if len(tools) > 0 {
		request["tools"] = tools
		request["toolConfig"] = map[string]any{"functionCallingConfig": map[string]any{"mode": "VALIDATED"}}
	}
	generationConfig, _ := request["generationConfig"].(map[string]any)
	if generationConfig == nil {
		generationConfig = map[string]any{}
	}
	if tokens, ok := generationConfig["maxOutputTokens"].(float64); ok && tokens > agMaxOutputTokens {
		generationConfig["maxOutputTokens"] = agMaxOutputTokens
	}
	request["generationConfig"] = generationConfig
	request["sessionId"] = agSessionId(geminiBody, connection)
	return request
}

// agStripBlacklisted remove os campos de thinking que thinkingUnified.js seta
// e que o protobuf do Google rejeita (request e top-level).
func agStripBlacklisted(target map[string]any) {
	for _, key := range agRequestBlacklist {
		delete(target, key)
	}
}

// agRewriteSystemPrompts aplica os ANTIGRAVITY_PROMPT_REWRITES: remove o
// prompt de agente Claude e troca marcas opencode por Antigravity.
func agRewriteSystemPrompts(request map[string]any) {
	system, ok := request["systemInstruction"].(map[string]any)
	if !ok {
		return
	}
	parts, ok := system["parts"].([]any)
	if !ok {
		return
	}
	for _, rawPart := range parts {
		part, ok := rawPart.(map[string]any)
		if !ok {
			continue
		}
		text, ok := part["text"].(string)
		if !ok {
			continue
		}
		text = agOpenCodeRe.ReplaceAllStringFunc(text, func(match string) string {
			switch match {
			case "OpenCode":
				return "Antigravity"
			case "OPENCODE":
				return "ANTIGRAVITY"
			}
			return "antigravity"
		})
		for _, trigger := range agPromptTriggers {
			text = strings.ReplaceAll(text, trigger, "")
		}
		part["text"] = text
	}
}

// agFixContents replica o map de contents da origem: functionResponse vira
// role "user", parts thought-only são descartadas (mantendo functionCall) e
// functionCalls sem thoughtSignature recebem backfill — a assinatura default
// no primeiro e o sentinel de bypass nos demais (Gemini 3+ rejeita functionCall
// sem assinatura; o upstream só tolera a fake histórica no 1º).
func agFixContents(raw any) []any {
	list, _ := raw.([]any)
	firstFunctionCallSeen := false
	out := []any{}
	for _, item := range list {
		content, ok := item.(map[string]any)
		if !ok {
			continue
		}
		parts, _ := content["parts"].([]any)
		role := ttsString(content["role"])
		hasFunctionResponse := false
		kept := []any{}
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			_, isCall := part["functionCall"]
			_, isResponse := part["functionResponse"]
			if isResponse {
				hasFunctionResponse = true
			}
			if nodeTruthy(part["thought"]) && !isCall {
				continue
			}
			if _, hasSig := part["thoughtSignature"]; hasSig && !isCall {
				if _, hasText := part["text"]; !hasText {
					continue
				}
			}
			if isCall {
				// A origem supriu a assinatura fake apenas no primeiro
				// functionCall (issue #725), mas o upstream passou a rejeitar
				// os subsequentes sem assinatura (superseded pela #1410): o
				// bypass sentinel cobre qualquer functionCall sem assinatura.
				if _, hasSig := part["thoughtSignature"]; !hasSig {
					if !firstFunctionCallSeen {
						part["thoughtSignature"] = agDefaultSignature
					} else {
						part["thoughtSignature"] = agSignatureBypass
					}
				}
				firstFunctionCallSeen = true
			}
			kept = append(kept, part)
		}
		if len(kept) == 0 {
			continue
		}
		if hasFunctionResponse {
			role = "user"
		}
		out = append(out, map[string]any{"role": role, "parts": kept})
	}
	return out
}

// agNormalizeContents replica normalizeGeminiContents: descarta contents sem
// role/parts, merge de turns consecutivos da mesma role e garante primeiro
// turn user (o proto rejeita história começando em model).
func agNormalizeContents(contents []any) []any {
	out := []any{}
	for _, item := range contents {
		content, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role := ttsString(content["role"])
		parts, ok := content["parts"].([]any)
		if role == "" || !ok {
			continue
		}
		filtered := []any{}
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok || len(part) == 0 {
				continue
			}
			filtered = append(filtered, part)
		}
		if len(filtered) == 0 {
			continue
		}
		if len(out) > 0 {
			if last, ok := out[len(out)-1].(map[string]any); ok && ttsString(last["role"]) == role {
				lastParts, _ := last["parts"].([]any)
				last["parts"] = append(lastParts, filtered...)
				continue
			}
		}
		out = append(out, map[string]any{"role": role, "parts": filtered})
	}
	if len(out) > 0 && ttsString(out[0].(map[string]any)["role"]) != "user" {
		out = append([]any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "..."}}}}, out...)
	}
	return out
}

// agCleanTools mescla todos os grupos em um único functionDeclarations,
// sanitiza os nomes e limpa os schemas (o protobuf espera 1 grupo; nomes
// duplicados ou schemas com keywords exóticas rejeitam o request).
func agCleanTools(raw any) []any {
	groups, _ := raw.([]any)
	if len(groups) == 0 {
		return nil
	}
	seen := map[string]bool{}
	declarations := []any{}
	for _, rawGroup := range groups {
		group, ok := rawGroup.(map[string]any)
		if !ok {
			continue
		}
		functions, _ := group["functionDeclarations"].([]any)
		for _, rawFunction := range functions {
			function, ok := rawFunction.(map[string]any)
			if !ok {
				continue
			}
			name := agSanitizeFunctionName(ttsString(function["name"]))
			if seen[name] {
				continue
			}
			seen[name] = true
			clean := map[string]any{}
			for key, value := range function {
				if key == "parameters" || key == "parametersJsonSchema" {
					continue
				}
				clean[key] = value
			}
			clean["name"] = name
			if parameters, ok := function["parameters"].(map[string]any); ok {
				clean["parameters"] = agCleanJSONSchema(parameters)
			} else {
				clean["parameters"] = map[string]any{"type": "object", "properties": map[string]any{"reason": map[string]any{"type": "string", "description": "Brief explanation"}}, "required": []any{"reason"}}
			}
			declarations = append(declarations, clean)
		}
	}
	if len(declarations) == 0 {
		return nil
	}
	return []any{map[string]any{"functionDeclarations": declarations}}
}

// agSanitizeFunctionName: Gemini exige [a-zA-Z_][a-zA-Z0-9_.:-]{0,63}.
func agSanitizeFunctionName(name string) string {
	if name == "" {
		return "_unknown"
	}
	sanitized := agFunctionBadRe.ReplaceAllString(name, "_")
	if !agFunctionLeadRe.MatchString(sanitized) {
		sanitized = "_" + sanitized
	}
	if len(sanitized) > 64 {
		sanitized = sanitized[:64]
	}
	return sanitized
}

// agSessionId: o sessionId do request ou um uuid estável derivado da conexão
// (simplificação do deriveSessionId da origem; mantém o requestId estável
// por conexão sem precisar de store).
func agSessionId(geminiBody map[string]any, connection map[string]any) string {
	if id := ttsString(geminiBody["sessionId"]); id != "" {
		return id
	}
	seed := firstNonEmptyString(connection["email"], connection["id"])
	if seed == "" {
		seed = "anonymous"
	}
	return agUuidFromSeed("antigravity:session:" + seed)
}

// agUuidFromSeed replica uuidFromSeed: uuid v4-lookalike por SHA-256 do seed,
// estável entre requests (conversation/trajectory do requestId).
func agUuidFromSeed(seed string) string {
	digest := sha256.Sum256([]byte(seed))
	bytes := digest[:16]
	bytes[6] = (bytes[6] & 0x0f) | 0x50
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(bytes)
	return fmt.Sprintf("%s-%s-%s-%s-%s", encoded[0:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:32])
}

// agBuildRequestId replica buildIdeRequestId: aceita requestId existente no
// formato da IDE ou gera agent/<conversation>/<ts>/<trajectory>/<step>.
func agBuildRequestId(body map[string]any, request map[string]any, model string) string {
	if id := ttsString(body["requestId"]); agRequestIdRe.MatchString(id) {
		return id
	}
	sessionId := ttsString(request["sessionId"])
	if sessionId == "" {
		sessionId = "anonymous"
	}
	conversation := agUuidFromSeed("antigravity:conversation:" + sessionId)
	trajectory := agUuidFromSeed("antigravity:trajectory:" + sessionId + ":" + model + ":agent")
	step := 1
	if contents, ok := request["contents"].([]any); ok && len(contents)*2-1 > step {
		step = len(contents)*2 - 1
	}
	return fmt.Sprintf("agent/%s/%d/%s/%d", conversation, time.Now().UnixMilli(), trajectory, step)
}

// agCleanJSONSchema replica cleanJSONSchemaForAntigravity: const→enum,
// enum→strings, merge allOf, flatten anyOf/oneOf, flatten type arrays,
// type inference, remoção de keywords não suportadas e placeholders para
// schemas de objeto vazios.
func agCleanJSONSchema(schema map[string]any) map[string]any {
	if schema == nil {
		return nil
	}
	agConvertConstToEnum(schema)
	agConvertEnumValuesToStrings(schema)
	agMergeAllOf(schema)
	agFlattenAnyOfOneOf(schema)
	agFlattenTypeArrays(schema)
	agEnsureObjectType(schema)
	agEnsureArrayItems(schema)
	agRemoveUnsupportedKeywords(schema)
	agCleanupRequired(schema)
	agAddPlaceholders(schema)
	return schema
}

func agConvertConstToEnum(value any) {
	switch typed := value.(type) {
	case map[string]any:
		if raw, has := typed["const"]; has {
			if _, hasEnum := typed["enum"]; !hasEnum {
				typed["enum"] = []any{raw}
				delete(typed, "const")
			}
		}
		for _, item := range typed {
			agConvertConstToEnum(item)
		}
	case []any:
		for _, item := range typed {
			agConvertConstToEnum(item)
		}
	}
}

func agConvertEnumValuesToStrings(value any) {
	switch typed := value.(type) {
	case map[string]any:
		if rawEnum, ok := typed["enum"].([]any); ok {
			enum := make([]any, len(rawEnum))
			for i, item := range rawEnum {
				enum[i] = fmt.Sprintf("%v", item)
			}
			typed["enum"] = enum
			if _, hasType := typed["type"]; !hasType {
				typed["type"] = "string"
			}
		}
		for _, item := range typed {
			agConvertEnumValuesToStrings(item)
		}
	case []any:
		for _, item := range typed {
			agConvertEnumValuesToStrings(item)
		}
	}
}

func agMergeAllOf(value any) {
	switch typed := value.(type) {
	case map[string]any:
		if allOf, ok := typed["allOf"].([]any); ok {
			properties := map[string]any{}
			required := []any{}
			for _, raw := range allOf {
				item, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				if itemProperties, ok := item["properties"].(map[string]any); ok {
					for key, item := range itemProperties {
						properties[key] = item
					}
				}
				if itemRequired, ok := item["required"].([]any); ok {
					for _, req := range itemRequired {
						duplicate := false
						for _, existing := range required {
							if existing == req {
								duplicate = true
								break
							}
						}
						if !duplicate {
							required = append(required, req)
						}
					}
				}
			}
			delete(typed, "allOf")
			if len(properties) > 0 {
				merged, _ := typed["properties"].(map[string]any)
				if merged == nil {
					merged = map[string]any{}
				}
				for key, item := range properties {
					merged[key] = item
				}
				typed["properties"] = merged
			}
			if len(required) > 0 {
				existing, _ := typed["required"].([]any)
				typed["required"] = append(existing, required...)
			}
		}
		for _, item := range typed {
			agMergeAllOf(item)
		}
	case []any:
		for _, item := range typed {
			agMergeAllOf(item)
		}
	}
}

func agFlattenAnyOfOneOf(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for _, key := range []string{"anyOf", "oneOf"} {
			list, ok := typed[key].([]any)
			if !ok || len(list) == 0 {
				continue
			}
			nonNull := []map[string]any{}
			for _, item := range list {
				schema, ok := item.(map[string]any)
				if !ok || ttsString(schema["type"]) == "null" {
					continue
				}
				nonNull = append(nonNull, schema)
			}
			if len(nonNull) == 0 {
				continue
			}
			selected := nonNull[agSelectBest(nonNull)]
			delete(typed, key)
			for schemaKey, schemaValue := range selected {
				typed[schemaKey] = schemaValue
			}
		}
		for _, item := range typed {
			agFlattenAnyOfOneOf(item)
		}
	case []any:
		for _, item := range typed {
			agFlattenAnyOfOneOf(item)
		}
	}
}

// agSelectBest pontua os schemas de anyOf/oneOf e escolhe o mais rico
// (object > array > escalar), como a origem.
func agSelectBest(items []map[string]any) int {
	best, bestScore := 0, -1
	for index, item := range items {
		score := 0
		schemaType := ttsString(item["type"])
		_, hasProperties := item["properties"]
		_, hasItems := item["items"]
		switch {
		case schemaType == "object" || hasProperties:
			score = 3
		case schemaType == "array" || hasItems:
			score = 2
		case schemaType != "" && schemaType != "null":
			score = 1
		}
		if score > bestScore {
			bestScore = score
			best = index
		}
	}
	return best
}

func agFlattenTypeArrays(value any) {
	switch typed := value.(type) {
	case map[string]any:
		if list, ok := typed["type"].([]any); ok {
			chosen := "string"
			for _, item := range list {
				if name := ttsString(item); name != "" && name != "null" {
					chosen = name
					break
				}
			}
			typed["type"] = chosen
		}
		for _, item := range typed {
			agFlattenTypeArrays(item)
		}
	case []any:
		for _, item := range typed {
			agFlattenTypeArrays(item)
		}
	}
}

func agEnsureObjectType(value any) {
	switch typed := value.(type) {
	case map[string]any:
		if _, hasProperties := typed["properties"]; hasProperties {
			if _, hasType := typed["type"]; !hasType {
				typed["type"] = "object"
			}
		}
		for _, item := range typed {
			agEnsureObjectType(item)
		}
	case []any:
		for _, item := range typed {
			agEnsureObjectType(item)
		}
	}
}

func agEnsureArrayItems(value any) {
	switch typed := value.(type) {
	case map[string]any:
		if ttsString(typed["type"]) == "array" {
			if _, hasItems := typed["items"]; !hasItems {
				typed["items"] = map[string]any{"type": "string"}
			}
		}
		for _, item := range typed {
			agEnsureArrayItems(item)
		}
	case []any:
		for _, item := range typed {
			agEnsureArrayItems(item)
		}
	}
}

func agRemoveUnsupportedKeywords(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key := range typed {
			if agUnsupportedSchema[key] || strings.HasPrefix(key, "x-") {
				delete(typed, key)
			}
		}
		for _, item := range typed {
			agRemoveUnsupportedKeywords(item)
		}
	case []any:
		for _, item := range typed {
			agRemoveUnsupportedKeywords(item)
		}
	}
}

func agCleanupRequired(value any) {
	switch typed := value.(type) {
	case map[string]any:
		if required, ok := typed["required"].([]any); ok {
			if properties, hasProperties := typed["properties"].(map[string]any); hasProperties {
				valid := []any{}
				for _, req := range required {
					if _, exists := properties[ttsString(req)]; exists {
						valid = append(valid, req)
					}
				}
				if len(valid) == 0 {
					delete(typed, "required")
				} else {
					typed["required"] = valid
				}
			}
		}
		for _, item := range typed {
			agCleanupRequired(item)
		}
	case []any:
		for _, item := range typed {
			agCleanupRequired(item)
		}
	}
}

// agPlaceholderSchema é o placeholder de propriedade da origem (addPlaceholders).
func agPlaceholderSchema() map[string]any {
	return map[string]any{"type": "string", "description": "Brief explanation of why you are calling this tool"}
}

func agAddPlaceholders(value any) {
	switch typed := value.(type) {
	case map[string]any:
		if len(typed) == 0 {
			typed["type"] = "object"
			typed["properties"] = map[string]any{"reason": agPlaceholderSchema()}
			typed["required"] = []any{"reason"}
			return
		}
		if ttsString(typed["type"]) == "object" {
			properties, _ := typed["properties"].(map[string]any)
			if len(properties) == 0 {
				typed["properties"] = map[string]any{"reason": agPlaceholderSchema()}
				typed["required"] = []any{"reason"}
			}
		}
		for _, item := range typed {
			agAddPlaceholders(item)
		}
	case []any:
		for _, item := range typed {
			agAddPlaceholders(item)
		}
	}
}
