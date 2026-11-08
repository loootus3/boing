package subsusage

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

//go:embed usage.html
var usagePage []byte

// usageETag identifies the embedded page, so a reload revalidates (304) instead of downloading it again.
var usageETag = func() string { sum := sha256.Sum256(usagePage); return `"` + hex.EncodeToString(sum[:12]) + `"` }()

// Handlers are the existing management handlers reused by the /usage page.
type Handlers struct {
	LoginClaude gin.HandlerFunc
	LoginCodex  gin.HandlerFunc
	LoginStatus gin.HandlerFunc
	// Callback accepts {"redirect_url": "..."} to finish a login whose provider redirect
	// could not reach this server (login started from another computer).
	Callback gin.HandlerFunc
	// PatchFields updates auth-file fields: {"name": "<auth id>", "<field>": value, ...}.
	PatchFields gin.HandlerFunc
}

// modePriority maps usage modes to credential priorities. Higher priorities are spent
// first, so "last" and "off" stay behind normal credentials in every routing strategy.
var modePriority = map[string]int{
	coreauth.UsageModeNormal: 0,
	coreauth.UsageModeLast:   -100,
	coreauth.UsageModeOff:    -1000,
}

// isLocalIP reports loopback, private (RFC 1918 / ULA) and link-local addresses.
func isLocalIP(ip net.IP) bool {
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())
}

// hostAllowed blocks DNS rebinding: the Host header must be a local IP literal, localhost,
// this machine's hostname, or a typical LAN-only name (.local, .lan, .home, .internal).
func hostAllowed(rawHost string) bool {
	host := rawHost
	if h, _, err := net.SplitHostPort(rawHost); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.Trim(strings.ToLower(host), "[]"), ".")
	if host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return isLocalIP(ip)
	}
	if host == "localhost" {
		return true
	}
	if name, err := os.Hostname(); err == nil && strings.EqualFold(host, strings.TrimSpace(name)) {
		return true
	}
	for _, suffix := range []string{".local", ".lan", ".home", ".internal", ".home.arpa"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// lanOnly rejects requests that do not come from this machine or the local network,
// or that target a public host name.
func lanOnly() gin.HandlerFunc {
	return func(c *gin.Context) {
		remote := net.ParseIP(strings.TrimSpace(c.RemoteIP()))
		if !isLocalIP(remote) || !hostAllowed(c.Request.Host) || crossSite(c.Request) {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		c.Next()
	}
}

// crossSite reports requests issued by another site in the user's browser (CSRF).
func crossSite(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default:
		return true
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host != r.Host {
			return true
		}
	}
	return false
}

const uiHeader = "X-Usage-UI"

// requireUIHeader demands a custom header, which cross-origin pages cannot send
// without a CORS preflight that this server never approves.
func requireUIHeader(next gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetHeader(uiHeader) != "1" {
			c.AbortWithStatus(http.StatusForbidden)
			return
		}
		next(c)
	}
}

// forceWebUI makes the reused OAuth handlers forward provider callbacks to this server.
func forceWebUI(next gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		q := c.Request.URL.Query()
		q.Set("is_webui", "true")
		c.Request.URL.RawQuery = q.Encode()
		next(c)
	}
}

func setBody(c *gin.Context, body []byte) {
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	c.Request.ContentLength = int64(len(body))
	c.Request.Header.Set("Content-Type", "application/json")
}

func readBody(c *gin.Context) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 64<<10))
	if err != nil || !ojson.ValidBytes(body) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return nil, false
	}
	return body, true
}

// queryLocation resolves an IANA zone name from the tz query parameter. Missing or invalid
// names fall back to the server's local zone.
func queryLocation(name string) *time.Location {
	name = strings.TrimSpace(name)
	if name == "" || name == "Local" {
		return time.Local
	}
	if loc, err := time.LoadLocation(name); err == nil {
		return loc
	}
	return time.Local
}

