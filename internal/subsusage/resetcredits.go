package subsusage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// Claude reset credits ("cedar_ember"): banked credits that clear an account's current rate-limit
// windows. The flow mirrors T3 Code's Claude driver: the usage endpoint lists the credits when
// called with cedar_ember=1, and one is redeemed with a POST to the organization's
// reset_rate_limits endpoint, carrying the grant id and an idempotency request id.

const resetCreditProgram = "cedar_ember"

// claudeUsagePollURL is the usage URL the tracker polls; cedar_ember=1 adds the reset credits to
// the same response, so reading them costs no extra request.
var claudeUsagePollURL = claudeUsageURL + "?cedar_ember=1"

// claudeAPIBase is the host of the reset endpoint; a variable so tests can point it elsewhere.
var claudeAPIBase = "https://api.anthropic.com"

var (
	grantIDPattern   = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)
	requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	orgIDPattern     = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
)

// Reset kinds, in display order, grouped like Claude Code (and T3 Code 0.1.9-nightly.4501): a
// grant that gives back weekly usage is a Fable reset, else its declared type, else what it clears.
var resetKinds = []struct{ kind, label, desc string }{
	{"session", "Reset de 5h", "Recarrega o limite de 5h."},
	{"weekly", "Reset semanal", "Recarrega o limite semanal."},
	{"fable", "Reset do Fable", "Recarrega a cota do Fable e devolve o que o Fable gastou do limite semanal. Não reseta a semana inteira."},
	{"full", "Reset completo", "Recarrega os limites de 5h e semanal."},
}

// ResetCredits is the reset-credit balance of a Claude account.
type ResetCredits struct {
	Available     int           `json:"available"`
	NextExpiresAt *time.Time    `json:"next_expires_at,omitempty"`
	Options       []ResetOption `json:"options,omitempty"`
}

