package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"runtime"
	"strings"
	"time"
)

var antigravityIneligible = regexp.MustCompile(`(?i)VALIDATION_REQUIRED|not eligible for Gemini Code Assist`)

type weeklyUsageEntry struct {
	done    chan struct{}
	result  map[string]any
	expires time.Time
}

func cloudProject(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case map[string]any:
		return strings.TrimSpace(ttsString(typed["id"]))
	default:
		return ""
	}
}

func googlePlatform() float64 {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64":
		return 2
	case "darwin/amd64":
		return 1
	case "linux/arm64":
		return 4
	case "linux/amd64":
		return 3
	case "windows/amd64":
		return 5
	default:
		return 0
	}
}

func fractionQuota(model string, fraction float64, reset any, display any) map[string]any {
	remaining := math.Round(1000 * fraction)
	result := map[string]any{"used": math.Max(0, 1000-remaining), "total": 1000, "resetAt": usageResetDate(reset), "remainingPercentage": fraction * 100, "unlimited": false}
	if display != nil {
		result["displayName"] = firstNonEmpty(display, model)
	}
	return result
}

func (u *UsageContracts) googleJSON(ctx context.Context, target string, headers map[string]string, payload map[string]any) (int, map[string]any, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	status, data, err := u.resetJSON(ctx, "POST", target, headers, payload)
	raw, _ := json.Marshal(data)
	return status, data, raw, err
}

// geminiUsage renova o access token em 401 antes de reportar expirado (mesma
// mecânica do antigravityUsage — um único re-tento, sem loop).
func (u *UsageContracts) geminiUsage(r *http.Request, token string, connection map[string]any) map[string]any {
	result := u.geminiUsageResult(r, token, connection)
	if result["authExpired"] != true {
		return result
	}
	if refreshed := u.refreshUsageToken(r, "gemini-cli", connection); refreshed != "" {
		return u.geminiUsageResult(r, refreshed, connection)
	}
	return result
}

func (u *UsageContracts) geminiUsageResult(r *http.Request, token string, connection map[string]any) map[string]any {
	if token == "" {
		return map[string]any{"plan": "Free", "message": "Gemini CLI access token not available."}
	}
	metadata, _ := connection["providerSpecificData"].(map[string]any)
	project := cloudProject(firstNonEmpty(connection["projectId"], metadata["projectId"]))
	plan := "Free"
	if project == "" {
		_, info, _, err := u.googleJSON(r.Context(), "https://cloudcode-pa.googleapis.com/v1internal:loadCodeAssist", map[string]string{"Authorization": "Bearer " + token, "Content-Type": "application/json"}, map[string]any{"metadata": map[string]any{"ideType": 9, "platform": googlePlatform(), "pluginType": 2}})
		if err == nil {
			project = cloudProject(info["cloudaicompanionProject"])
			tier, _ := info["currentTier"].(map[string]any)
			if name := ttsString(tier["name"]); name != "" {
				plan = name
			}
		}
	}
	if project == "" {
		return map[string]any{"plan": plan, "message": "Gemini CLI project ID not available. Reconnect Gemini CLI, or configure a Google Cloud project with Gemini Code Assist access before checking quota."}
	}
	status, data, _, err := u.googleJSON(r.Context(), "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota", map[string]string{"Authorization": "Bearer " + token, "Content-Type": "application/json"}, map[string]any{"project": project})
	if err != nil {
		return map[string]any{"message": "Gemini CLI error: " + err.Error()}
	}
	if status == 401 {
		return map[string]any{"plan": plan, "message": fmt.Sprintf("Gemini CLI quota error (%d).", status), "authExpired": true}
	}
	if status < 200 || status >= 300 {
		return map[string]any{"plan": plan, "message": fmt.Sprintf("Gemini CLI quota error (%d).", status)}
	}
	quotas := map[string]any{}
	buckets, _ := data["buckets"].([]any)
	for _, raw := range buckets {
		bucket, _ := raw.(map[string]any)
		model := ttsString(bucket["modelId"])
		fraction, ok := bucket["remainingFraction"].(float64)
		if model == "" || !ok {
			continue
		}
		quotas[model] = fractionQuota(model, fraction, bucket["resetTime"], nil)
	}
	return map[string]any{"plan": plan, "quotas": quotas}
}

