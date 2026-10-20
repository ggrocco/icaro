package usage

import (
	"sort"
	"strings"
)

// Price is USD per million tokens.
type Price struct {
	Input, Output float64
}

// Cache tokens are billed relative to input: writes at 1.25x, reads at 0.1x.
const (
	cacheWriteFactor = 1.25
	cacheReadFactor  = 0.10
)

// Prices maps model-id prefixes to list prices. These are ESTIMATES copied
// from the provider's public pricing page; they ignore discounts, batch
// pricing and plan subscriptions, so a cost computed from them is labelled
// an estimate everywhere. Models not listed get no cost at all.
var Prices = map[string]Price{
	"claude-opus-4-5":   {Input: 5, Output: 25},
	"claude-opus-4":     {Input: 15, Output: 75},
	"claude-sonnet-4":   {Input: 3, Output: 15},
	"claude-3-7-sonnet": {Input: 3, Output: 15},
	"claude-3-5-sonnet": {Input: 3, Output: 15},
	"claude-haiku-4-5":  {Input: 1, Output: 5},
	"claude-3-5-haiku":  {Input: 0.8, Output: 4},
}

// PriceFor returns the longest matching prefix entry.
func PriceFor(model string) (Price, bool) {
	keys := make([]string, 0, len(Prices))
	for k := range Prices {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	for _, k := range keys {
		if strings.HasPrefix(model, k) {
			return Prices[k], true
		}
	}
	return Price{}, false
}

// EstimateCost prices a record, or returns nil when the model is unpriced.
func EstimateCost(r Record) *float64 {
	p, ok := PriceFor(r.Model)
	if !ok {
		return nil
	}
	c := (float64(r.InputTokens)*p.Input +
		float64(r.OutputTokens)*p.Output +
		float64(r.CacheWriteTokens)*p.Input*cacheWriteFactor +
		float64(r.CacheReadTokens)*p.Input*cacheReadFactor) / 1e6
	return &c
}