// ResetOption is one kind of reset the account can redeem, backed by its first grant.
type ResetOption struct {
	Kind        string     `json:"kind"`
	Label       string     `json:"label"`
	Description string     `json:"description"`
	Available   int        `json:"available"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	// grantID is the grant redeemed for this kind; kept server-side.
	grantID string
}

func grantKind(g ojson.Result) string {
	if g.Get("gives_back_weekly").Bool() {
		return "fable"
	}
	switch t := g.Get("type").String(); t {
	case "session", "weekly", "full":
		return t
	}
	session, other := false, false
	for _, c := range g.Get("clears").Array() {
		if c.String() == "five_hour" {
			session = true
		} else {
			other = true
		}
	}
	switch {
	case session && !other:
		return "session"
	case other && !session:
		return "weekly"
	}
	return "full"
}

// parseResetCredits reads the cedar_ember block of a usage body. It returns nil when the block is
// missing or the account is not eligible. Only grants usable now, not paused and not expired count,
// and nothing is redeemable unless the grant the API picks next is among them (as T3 Code does).
func parseResetCredits(body []byte, now time.Time) *ResetCredits {
	block := ojson.GetBytes(body, "cedar_ember")
	if !block.IsObject() || !block.Get("eligible").Bool() {
		return nil
	}
	nextID := block.Get("next_grant_id").String()
	rc := &ResetCredits{}
	total := 0
	found := false
	byKind := map[string]*ResetOption{}
	for _, g := range block.Get("grants").Array() {
		id := g.Get("id").String()
		left := g.Get("resets_left")
		if !grantIDPattern.MatchString(id) || !left.Exists() || left.Int() < 0 {
			continue
		}
		if g.Get("paused").Bool() || !g.Get("usable_now").Bool() {
			continue
		}
		var ends *time.Time
		if e := g.Get("ends_at"); e.Exists() && e.String() != "" {
			ends = parseResetTime(e)
			if ends == nil || !ends.After(now) {
				continue
			}
		}
		total += int(left.Int())
		if id == nextID {
			found = true
			rc.NextExpiresAt = ends
		}
		if left.Int() == 0 {
			continue
		}
		k := grantKind(g)
		if o := byKind[k]; o != nil {
			o.Available += int(left.Int())
		} else {
			byKind[k] = &ResetOption{Kind: k, Available: int(left.Int()), ExpiresAt: ends, grantID: id}
		}
	}
	if !found {
		return rc
	}
	rc.Available = total
	for _, k := range resetKinds {
		if o := byKind[k.kind]; o != nil {
			o.Label, o.Description = k.label, k.desc
			rc.Options = append(rc.Options, *o)
		}
	}
	return rc
}

// Reset outcomes returned to the page.
const (
	ResetDone           = "reset"
	ResetNothingToReset = "nothing_to_reset"
	ResetNoCredit       = "no_credit"
	ResetAlreadyUsed    = "already_redeemed"
)

// resetError is a redemption failure with a message for the page. Settled failures are final
// answers from Claude; unsettled ones (network, unknown result) keep the idempotency key so a
// retry cannot spend a second credit.
type resetError struct {
	msg     string
	settled bool
}

func (e *resetError) Error() string { return e.msg }

// RedeemResetCredit spends one reset credit of the given kind ("session", "weekly", "fable" or
// "full") on a Claude account and, when Claude confirms, clears the proxy's routing cooldown for
// it and refetches its usage.
func (t *Tracker) RedeemResetCredit(ctx context.Context, authID, kind string) (string, string, error) {
	// Lock before reading the credit: a second request queued behind this one must see the
	// balance after the first redemption, not the grant it already spent.
	lock := t.resetLock(authID)
	lock.Lock()
	defer lock.Unlock()

	t.mu.RLock()
	m := t.manager
	e := t.entries[authID]
	t.mu.RUnlock()
	if m == nil {
		return "", "", &resetError{msg: "proxy ainda iniciando", settled: true}
	}
	a, ok := m.GetByID(authID)
	if !ok || a == nil || a.Disabled || !strings.EqualFold(a.Provider, "claude") {
		return "", "", &resetError{msg: "conta Claude não encontrada ou desativada", settled: true}
	}
	grant := ""
	if e != nil && e.ResetCredits != nil {
		for _, o := range e.ResetCredits.Options {
			if o.Kind == kind && o.Available > 0 {
				grant = o.grantID
			}
		}
	}
	if grant == "" {
		return ResetNoCredit, "", nil
	}

	reqID, errID := t.pendingResetID(authID + "/" + grant)
	if errID != nil {
		return "", "", &resetError{msg: "não consegui gerar o id do pedido", settled: true}
	}
	outcome, err := t.claimReset(ctx, a, grant, reqID)
	var re *resetError
	if err == nil || (errors.As(err, &re) && re.settled) {
		t.clearPendingResetID(authID + "/" + grant)
	}
	if err != nil {
		return "", "", err
	}
	warning := ""
	if outcome == ResetDone || outcome == ResetAlreadyUsed {
		if _, _, errCool := m.ResetQuota(ctx, authID); errCool != nil {
			log.Warnf("subsusage: clear cooldown of %s after reset: %v", authID, errCool)
			warning = "Reset feito, mas não consegui liberar a pausa do proxy; ela termina sozinha."
		}
		t.markSpent(authID, grant)
		t.refetchOne(ctx, a)
	}
	log.Infof("subsusage: %s reset credit for %s: %s", kind, authID, outcome)
	return outcome, warning, nil
}

func (t *Tracker) claimReset(ctx context.Context, a *coreauth.Auth, grant, reqID string) (string, error) {
	// Tests must never spend a real credit.
	if testing.Testing() && claudeAPIBase == "https://api.anthropic.com" {
		return "", &resetError{msg: "reset real bloqueado em testes", settled: true}
	}
	token := coreauth.AuthAccessToken(a)
	org := metaString(a, "organization_uuid")
	if token == "" || org == "" {
		return "", &resetError{msg: "faça login de novo nesta conta para usar resets", settled: true}
	}
	if !grantIDPattern.MatchString(grant) || !requestIDPattern.MatchString(reqID) || !orgIDPattern.MatchString(org) {
		return "", &resetError{msg: "Claude devolveu um crédito inválido", settled: true}
	}
	payload, _ := json.Marshal(map[string]string{"program": resetCreditProgram, "grant_id": grant, "request_id": reqID})
	endpoint := claudeAPIBase + "/api/organizations/" + url.PathEscape(org) + "/reset_rate_limits"
	cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", &resetError{msg: "não consegui montar o pedido", settled: true}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("User-Agent", claudeUserAgent)
	resp, err := t.client.Do(req)
	if err != nil {
		return "", &resetError{msg: "Claude não respondeu ao reset; tente de novo"}
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return "", &resetError{msg: "Claude está limitando resets; tente daqui a pouco", settled: true}
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return "", &resetError{msg: "faça login de novo nesta conta para usar resets", settled: true}
	case resp.StatusCode != http.StatusOK:
		return "", &resetError{msg: fmt.Sprintf("Claude recusou o reset (HTTP %d)", resp.StatusCode)}
	}
	switch ojson.GetBytes(body, "result").String() {
	case "reset":
		return ResetDone, nil
	case "already_used":
		return ResetAlreadyUsed, nil
	case "not_limited":
		return ResetNothingToReset, nil
	case "ineligible":
		return ResetNoCredit, nil
	case "cooldown":
		return "", &resetError{msg: "os resets estão em espera no Claude; tente mais tarde", settled: true}
	case "unavailable":
		return "", &resetError{msg: "Claude não confirmou o reset; se continuar limitado, tente de novo"}
	}
	return "", &resetError{msg: "resposta inesperada do Claude ao reset"}
}

// resetLock returns the per-account lock that serializes redemptions.
func (t *Tracker) resetLock(authID string) *sync.Mutex {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.resetLocks == nil {
		t.resetLocks = make(map[string]*sync.Mutex)
	}
	l, ok := t.resetLocks[authID]
	if !ok {
		l = &sync.Mutex{}
		t.resetLocks[authID] = l
	}
	return l
}

// pendingResetID returns the idempotency key of the account's unsettled redemption, or a new one.
func (t *Tracker) pendingResetID(authID string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if id := t.resetPending[authID]; id != "" {
		return id, nil
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b[:])
	if t.resetPending == nil {
		t.resetPending = make(map[string]string)
	}
	t.resetPending[authID] = id
	return id, nil
}

func (t *Tracker) clearPendingResetID(authID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.resetPending, authID)
}

// refetchOne fetches one account's usage right away and stores it, so the page shows the cleared
// windows without waiting for the next round.
func (t *Tracker) refetchOne(ctx context.Context, a *coreauth.Auth) {
	t.refreshMu.Lock()
	defer t.refreshMu.Unlock()
	e, err := t.doFetch(ctx, a)
	if err != nil || e == nil {
		return
	}
	t.mu.Lock()
	now := t.now()
	next := make(map[string]*Entry, len(t.entries))
	for id, v := range t.entries {
		next[id] = v
	}
	next[a.ID] = e
	t.entries = next
	if t.lastFetch == nil {
		t.lastFetch = make(map[string]time.Time)
	}
	t.lastFetch[a.ID] = now
	t.mu.Unlock()
	defaultHistory.recordQuota(e, now)
}

// markSpent takes one use off the cached balance of a grant right after it is redeemed, so a
// request queued behind this one cannot pick the same grant if the refetch fails.
func (t *Tracker) markSpent(authID, grant string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.entries[authID]
	if e == nil || e.ResetCredits == nil {
		return
	}
	rc := *e.ResetCredits
	rc.Options = nil
	for _, o := range e.ResetCredits.Options {
		if o.grantID == grant {
			o.Available--
			rc.Available--
			o.grantID = "" // its next use waits for fresh data from the API
		}
		if o.Available > 0 && o.grantID != "" {
			rc.Options = append(rc.Options, o)
		}
	}
	ne := *e
	ne.ResetCredits = &rc
	next := make(map[string]*Entry, len(t.entries))
	for id, v := range t.entries {
		next[id] = v
	}
	next[authID] = &ne
	t.entries = next
}