// refreshUsageToken renova o access token OAuth da conexão (mesma mecânica do
// envio do gateway) e persiste o patch. Devolve o token novo, ou vazio quando
// o provider não suporta refresh ou o refresh falhou. lookup nil: a tela de
// quota não marca fitness de pool (mesmo comportamento do grokUsage).
func (u *UsageContracts) refreshUsageToken(r *http.Request, provider string, connection map[string]any) string {
	if u.Connections == nil {
		return ""
	}
	patch, err := oauthRefreshPatch(r.Context(), u.Client, provider, connection, nil)
	if err != nil || patch == nil {
		return ""
	}
	if _, err := u.Connections.Update(r.Context(), ttsString(connection["id"]), patch); err != nil {
		return ""
	}
	return ttsString(patch["accessToken"])
}

// antigravityUsage replica o cálculo de quota do OmniRoute (open-sse/services/
// usage/antigravity.ts + antigravityWeeklyQuota.ts) com três fontes paralelas:
//
//  1. fetchAvailableModels — catálogo + quotaInfo por model (pode ficar stale:
//     o bucket continua cheio depois do consumo real);
//  2. retrieveUserQuota — consumo per-model, fonte da verdade pra família
//     Gemini; fallback pra models que só existem lá (Claude/GPT);
//  3. retrieveUserQuotaSummary — janelas por família (5h + weekly).
//
// Prioridade: retrieveUserQuota > quotaInfo. Normalização do OmniRoute: base
// 1000; "unlimited" só quando fraction ≥ 1 sem resetTime; fraction ausente é
// desconhecido (fail-open — o model sai do painel em vez de exibir 0%).
//
// O gate dos endpoints é o mesmo do discovery/chat (ag_headers.go): User-Agent
// do CLI + project no corpo. O project vem da conexão (igual ao translator);
// sem conexão, cai pro loadCodeAssist.
//
// Em 401 renova o access token e re-tenta uma vez (mesma mecânica do envio do
// gateway e do grokUsage): o scheduler proativo cobre o token de hora em hora,
// mas uma janela de falha transitória deixava a tela de quota presa em
// "authentication expired" até o próximo chat renovar reativamente.
func (u *UsageContracts) antigravityUsage(r *http.Request, token string, connection map[string]any) map[string]any {
	result := u.antigravityUsageResult(r, token, connection)
	if result["authExpired"] != true {
		return result
	}
	if refreshed := u.refreshUsageToken(r, "antigravity", connection); refreshed != "" {
		return u.antigravityUsageResult(r, refreshed, connection)
	}
	return result
}

func (u *UsageContracts) antigravityUsageResult(r *http.Request, token string, connection map[string]any) map[string]any {
	metadata, _ := connection["providerSpecificData"].(map[string]any)
	project := firstNonEmptyString(metadata["projectId"], metadata["quota_project_id"], connection["projectId"])
	headers := map[string]string{"Authorization": "Bearer " + token, "Content-Type": "application/json", "User-Agent": agUserAgent}
	// loadCodeAssist resolve o project (contas antigas sem projectId na
	// conexão) e traz plano + subscriptionInfo pro painel.
	_, subscription, _, _ := u.googleJSON(r.Context(), "https://cloudcode-pa.googleapis.com/v1internal:loadCodeAssist", headers, map[string]any{"metadata": map[string]any{"ideType": 9, "platform": googlePlatform(), "pluginType": 2}, "mode": 1})
	if project == "" {
		project = cloudProject(subscription["cloudaicompanionProject"])
	}
	payload := map[string]any{}
	if project != "" {
		payload["project"] = project
	}
	status, data, raw, err := u.googleJSON(r.Context(), agModelsURL, headers, payload)
	if err != nil {
		return map[string]any{"message": "Antigravity error: " + err.Error()}
	}
	if status == 403 {
		message := "Antigravity quota API access forbidden. Chat may still work."
		ineligible := antigravityIneligible.Match(raw)
		if ineligible {
			message = "Antigravity: Google marked this account ineligible (VALIDATION_REQUIRED). Chat will fail with 403 until the account owner completes verification."
		}
		return map[string]any{"message": message, "quotas": map[string]any{}, "ineligible": ineligible}
	}
	if status == 401 {
		return map[string]any{"message": "Antigravity quota API authentication expired. Chat may still work.", "quotas": map[string]any{}, "authExpired": true}
	}
	if status < 200 || status >= 300 {
		return map[string]any{"message": fmt.Sprintf("Antigravity error: Antigravity API error: %d", status)}
	}
	quotas := map[string]any{}
	models, _ := data["models"].(map[string]any)
	// Fonte 1: quotaInfo do catálogo. Allowlist agora é dinâmica — fail-open
	// do OmniRoute: todo model público com quotaInfo entra (a lista estática
	// antiga não tinha nenhum gemini-4 e o gating real de catálogo acontece
	// no próprio upstream via UA/project). remainingFraction ausente é
	// "desconhecido": o model sai do painel em vez de exibir 0%.
	for model, rawModel := range models {
		info, _ := rawModel.(map[string]any)
		quota, ok := info["quotaInfo"].(map[string]any)
		if !ok || info["isInternal"] == true {
			continue
		}
		fraction, ok := quota["remainingFraction"].(float64)
		if !ok {
			continue
		}
		quotas[model] = agFractionEntry(fraction, quota["resetTime"], info["displayName"])
	}
	// Fonte 2: retrieveUserQuota per-model — fonte da verdade do consumo
	// Gemini; sobrescreve quotaInfo stale e inclui buckets fora do catálogo.
	for model, entry := range u.antigravityPerModelQuota(r.Context(), token, project) {
		quotas[model] = entry
	}
	// Fonte 3: summary por família (Gemini / Claude&GPT / Argon, janelas 5h+weekly).
	for key, value := range u.antigravityWeekly(r.Context(), token, project, headers, payload) {
		quotas[key] = value
	}
	current, _ := subscription["currentTier"].(map[string]any)
	return map[string]any{"plan": firstNonEmpty(current["name"], "Unknown"), "quotas": quotas, "subscriptionInfo": subscription}
}

