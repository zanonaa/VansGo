package providers

import (
	"testing"
	"time"
)

func TestFallbackPricingJSParity(t *testing.T) {
	for _, tc := range []struct {
		model         string
		input, output float64
	}{
		{"gpt-6.1-sol", 2, 10},
		{"vendor/gpt-6.1-sol", 2, 10},
		// DeepSeek oficial (off-peak): api-docs.deepseek.com/quick_start/pricing
		{"deepseek-flash", 0.15, 0.6},
		{"deepseek-v4-flash", 0.15, 0.6},
		{"deepseek-v4-pro", 0.66, 1.98},
		// Z.AI oficial: docs.z.ai/guides/overview/pricing
		{"glm-5.3", 1.4, 4.4},
		{"glm-4.7", 0.6, 2.2},
		{"glm-4.7-flash", 0, 0},
	} {
		price := FallbackPricing(tc.model)
		if price["input"] != tc.input || price["output"] != tc.output {
			t.Fatalf("%s: %v", tc.model, price)
		}
	}
	if FallbackPricing("totally-unknown-model") != nil {
		t.Fatal("unknown model should have no price")
	}
}

// DeepSeek cobra 2x em dias úteis 01:00–04:00 e 06:00–10:00 UTC; fora disso
// (e fins de semana) metade. Janelas vêm do catálogo, não do código.
func TestFallbackPricingPeakWindows(t *testing.T) {
	at := func(day int, hour, minute int) time.Time {
		return time.Date(2026, time.November, day, hour, minute, 0, 0, time.UTC)
	}
	for _, tc := range []struct {
		name   string
		when   time.Time
		expect float64
	}{
		{"segunda 02:00 UTC = pico", at(2, 2, 0), 0.3},
		{"segunda 01:00 UTC = pico (início inclusivo)", at(2, 1, 0), 0.3},
		{"segunda 04:00 UTC = off-peak (fim exclusivo)", at(2, 4, 0), 0.15},
		{"segunda 09:30 UTC = pico", at(2, 9, 30), 0.3},
		{"segunda 10:00 UTC = off-peak", at(2, 10, 0), 0.15},
		{"sábado 02:00 UTC = off-peak (fim de semana)", at(7, 2, 0), 0.15},
		{"domingo 09:00 UTC = off-peak", at(8, 9, 0), 0.15},
	} {
		price := FallbackPricingAt("deepseek-flash", tc.when)
		if price["input"] != tc.expect || price["output"] != tc.expect*4 {
			t.Fatalf("%s: esperava input %.3f, veio %v", tc.name, tc.expect, price)
		}
	}
	if FallbackPricingAt("deepseek-flash", at(2, 2, 0))["cached"] != 0.006 {
		t.Fatal("cache hit deve dobrar no pico")
	}
}
