package subsusage

import (
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

const (
	healthBucket = 5 * time.Minute
	healthKeep   = 24 * time.Hour
)

// healthStat is one healthBucket of proxied model requests as the clients saw them, plus the
// upstream attempts that failed (a request that the proxy retried on another account still
// succeeds for the client but leaves a failed attempt here).
type healthStat struct {
	T        int64 `json:"t"`
	Requests int   `json:"req"`
	// Failed counts responses with status >= 500 or 429 sent to the client.
	Failed int `json:"fail"`
	// ClientErr counts other 4xx responses (bad requests, auth), usually the client's own doing.
	ClientErr int `json:"cerr,omitempty"`
	// TTFT is the summed time to first token of successful requests, in ms, over TTFTn requests.
	TTFT    float64 `json:"ttft,omitempty"`
	TTFTn   int     `json:"ttft_n,omitempty"`
	TTFTMax float64 `json:"ttft_max,omitempty"`
	// Upstream maps reason -> failed upstream attempts; Accounts maps auth ID -> failed attempts.
	Upstream map[string]int `json:"up,omitempty"`
	Accounts map[string]int `json:"acc,omitempty"`
}

type healthLog struct {
	mu      sync.Mutex
	buckets map[int64]*healthStat
	now     func() time.Time
}

var health = &healthLog{buckets: make(map[int64]*healthStat)}

func (h *healthLog) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

// bucket returns the bucket for at, creating it and dropping expired ones; h.mu must be held.
func (h *healthLog) bucket(at time.Time) *healthStat {
	k := at.Truncate(healthBucket).Unix()
	b := h.buckets[k]
	if b == nil {
		b = &healthStat{T: k}
		h.buckets[k] = b
		cut := h.clock().Add(-healthKeep).Unix()
		for t := range h.buckets {
			if t < cut {
				delete(h.buckets, t)
			}
		}
	}
	return b
}

func (h *healthLog) recordResponse(status int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	b := h.bucket(h.clock())
	b.Requests++
	switch {
	case status >= 500 || status == http.StatusTooManyRequests:
		b.Failed++
	case status >= 400:
		b.ClientErr++
	}
}

func (h *healthLog) recordTTFT(at time.Time, d time.Duration) {
	if d <= 0 {
		return
	}
	ms := float64(d) / float64(time.Millisecond)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.clock().Sub(at) > healthKeep {
		return
	}
	b := h.bucket(at)
	b.TTFT += ms
	b.TTFTn++
	if ms > b.TTFTMax {
		b.TTFTMax = ms
	}
}

func (h *healthLog) recordUpstream(authID, reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	b := h.bucket(h.clock())
	if b.Upstream == nil {
		b.Upstream = make(map[string]int)
	}
	b.Upstream[reason]++
	if authID != "" {
		if b.Accounts == nil {
			b.Accounts = make(map[string]int)
		}
		b.Accounts[authID]++
	}
}

func (h *healthLog) view() []healthStat {
	h.mu.Lock()
	defer h.mu.Unlock()
	cut := h.clock().Add(-healthKeep).Unix()
	out := make([]healthStat, 0, len(h.buckets))
	for t, b := range h.buckets {
		if t >= cut {
			c := *b
			c.Upstream = copyCounts(b.Upstream)
			c.Accounts = copyCounts(b.Accounts)
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].T < out[j].T })
	return out
}

func copyCounts(m map[string]int) map[string]int {
	if m == nil {
		return nil
	}
	c := make(map[string]int, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

// isModelRequest reports whether a request is a proxied Claude model call (the Anthropic Messages
// API), the traffic the health card and the failed-attempt hook both cover.
func isModelRequest(method, path string) bool {
	return method == http.MethodPost && strings.TrimSuffix(path, "/") == "/v1/messages"
}

// HealthMiddleware counts the status of every proxied model request for the /usage health card.
func HealthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request == nil || c.Request.URL == nil || !isModelRequest(c.Request.Method, c.Request.URL.Path) {
			c.Next()
			return
		}
		c.Next()
		health.recordResponse(c.Writer.Status())
	}
}

var (
	reAuthFile   = regexp.MustCompile(`auth_file=(\S+)`)
	reLeadStatus = regexp.MustCompile(`^\s*(\d{3}) \|`)
)

// failureReason buckets an upstream error into a short reason the page can label.
func failureReason(status int, errText string) string {
	e := strings.ToLower(errText)
	switch {
	case strings.Contains(e, "lookup ") || strings.Contains(e, "no such host") || strings.Contains(e, "server misbehaving"):
		return "dns"
	case status == http.StatusTooManyRequests:
		return "429"
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "auth"
	case status == 529 || status == http.StatusServiceUnavailable:
		return "overloaded"
	case status >= 500:
		return "5xx"
	case strings.Contains(e, "timed out") || strings.Contains(e, "timeout") || strings.Contains(e, "deadline exceeded"):
		return "timeout"
	case strings.Contains(e, "unreachable") || strings.Contains(e, "refused") || strings.Contains(e, "reset by peer") || strings.Contains(e, "eof") || strings.Contains(e, "broken pipe"):
		return "network"
	case status >= 400:
		return strconv.Itoa(status)
	}
	return "other"
}

// healthHook reads the conductor's "upstream execution failed" warnings, the one place every
// failed upstream attempt is reported with its account and error.
type healthHook struct{}

func (healthHook) Levels() []log.Level { return []log.Level{log.WarnLevel} }

func (healthHook) Fire(e *log.Entry) error {
	msg := e.Message
	i := strings.Index(msg, "upstream execution failed:")
	if i < 0 || !strings.Contains(msg, "provider=claude") {
		return nil
	}
	status := 0
	if m := reLeadStatus.FindStringSubmatch(msg[:i]); m != nil {
		status, _ = strconv.Atoi(m[1])
	}
	authID := ""
	if m := reAuthFile.FindStringSubmatch(msg); m != nil {
		authID = m[1]
	}
	errText := ""
	if j := strings.Index(msg, " err="); j >= 0 {
		errText = msg[j+5:]
	}
	health.recordUpstream(authID, failureReason(status, errText))
	return nil
}

var hookOnce sync.Once

func installHealthHook() { hookOnce.Do(func() { log.AddHook(healthHook{}) }) }