// agFractionEntry normaliza um fraction (0..1) pro formato do painel — base
// 1000 (reuso da regra do fractionQuota) com a semântica do OmniRoute:
// unlimited só quando fraction ≥ 1 SEM resetTime (ex. buckets tab_*); fraction
// reportado ≥ 1 com reset é janela cheia, não ausência de limite.
func agFractionEntry(fraction float64, reset any, display any) map[string]any {
	if fraction < 0 {
		fraction = 0
	} else if fraction > 1 {
		fraction = 1
	}
	entry := fractionQuota("", fraction, reset, display)
	if fraction >= 1 && reset == nil {
		entry["unlimited"] = true
		entry["used"] = 0.0
		entry["total"] = 0.0
		entry["remainingPercentage"] = 100.0
	}
	return entry
}

// antigravityPerModelQuota consome v1internal:retrieveUserQuota (buckets
// per-model: modelId/remainingFraction/resetTime). Falha silenciosa: sem a
// RPC, o painel cai pro quotaInfo do catálogo — igual ao fallback do OmniRoute.
func (u *UsageContracts) antigravityPerModelQuota(ctx context.Context, token, project string) map[string]any {
	payload := map[string]any{}
	if project != "" {
		payload["project"] = project
	}
	status, data, _, err := u.googleJSON(ctx, "https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota", map[string]string{"Authorization": "Bearer " + token, "Content-Type": "application/json", "User-Agent": agUserAgent}, payload)
	if err != nil || status < 200 || status >= 300 {
		return map[string]any{}
	}
	quotas := map[string]any{}
	buckets, _ := data["buckets"].([]any)
	for _, raw := range buckets {
		bucket, _ := raw.(map[string]any)
		model := ttsString(bucket["modelId"])
		if model == "" || strings.HasPrefix(model, "tab_") {
			continue
		}
		if fraction, ok := bucket["remainingFraction"].(float64); ok {
			quotas[model] = agFractionEntry(fraction, bucket["resetTime"], nil)
		}
	}
	return quotas
}

func (u *UsageContracts) antigravityWeekly(ctx context.Context, token, project string, headers map[string]string, payload map[string]any) map[string]any {
	key := sha256.Sum256([]byte(token + "::" + project))
	u.claudeMu.Lock()
	if u.weeklyUsage == nil {
		u.weeklyUsage = map[[32]byte]*weeklyUsageEntry{}
	}
	current := u.weeklyUsage[key]
	if current != nil && current.done == nil && current.expires.After(time.Now()) {
		result := current.result
		u.claudeMu.Unlock()
		return cloneUsageMap(result)
	}
	if current != nil && current.done != nil {
		done := current.done
		u.claudeMu.Unlock()
		select {
		case <-ctx.Done():
			return map[string]any{}
		case <-done:
			return cloneUsageMap(current.result)
		}
	}
	entry := &weeklyUsageEntry{done: make(chan struct{})}
	u.weeklyUsage[key] = entry
	u.claudeMu.Unlock()
	status, data, _, err := u.googleJSON(ctx, "https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary", headers, payload)
	result := map[string]any{}
	if err == nil && status >= 200 && status < 300 {
		result = parseWeeklyQuota(data)
	}
	u.claudeMu.Lock()
	entry.result = cloneUsageMap(result)
	close(entry.done)
	entry.done = nil
	if len(result) > 0 {
		entry.expires = time.Now().Add(3 * time.Minute)
	} else if u.weeklyUsage[key] == entry {
		delete(u.weeklyUsage, key)
	}
	u.claudeMu.Unlock()
	return cloneUsageMap(result)
}

