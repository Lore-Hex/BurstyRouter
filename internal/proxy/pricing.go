package proxy

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/Lore-Hex/BurstyRouter/internal/policy"
)

func (s *Server) localSavingsPrice(ctx context.Context, decision policy.Decision) priceQuote {
	for _, model := range s.localPricingCandidates(decision) {
		if quote, ok := s.catalogPrice(ctx, model); ok {
			return quote
		}
	}
	return priceQuote{}
}

func (s *Server) localPricingCandidates(decision policy.Decision) []string {
	var candidates []string
	add := func(model string) {
		model = strings.TrimSpace(model)
		if model == "" {
			return
		}
		for _, existing := range candidates {
			if existing == model {
				return
			}
		}
		candidates = append(candidates, model)
	}
	if decision.AliasKey != "" {
		add(decision.AliasKey)
	}
	if trustedRouterModelCandidate(decision.View.Model) {
		add(decision.View.Model)
	}
	if s.cfg.SavingsReference != "" {
		add(s.cfg.SavingsReference)
	}
	return candidates
}

func trustedRouterModelCandidate(model string) bool {
	model = strings.TrimSpace(model)
	return model != "" && !strings.HasPrefix(model, "local/")
}

func (s *Server) cloudSavingsPrice(ctx context.Context, model string) priceQuote {
	quote, _ := s.catalogPrice(ctx, model)
	return quote
}

func (s *Server) catalogPrice(ctx context.Context, model string) (priceQuote, bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		return priceQuote{}, false
	}
	// Pricing runs after the response has been served, when the request context
	// is often already canceled (the client is gone). Detach from cancellation
	// so the catalog fetch still succeeds, with its own timeout so a record can
	// never hang the handler.
	fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), catalogTimeout)
	defer cancel()
	models, err := s.cachedTrustedRouterModels(fetchCtx)
	if err != nil {
		return priceQuote{}, false
	}
	for _, entry := range models {
		id, _ := entry["id"].(string)
		if id != model {
			continue
		}
		quote, ok := priceFromModel(entry)
		if !ok {
			return priceQuote{}, false
		}
		quote.Reference = id
		return quote, true
	}
	return priceQuote{}, false
}

func priceFromModel(entry map[string]any) (priceQuote, bool) {
	pricing, _ := entry["pricing"].(map[string]any)
	prompt, promptOK := priceMicroPerToken(pricing, entry, "prompt", "prompt_max", "prompt_usd_per_mtok", "input_usd_per_mtok", "input_price_per_mtok")
	completion, completionOK := priceMicroPerToken(pricing, entry, "completion", "completion_max", "completion_usd_per_mtok", "output_usd_per_mtok", "output_price_per_mtok")
	if !promptOK || !completionOK {
		return priceQuote{}, false
	}
	return priceQuote{
		PromptMicroPerToken:     prompt,
		CompletionMicroPerToken: completion,
		Priced:                  true,
	}, true
}

func priceMicroPerToken(pricing map[string]any, entry map[string]any, tokenPriceKey, maxTokenPriceKey string, perMTokKeys ...string) (float64, bool) {
	for _, key := range perMTokKeys {
		if value, ok := numericField(pricing, key); ok {
			return value, true
		}
		if value, ok := numericField(entry, key); ok {
			return value, true
		}
	}
	if value, ok := numericField(pricing, tokenPriceKey); ok {
		return value * 1_000_000, true
	}
	if value, ok := numericField(pricing, maxTokenPriceKey); ok {
		return value * 1_000_000, true
	}
	return 0, false
}

func numericField(fields map[string]any, key string) (float64, bool) {
	if fields == nil {
		return 0, false
	}
	switch value := fields[key].(type) {
	case float64:
		return value, true
	case int:
		return float64(value), true
	case int64:
		return float64(value), true
	case json.Number:
		parsed, err := strconv.ParseFloat(string(value), 64)
		return parsed, err == nil
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}
