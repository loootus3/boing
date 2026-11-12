package subsusage

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"

	"github.com/gin-gonic/gin"
)

// trayFile is the Windows tray app, kept next to the server binary (built by .github/workflows/usage-tray.yml).
const trayFile = "usage-tray.exe"

// safeHost limits the Host header written into the download to a plain host[:port].
var safeHost = regexp.MustCompile(`^[A-Za-z0-9.\-\[\]:]{1,100}$`)

// serveTray sends the tray app with a "USAGE_TRAY_URL=<this server>" line appended after the PE image, so the
// app points at the address the browser used without any setup. Windows ignores bytes after the image.
func serveTray(c *gin.Context) {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		c.String(http.StatusInternalServerError, "caminho do servidor indisponível")
		return
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(exe), trayFile))
	if err != nil {
		c.String(http.StatusNotFound, "O app da bandeja ainda não foi publicado neste servidor.")
		return
	}
	host := c.Request.Host
	if !safeHost.MatchString(host) {
		host = "127.0.0.1:8317"
	}
	scheme := "http"
	if c.Request.TLS != nil {
		scheme = "https"
	}
	out := make([]byte, 0, len(data)+128)
	out = append(out, data...)
	out = append(out, "\n"+"USAGE_TRAY_URL="+scheme+"://"+host+"\n"...)
	c.Header("Content-Disposition", `attachment; filename="UsageTray.exe"`)
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "application/vnd.microsoft.portable-executable", out)
}