// parseWeeklyQuota converte retrieveUserQuotaSummary (quotas por FAMÍLIA,
// não por model) pro formato do painel. Porte do parser do OmniRoute
// (antigravityWeeklyQuota.ts): grupos Gemini / Claude&GPT / Gemini 4 Argon
// (Argon detectado pelo texto do grupo OU pelo bucketId — só aparece em
// Ultra), janelas independentes weekly e 5h, e fraction podendo vir aninhado
// em bucket.remaining.remainingFraction (defensivo: uma mudança silenciosa do
// envelope upstream não pode apagar as famílias do painel). Cada família
// expõe até duas janelas, chaveadas <family>_<window>.
func parseWeeklyQuota(data map[string]any) map[string]any {
	groups, _ := data["groups"].([]any)
	if groups == nil {
		summary, _ := data["quotaSummary"].(map[string]any)
		groups, _ = summary["groups"].([]any)
	}
	result := map[string]any{}
	argonGroup := regexp.MustCompile(`(?i)\bargon\b|\bgemini[\s_-]*4\b`)
	argonBucket := regexp.MustCompile(`(?i)\bargon\b|gemini-argon-wtu-weekly`)
	weeklyPattern := regexp.MustCompile(`(?i)\bweekly\b|\b7d\b|seven[-\s]?day`)
	sessionPattern := regexp.MustCompile(`(?i)\b5h\b|five[-\s]?hour|session`)
	for _, raw := range groups {
		group, _ := raw.(map[string]any)
		name := ttsString(group["displayName"])
		text := strings.ToLower(ttsString(group["id"]) + " " + name)
		buckets, _ := group["buckets"].([]any)
		family := ""
		switch {
		case argonGroup.MatchString(text):
			family = "gemini_4_argon"
		case strings.Contains(strings.ToLower(name), "gemini"):
			family = "gemini"
		case regexp.MustCompile(`(?i)claude|gpt`).MatchString(name):
			family = "claude_gpt"
		default:
			// Sem match no texto do grupo: Argon pode aparecer só nos
			// bucketIds (Gemini 4 Argon em contas Ultra).
			for _, rawBucket := range buckets {
				bucket, _ := rawBucket.(map[string]any)
				if argonBucket.MatchString(strings.ToLower(ttsString(bucket["bucketId"]) + " " + ttsString(bucket["displayName"]))) {
					family = "gemini_4_argon"
					break
				}
			}
			if family == "" {
				continue
			}
		}
		for _, rawBucket := range buckets {
			bucket, _ := rawBucket.(map[string]any)
			if bucket["disabled"] == true {
				continue
			}
			// fraction direto (envelope atual) ou aninhado em bucket.remaining.
			remaining, _ := bucket["remaining"].(map[string]any)
			fraction, ok := bucket["remainingFraction"].(float64)
			if !ok {
				fraction, ok = remaining["remainingFraction"].(float64)
			}
			if !ok {
				continue
			}
			// Janela: campo window explícito; fallback textual no bucketId/
			// displayName (formatos antigos).
			windowText := strings.ToLower(ttsString(bucket["window"]) + " " + ttsString(bucket["bucketId"]) + " " + ttsString(bucket["displayName"]))
			window := ""
			switch {
			case weeklyPattern.MatchString(windowText):
				window = "weekly"
			case sessionPattern.MatchString(windowText):
				window = "session"
			}
			if window == "" {
				continue
			}
			key := family + "_" + window
			if result[key] != nil {
				continue
			}
			reset := bucket["resetTime"]
			if reset == nil {
				reset = remaining["resetTime"]
			}
			label := map[string]string{
				"gemini":         "Gemini",
				"gemini_4_argon": "Gemini 4 Argon",
				"claude_gpt":     "Claude & GPT",
			}[family] + agWindowLabel(window)
			// unlimited (OmniRoute): fraction ≥ 1 SEM resetTime — fraction 1
			// com reset é só janela não consumida, não ausência de limite.
			entry := fractionQuota(key, fraction, reset, label)
			if fraction >= 1 && reset == nil {
				entry["unlimited"] = true
				entry["used"] = 0.0
				entry["total"] = 0.0
			}
			result[key] = entry
		}
	}
	return result
}

// agWindowLabel converte a janela crua ("weekly"/"session") pro rótulo do painel.
func agWindowLabel(window string) string {
	if window == "session" {
		return " (5h)"
	}
	return " (Weekly)"
}

func cloneUsageMap(value map[string]any) map[string]any {
	raw, _ := json.Marshal(value)
	var result map[string]any
	json.Unmarshal(raw, &result)
	if result == nil {
		return map[string]any{}
	}
	return result
}
