package providers

import (
	_ "embed"
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

//go:embed pricing.json
var pricingJSON []byte

//go:embed pricing_fallback.json
var pricingFallbackJSON []byte

// FallbackPricing ports getPricingForModel from open-sse/providers/pricing.js:
// canonical model first, then ordered case-insensitive glob patterns.
func FallbackPricing(model string) map[string]any {
	var catalog struct {
		Models   map[string]map[string]any `json:"models"`
		Patterns []struct {
			Pattern string         `json:"pattern"`
			Pricing map[string]any `json:"pricing"`
		} `json:"patterns"`
	}
	if err := json.Unmarshal(pricingFallbackJSON, &catalog); err != nil {
		panic(err)
	}
	base := model[strings.LastIndex(model, "/")+1:]
	if price := catalog.Models[base]; price != nil {
		return price
	}
	if price := catalog.Models[model]; price != nil {
		return price
	}
	for _, item := range catalog.Patterns {
		pattern := "(?i)^" + strings.ReplaceAll(regexp.QuoteMeta(item.Pattern), `\*`, ".*") + "$"
		match := regexp.MustCompile(pattern)
		if match.MatchString(base) || match.MatchString(model) {
			return item.Pricing
		}
	}
	return nil
}

// FallbackPricingAt resolve o preço no instante do request e aplica a tarifa de
// pico quando o catálogo declara janelas horárias (ex.: DeepSeek cobra 2x em
// horários específicos). O catálogo é a fonte; nenhum provider é hardcoded aqui.
//
// shortcut: feriados não são modelados (DeepSeek exclui feriados chineses),
// então dias de feriado caem na tarifa off-peak; revisar se o custo desviar.
func FallbackPricingAt(model string, at time.Time) map[string]any {
	price := FallbackPricing(model)
	if price == nil {
		return nil
	}
	peak, _ := price["peak"].(map[string]any)
	if peak == nil || !inPeakWindow(price, at.UTC()) {
		return price
	}
	effective := make(map[string]any, len(price))
	for key, value := range price {
		effective[key] = value
	}
	for key, value := range peak {
		effective[key] = value
	}
	return effective
}

func inPeakWindow(price map[string]any, at time.Time) bool {
	windows, _ := price["peakWindowsUtc"].([]any)
	if len(windows) == 0 {
		return false
	}
	if weekdays, _ := price["peakWeekdaysOnly"].(bool); weekdays {
		if at.Weekday() == time.Saturday || at.Weekday() == time.Sunday {
			return false
		}
	}
	minute := at.Hour()*60 + at.Minute()
	for _, raw := range windows {
		window, _ := raw.(map[string]any)
		start, okStart := clockMinute(window["start"])
		end, okEnd := clockMinute(window["end"])
		if okStart && okEnd && minute >= start && minute < end {
			return true
		}
	}
	return false
}

func clockMinute(value any) (int, bool) {
	text, ok := value.(string)
	if !ok {
		return 0, false
	}
	parsed, err := time.Parse("15:04", text)
	if err != nil {
		return 0, false
	}
	return parsed.Hour()*60 + parsed.Minute(), true
}

func DefaultPricing() map[string]any {
	var value map[string]any
	if err := json.Unmarshal(pricingJSON, &value); err != nil {
		panic(err)
	}
	return value
}
