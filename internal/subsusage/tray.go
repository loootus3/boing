package subsusage

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// trayFile is the Windows tray app, kept next to the server binary (built by .github/workflows/usage-tray.yml).
const trayFile = "usage-tray.exe"

// safeHost limits the Host header written into the download to a plain host[:port].
var (
	safeHost = regexp.MustCompile(`^[A-Za-z0-9.\-\[\]:]{1,100}$`)
	safeBase = regexp.MustCompile(`^https?://[A-Za-z0-9.\-\[\]:]{1,100}$`)
)

// trayPath is the published tray app next to the server binary.
func trayPath() (string, error) {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(exe), trayFile), nil
}

// The app carries its build id as "TRAYBUILD:<id>:END" (see tools/usage-tray); the server reads it from the
// published file and reports it in /usage/api/data, so the app can tell when another build is published.
var (
	trayBuildRe  = regexp.MustCompile(`TRAYBUILD:([A-Za-z0-9._-]{1,64}):END`)
	trayBuildMu  sync.Mutex
	trayBuildKey string
	trayBuildVal string
)

// trayBuild is the build id of the published tray app ("" when absent or unmarked), cached by size and mtime.
func trayBuild() string {
	p, err := trayPath()
	if err != nil {
		return ""
	}
	st, err := os.Stat(p)
	if err != nil {
		return ""
	}
	key := st.ModTime().Format(time.RFC3339Nano) + "|" + strconv.FormatInt(st.Size(), 10)
	trayBuildMu.Lock()
	defer trayBuildMu.Unlock()
	if key == trayBuildKey {
		return trayBuildVal
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	val := parseTrayBuild(data)
	trayBuildKey, trayBuildVal = key, val
	return val
}

// parseTrayBuild finds the "TRAYBUILD:<id>:END" mark in the app's bytes.
func parseTrayBuild(data []byte) string {
	for off := 0; ; {
		i := bytes.Index(data[off:], []byte("TRAYBUILD:"))
		if i < 0 {
			return ""
		}
		off += i
		if m := trayBuildRe.FindSubmatch(data[off:min(len(data), off+90)]); m != nil {
			return string(m[1])
		}
		off += len("TRAYBUILD:")
	}
}

// serveTray sends the tray app with a "USAGE_TRAY_URL=<this server>" line appended after the PE image, so the
// app points at the address the browser used without any setup. Windows ignores bytes after the image.
func serveTray(c *gin.Context) {
	p, err := trayPath()
	if err != nil {
		c.String(http.StatusInternalServerError, "caminho do servidor indisponível")
		return
	}
	data, err := os.ReadFile(p)
	if err != nil {
		c.String(http.StatusNotFound, "O app da bandeja ainda não foi publicado neste servidor.")
		return
	}
	// The page sends its own origin (?base=), the address the browser reached this server at; the request's
	// TLS state is not reliable here (the listener multiplexes protocols), so http is assumed otherwise.
	base := c.Query("base")
	if !safeBase.MatchString(base) {
		host := c.Request.Host
		if !safeHost.MatchString(host) {
			host = "127.0.0.1:8317"
		}
		base = "http://" + host
	}
	out := make([]byte, 0, len(data)+128)
	out = append(out, data...)
	out = append(out, "\nUSAGE_TRAY_URL="+base+"\n"...)
	c.Header("Content-Disposition", `attachment; filename="UsageTray.exe"`)
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "application/vnd.microsoft.portable-executable", out)
}
