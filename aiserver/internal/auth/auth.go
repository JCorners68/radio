// Package auth provides a passcode gate with 90-day HMAC-signed session cookies for remote
// requests while automatically bypassing authentication for direct localhost desktop requests.
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"html"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	cookieName = "radio_session"
	sessionTTL = 90 * 24 * time.Hour
	maxFails   = 5
	failWindow = time.Minute
)

type Gate struct {
	passcode string
	secret   []byte

	mu       sync.Mutex
	failures map[string][]time.Time
}

func NewGate(passcode string) *Gate {
	sum := sha256.Sum256([]byte("radio-station-session-v1:" + passcode))
	return &Gate{
		passcode: passcode,
		secret:   sum[:],
		failures: make(map[string][]time.Time),
	}
}

// Enabled reports whether remote passcode authentication is active.
func (g *Gate) Enabled() bool {
	return g != nil && g.passcode != ""
}

// IsLocalDirect reports whether r came directly from loopback without a reverse proxy/tunnel.
func IsLocalDirect(r *http.Request) bool {
	if r.Header.Get("Cf-Connecting-Ip") != "" || r.Header.Get("X-Forwarded-For") != "" {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func clientIP(r *http.Request) string {
	if ip := strings.TrimSpace(r.Header.Get("Cf-Connecting-Ip")); ip != "" {
		return ip
	}
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil ||
		strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") ||
		r.Header.Get("Cf-Connecting-Ip") != ""
}

func (g *Gate) signToken(exp int64) string {
	payload := strconv.FormatInt(exp, 10)
	mac := hmac.New(sha256.New, g.secret)
	mac.Write([]byte(payload))
	return payload + "." + hex.EncodeToString(mac.Sum(nil))
}

func (g *Gate) verifyToken(tok string) bool {
	parts := strings.SplitN(tok, ".", 2)
	if len(parts) != 2 {
		return false
	}
	exp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	expected := g.signToken(exp)
	return subtle.ConstantTimeCompare([]byte(tok), []byte(expected)) == 1
}

func (g *Gate) isAuthenticated(r *http.Request) bool {
	if !g.Enabled() || IsLocalDirect(r) {
		return true
	}
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return false
	}
	return g.verifyToken(c.Value)
}

func (g *Gate) allowAttempt(ip string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-failWindow)
	list := g.failures[ip]
	kept := list[:0]
	for _, t := range list {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	g.failures[ip] = kept
	return len(kept) < maxFails
}

func (g *Gate) recordFailure(ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failures[ip] = append(g.failures[ip], time.Now())
}

// Wrap protects an http.Handler with the passcode gate for non-local requests.
func (g *Gate) Wrap(next http.Handler) http.Handler {
	if !g.Enabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz", "/manifest.json", "/icon.svg":
			next.ServeHTTP(w, r)
			return
		case "/auth/login":
			if r.Method == http.MethodPost {
				g.handleLoginPost(w, r)
			} else {
				g.handleLoginPage(w, r, "")
			}
			return
		case "/auth/logout":
			http.SetCookie(w, &http.Cookie{
				Name:     cookieName,
				Value:    "",
				Path:     "/",
				MaxAge:   -1,
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
				Secure:   isHTTPS(r),
			})
			http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
			return
		}

		if g.isAuthenticated(r) {
			next.ServeHTTP(w, r)
			return
		}

		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"unauthorized"}`)
			return
		}
		g.handleLoginPage(w, r, "")
	})
}

func (g *Gate) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !g.allowAttempt(ip) {
		w.WriteHeader(http.StatusTooManyRequests)
		g.handleLoginPage(w, r, "Too many attempts. Wait 60 seconds and try again.")
		return
	}
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		g.handleLoginPage(w, r, "Bad form submission.")
		return
	}
	code := strings.TrimSpace(r.FormValue("passcode"))
	if subtle.ConstantTimeCompare([]byte(code), []byte(g.passcode)) != 1 {
		g.recordFailure(ip)
		w.WriteHeader(http.StatusUnauthorized)
		g.handleLoginPage(w, r, "Invalid passcode.")
		return
	}

	exp := time.Now().Add(sessionTTL).Unix()
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    g.signToken(exp),
		Path:     "/",
		Expires:  time.Unix(exp, 0),
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (g *Gate) handleLoginPage(w http.ResponseWriter, r *http.Request, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	errHTML := ""
	if errMsg != "" {
		errHTML = `<div class="err">` + html.EscapeString(errMsg) + `</div>`
	}
	fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0, viewport-fit=cover">
<meta name="apple-mobile-web-app-capable" content="yes">
<meta name="apple-mobile-web-app-status-bar-style" content="black-translucent">
<meta name="apple-mobile-web-app-title" content="My Station">
<link rel="manifest" href="/manifest.json">
<link rel="apple-touch-icon" href="/icon.svg">
<title>My Station · Sign In</title>
<style>
  :root {
    --bg: #0b0b10;
    --panel: #14141d;
    --border: #252637;
    --text: #f3f1ec;
    --muted: #8a8899;
    --accent: #ff6a3d;
    --danger: #ff5470;
  }
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body {
    min-height: 100vh;
    display: flex;
    align-items: center;
    justify-content: center;
    padding: 24px;
    background: radial-gradient(circle at 30%% 20%%, rgba(255,106,61,.14), transparent 55%%), var(--bg);
    color: var(--text);
    font-family: -apple-system, BlinkMacSystemFont, 'Space Grotesk', system-ui, sans-serif;
  }
  .card {
    width: 100%%;
    max-width: 380px;
    background: var(--panel);
    border: 1px solid var(--border);
    border-radius: 18px;
    padding: 32px 26px;
    box-shadow: 0 24px 60px rgba(0,0,0,.55);
    text-align: center;
  }
  .badge {
    display: inline-flex;
    align-items: center;
    gap: 8px;
    font-size: 11px;
    letter-spacing: .16em;
    text-transform: uppercase;
    color: var(--accent);
    margin-bottom: 12px;
    font-weight: 600;
  }
  .dot {
    width: 8px;
    height: 8px;
    border-radius: 50%%;
    background: var(--accent);
    box-shadow: 0 0 10px var(--accent);
  }
  h1 {
    font-size: 24px;
    font-weight: 600;
    margin-bottom: 8px;
  }
  p {
    font-size: 14px;
    color: var(--muted);
    margin-bottom: 24px;
  }
  input {
    width: 100%%;
    padding: 14px 16px;
    font-size: 17px;
    border-radius: 12px;
    border: 1px solid var(--border);
    background: #0e0e15;
    color: var(--text);
    text-align: center;
    letter-spacing: .08em;
    outline: none;
    margin-bottom: 14px;
  }
  input:focus {
    border-color: var(--accent);
  }
  button {
    width: 100%%;
    padding: 14px 18px;
    font-size: 15px;
    font-weight: 600;
    border-radius: 12px;
    border: none;
    background: var(--accent);
    color: #0b0b10;
    cursor: pointer;
  }
  .err {
    margin-bottom: 16px;
    padding: 10px 12px;
    border-radius: 10px;
    background: rgba(255,84,112,.14);
    border: 1px solid rgba(255,84,112,.35);
    color: var(--danger);
    font-size: 13px;
  }
</style>
</head>
<body>
  <form class="card" method="POST" action="/auth/login">
    <div class="badge"><span class="dot"></span>On Air · Private Station</div>
    <h1>My Radio Station</h1>
    <p>Enter your station passcode to unlock for 90 days on this device.</p>
    %s
    <input type="password" name="passcode" placeholder="Passcode" autocomplete="current-password" autofocus required>
    <button type="submit">Unlock Station</button>
  </form>
</body>
</html>`, errHTML)
}