// Register mounts the usage dashboard under /usage.
func Register(engine *gin.Engine, h Handlers) {
	if engine == nil {
		return
	}
	t := Default()
	g := engine.Group("/usage", lanOnly())
	page := func(c *gin.Context) {
		// no-cache: the browser keeps the page but checks the ETag on every load, so a deploy shows at once.
		c.Header("Cache-Control", "no-cache")
		c.Header("ETag", usageETag)
		if strings.Contains(c.GetHeader("If-None-Match"), usageETag) {
			c.Status(http.StatusNotModified)
			return
		}
		c.Data(http.StatusOK, "text/html; charset=utf-8", usagePage)
	}
	g.GET("", page)
	g.GET("/", page)
	g.GET("/api/data", func(c *gin.Context) {
		if c.Query("refresh") == "1" && c.GetHeader(uiHeader) == "1" {
			t.Refresh(c.Request.Context(), false)
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, gin.H{"now": time.Now().UTC(), "accounts": t.Snapshot()})
	})
	g.GET("/api/history", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.JSON(http.StatusOK, DefaultHistory().ViewWith(c.Query("range"), c.Query("heat"), time.Now().In(queryLocation(c.Query("tz")))))
	})
	g.GET("/api/quota", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		id := c.Query("id")
		c.JSON(http.StatusOK, gin.H{"id": id, "quota": DefaultHistory().QuotaAll(id)})
	})
	if h.LoginClaude != nil {
		g.POST("/api/login/claude", requireUIHeader(forceWebUI(h.LoginClaude)))
	}
	if h.LoginCodex != nil {
		g.POST("/api/login/codex", requireUIHeader(forceWebUI(h.LoginCodex)))
	}
	if h.LoginStatus != nil {
		g.GET("/api/login/status", h.LoginStatus)
	}
	if h.Callback != nil {
		g.POST("/api/login/callback", requireUIHeader(func(c *gin.Context) {
			body, ok := readBody(c)
			if !ok {
				return
			}
			redirect := strings.TrimSpace(ojson.GetBytes(body, "redirect_url").String())
			if redirect == "" {
				c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "cole a URL completa"})
				return
			}
			doc := ojson.NewDocString("{}")
			if errSet := doc.Set("redirect_url", redirect); errSet != nil {
				c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "invalid body"})
				return
			}
			setBody(c, doc.Bytes())
			h.Callback(c)
		}))
	}
	if h.PatchFields != nil {
		g.POST("/api/mode", requireUIHeader(func(c *gin.Context) {
			body, ok := readBody(c)
			if !ok {
				return
			}
			authID := strings.TrimSpace(ojson.GetBytes(body, "auth_id").String())
			mode := strings.TrimSpace(ojson.GetBytes(body, "mode").String())
			priority, okMode := modePriority[mode]
			if authID == "" || !okMode {
				c.JSON(http.StatusBadRequest, gin.H{"error": "auth_id and a valid mode are required"})
				return
			}
			doc := ojson.NewDocString("{}")
			errName := doc.Set("name", authID)
			errPriority := doc.Set("priority", priority)
			errMode := doc.Set(coreauth.MetadataUsageMode, mode)
			if errName != nil || errPriority != nil || errMode != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build patch"})
				return
			}
			setBody(c, doc.Bytes())
			h.PatchFields(c)
		}))
		// {"auth_id": "...", "name": "..."} sets the account's display name; an empty name clears it.
		g.POST("/api/name", requireUIHeader(func(c *gin.Context) {
			body, ok := readBody(c)
			if !ok {
				return
			}
			authID := strings.TrimSpace(ojson.GetBytes(body, "auth_id").String())
			nameField := ojson.GetBytes(body, "name")
			name, okName := cleanAccountName(nameField.String())
			if authID == "" || !nameField.Exists() || !okName {
				c.JSON(http.StatusBadRequest, gin.H{"error": "auth_id and a name of at most 32 characters are required"})
				return
			}
			patch := map[string]any{"name": authID, MetadataUsageName: nil}
			if name != "" {
				patch[MetadataUsageName] = name
			}
			raw, errMarshal := json.Marshal(patch)
			if errMarshal != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build patch"})
				return
			}
			setBody(c, raw)
			h.PatchFields(c)
		}))
	}
}
