package subsusage

import (
	_ "embed"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

//go:embed usage.html
var usagePage []byte

// LoginHandlers are the existing OAuth handlers reused by the /usage page.
type LoginHandlers struct {
	Claude gin.HandlerFunc
	Codex  gin.HandlerFunc
	Status gin.HandlerFunc
}

// localOnly rejects any request that does not come from the loopback interface or
// does not target a loopback host name (DNS rebinding protection).
func localOnly() gin.HandlerFunc {
	return func(c *gin.Context) {
		remote := net.ParseIP(strings.TrimSpace(c.RemoteIP()))
		host := c.Request.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.Trim(strings.ToLower(host), "[]")
		hostOK := host == "localhost" || host == "127.0.0.1" || host == "::1"
		if remote == nil || !remote.IsLoopback() || !hostOK || crossSite(c.Request) {
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

const uiHeader = "X-Usage-UI"

// forceWebUI makes the reused OAuth handlers forward provider callbacks to this server.
func forceWebUI(next gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		q := c.Request.URL.Query()
		q.Set("is_webui", "true")
		c.Request.URL.RawQuery = q.Encode()
		next(c)
	}
}

// Register mounts the local usage dashboard under /usage.
func Register(engine *gin.Engine, login LoginHandlers) {
	if engine == nil {
		return
	}
	t := Default()
	g := engine.Group("/usage", localOnly())
	page := func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
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
	if login.Claude != nil {
		g.POST("/api/login/claude", requireUIHeader(forceWebUI(login.Claude)))
	}
	if login.Codex != nil {
		g.POST("/api/login/codex", requireUIHeader(forceWebUI(login.Codex)))
	}
	if login.Status != nil {
		g.GET("/api/login/status", login.Status)
	}
}
