package subsusage

import (
	"regexp"
	"strings"
)

// modelPrice holds Anthropic first-party API list prices in USD per million tokens.
// Cache writes are derived from the base input price: 1.25x for the 5-minute TTL and 2x for
// the 1-hour TTL. Cache reads are listed per model because their ratio to input varies.
type modelPrice struct {
	Input     float64
	Output    float64
	CacheRead float64
	// FastMultiplier scales every rate for fast mode (usage.speed "fast"); 0 means fast mode
	// pricing is unknown for the model and fast requests are left unpriced.
	FastMultiplier float64
	// LongInput, LongOutput and LongCacheRead apply when the prompt (fresh input + cache read
	// + cache write) exceeds LongThreshold tokens; 0 means no long-prompt tier.
	LongThreshold int64
	LongInput     float64
	LongOutput    float64
	LongCacheRead float64
}

// Prices from the claude-api reference (cached 2026-11-03). Keys are normalized model IDs.
var modelPrices = map[string]modelPrice{
	"claude-fable-5-2":  {Input: 8, Output: 40, CacheRead: 0.20, FastMultiplier: 2},
	"claude-mythos-5-2": {Input: 8, Output: 40, CacheRead: 0.20, FastMultiplier: 2},
	"claude-fable-5-1":  {Input: 10, Output: 50, CacheRead: 0.25},
	"claude-mythos-5-1": {Input: 10, Output: 50, CacheRead: 0.25},
	"claude-fable-5":    {Input: 10, Output: 50, CacheRead: 1},
	"claude-mythos-5":   {Input: 10, Output: 50, CacheRead: 1},
	"claude-opus-5-5":   {Input: 4, Output: 20, CacheRead: 0.20, FastMultiplier: 2},
	"claude-opus-5":     {Input: 5, Output: 25, CacheRead: 0.50, FastMultiplier: 2},
	"claude-opus-4-8":   {Input: 5, Output: 25, CacheRead: 0.50},
	"claude-opus-4-7":   {Input: 5, Output: 25, CacheRead: 0.50},
	"claude-opus-4-6":   {Input: 5, Output: 25, CacheRead: 0.50},
	"claude-sonnet-5-5": {Input: 2, Output: 10, CacheRead: 0.20},
	"claude-sonnet-5":   {Input: 2, Output: 10, CacheRead: 0.20},
	"claude-sonnet-4-6": {Input: 3, Output: 15, CacheRead: 0.30},
	"claude-haiku-5-5": {Input: 0.10, Output: 0.50, CacheRead: 0.01,
		LongThreshold: 100_000, LongInput: 0.50, LongOutput: 2.50, LongCacheRead: 0.05},
	"claude-haiku-4-5": {Input: 1, Output: 5, CacheRead: 0.10},
}

var (
	modelDateSuffix = regexp.MustCompile(`-20\d{6}$`)
	modelVersionDot = regexp.MustCompile(`(\d)\.(\d)`)
)

// normalizeModel maps a reported model name to a modelPrices key: lower case, without
// provider prefixes ("anthropic.", "us.anthropic."), context suffixes ("[1m]"), Vertex
// "@date" or "-YYYYMMDD" snapshot suffixes and with dotted versions as dashes.
func normalizeModel(name string) string {
	m := strings.ToLower(strings.TrimSpace(name))
	if i := strings.LastIndex(m, "anthropic."); i >= 0 {
		m = m[i+len("anthropic."):]
	}
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	if i := strings.IndexAny(m, "[@:"); i >= 0 {
		m = m[:i]
	}
	m = strings.TrimSuffix(m, "-latest")
	m = modelDateSuffix.ReplaceAllString(m, "")
	return modelVersionDot.ReplaceAllString(m, "$1-$2")
}

// Pricing key flags appended to the normalized model.
const (
	flagFast = "+fast"
	flagLong = "+long"
)

// pricingKey is the per-model bucket a request is recorded under. It carries the flags that
// change the rate, so the cost can be computed later from the stored token counts.
func pricingKey(model, speed string, prompt int64) string {
	key := normalizeModel(model)
	if key == "" {
		key = "unknown"
	}
	if strings.EqualFold(strings.TrimSpace(speed), "fast") {
		key += flagFast
	}
	if p, ok := modelPrices[strings.TrimSuffix(key, flagFast)]; ok && p.LongThreshold > 0 && prompt > p.LongThreshold {
		key += flagLong
	}
	return key
}

// CostBreakdown is an API-equivalent cost estimate in USD.
type CostBreakdown struct {
	Total      float64 `json:"total"`
	Input      float64 `json:"input"`       // fresh input
	CacheRead  float64 `json:"cache_read"`  // cache reads
	CacheWrite float64 `json:"cache_write"` // cache writes, both TTLs
	Output     float64 `json:"output"`
	// NoCache is what the same tokens would cost with every cache read and write billed as
	// fresh input. NoCache - Total is the net saving from caching (negative when writes
	// outweigh reads).
	NoCache float64 `json:"no_cache"`
}

func (c *CostBreakdown) add(o CostBreakdown) {
	c.Total += o.Total
	c.Input += o.Input
	c.CacheRead += o.CacheRead
	c.CacheWrite += o.CacheWrite
	c.Output += o.Output
	c.NoCache += o.NoCache
}

// priceOf returns the cost of the tokens of one pricing key and whether the key is priced.
func priceOf(key string, s *modelStat) (CostBreakdown, bool) {
	base := key
	fast := false
	long := false
	for {
		switch {
		case strings.HasSuffix(base, flagLong):
			base, long = strings.TrimSuffix(base, flagLong), true
			continue
		case strings.HasSuffix(base, flagFast):
			base, fast = strings.TrimSuffix(base, flagFast), true
			continue
		}
		break
	}
	p, ok := modelPrices[base]
	if !ok || (fast && p.FastMultiplier == 0) {
		return CostBreakdown{}, false
	}
	in, out, read := p.Input, p.Output, p.CacheRead
	if long && p.LongThreshold > 0 {
		in, out, read = p.LongInput, p.LongOutput, p.LongCacheRead
	}
	if fast {
		in, out, read = in*p.FastMultiplier, out*p.FastMultiplier, read*p.FastMultiplier
	}
	const perToken = 1e-6
	c := CostBreakdown{
		Input:      float64(s.Input) * in * perToken,
		CacheRead:  float64(s.CacheRead) * read * perToken,
		CacheWrite: (float64(s.CacheWrite5m)*1.25 + float64(s.CacheWrite1h)*2) * in * perToken,
		Output:     float64(s.Output) * out * perToken,
	}
	c.Total = c.Input + c.CacheRead + c.CacheWrite + c.Output
	c.NoCache = c.Input + c.Output + float64(s.CacheRead+s.CacheWrite5m+s.CacheWrite1h)*in*perToken
	return c, true
}
