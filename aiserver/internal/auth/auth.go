// Package auth provides a hybrid remote authentication gate combining:
//  1. Localhost direct loopback bypass (maps to primary owner)
//  2. Cloudflare Zero Trust Access edge JWT verification (Cf-Access-Jwt-Assertion)
//  3. Google OAuth 2.0 / OIDC (Authorization Code flow + Google Identity Services ID token verification)
//  4. Strict email allowlist (owner vs family/guest accounts)
//  5. 90-day HMAC-SHA256 session cookies with instant per-email session revocation
package auth

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	cookieName  = "radio_session"
	oauthStateC = "radio_oauth_state"
	sessionTTL  = 90 * 24 * time.Hour
	maxFails    = 5
	failWindow  = time.Minute
	defaultDailyAISuggestLimit = 25
)

type User struct {
	Email   string
	IsOwner bool
}

type ctxKey struct{}

func WithUser(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

func UserFromContext(ctx context.Context) User {
	if u, ok := ctx.Value(ctxKey{}).(User); ok {
		return u
	}
	return User{Email: "local@localhost", IsOwner: true}
}

type Config struct {
	AllowedEmails      []string
	OwnerEmails        []string
	GoogleClientID     string
	GoogleClientSecret string
	CFAccessTeamDomain string
	CFAccessAUD        string
	Passcode           string
	SecretSeed         string
	DataDir            string
}

type Gate struct {
	allowed       map[string]bool
	owners        map[string]bool
	primaryOwner  string
	googleID      string
	googleSecret  string
	cfTeamDomain  string
	cfAUD         string
	passcode      string
	secret        []byte
	revokedPath   string
	httpCli       *http.Client

	mu          sync.Mutex
	failures    map[string][]time.Time
	revokedAt   map[string]int64
	aiUsageDay  string
	aiUsage     map[string]int
	jwksCache   map[string]*rsa.PublicKey
	jwksExpires time.Time
}

// NewGate constructs a passcode-backed Gate (kept for backwards compatibility and unit tests).
func NewGate(passcode string) *Gate {
	return NewOAuthGate(Config{
		Passcode:   passcode,
		SecretSeed: passcode,
	})
}

// NewOAuthGate constructs the hybrid Cloudflare Access + Google OAuth2/OIDC Gate.
func NewOAuthGate(cfg Config) *Gate {
	allowed := make(map[string]bool)
	owners := make(map[string]bool)
	primaryOwner := "jonathan.corners@gmail.com"

	for i, e := range cfg.OwnerEmails {
		clean := strings.ToLower(strings.TrimSpace(e))
		if clean != "" {
			owners[clean] = true
			allowed[clean] = true
			if i == 0 {
				primaryOwner = clean
			}
		}
	}
	for _, e := range cfg.AllowedEmails {
		clean := strings.ToLower(strings.TrimSpace(e))
		if clean != "" {
			allowed[clean] = true
		}
	}

	seed := cfg.SecretSeed
	if seed == "" {
		seed = cfg.GoogleClientSecret + "|" + cfg.Passcode + "|" + primaryOwner
	}
	sum := sha256.Sum256([]byte("radio-station-session-v2:" + seed))

	var revokedPath string
	if cfg.DataDir != "" {
		revokedPath = filepath.Join(cfg.DataDir, "revoked_sessions.json")
	}

	g := &Gate{
		allowed:      allowed,
		owners:       owners,
		primaryOwner: primaryOwner,
		googleID:     strings.TrimSpace(cfg.GoogleClientID),
		googleSecret: strings.TrimSpace(cfg.GoogleClientSecret),
		cfTeamDomain: strings.TrimSpace(cfg.CFAccessTeamDomain),
		cfAUD:        strings.TrimSpace(cfg.CFAccessAUD),
		passcode:     strings.TrimSpace(cfg.Passcode),
		secret:       sum[:],
		revokedPath:  revokedPath,
		httpCli:      &http.Client{Timeout: 10 * time.Second},
		failures:     make(map[string][]time.Time),
		revokedAt:    make(map[string]int64),
		aiUsage:      make(map[string]int),
		jwksCache:    make(map[string]*rsa.PublicKey),
	}
	g.loadRevoked()
	return g
}

// Enabled reports whether remote authentication is active.
func (g *Gate) Enabled() bool {
	if g == nil {
		return false
	}
	return len(g.allowed) > 0 || g.googleID != "" || g.cfTeamDomain != "" || g.passcode != ""
}

// GoogleEnabled reports whether Google OAuth / OIDC is configured.
func (g *Gate) GoogleEnabled() bool {
	return g != nil && g.googleID != ""
}

// AllowAISuggest enforces a daily per-user quota on non-owner accounts.
func (g *Gate) AllowAISuggest(u User) bool {
	if u.IsOwner {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	today := time.Now().UTC().Format("2006-01-02")
	if g.aiUsageDay != today {
		g.aiUsageDay = today
		g.aiUsage = make(map[string]int)
	}
	email := strings.ToLower(u.Email)
	if g.aiUsage[email] >= defaultDailyAISuggestLimit {
		return false
	}
	g.aiUsage[email]++
	return true
}

func (g *Gate) isAllowedEmail(email string) (User, bool) {
	clean := strings.ToLower(strings.TrimSpace(email))
	if clean == "" {
		return User{}, false
	}
	if len(g.allowed) == 0 {
		return User{Email: clean, IsOwner: true}, true
	}
	if !g.allowed[clean] {
		return User{}, false
	}
	return User{Email: clean, IsOwner: g.owners[clean]}, true
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

func externalBaseURL(r *http.Request) string {
	scheme := "http"
	if isHTTPS(r) {
		scheme = "https"
	}
	host := r.Host
	if fwdHost := r.Header.Get("X-Forwarded-Host"); fwdHost != "" {
		host = strings.TrimSpace(strings.Split(fwdHost, ",")[0])
	}
	return scheme + "://" + host
}

func (g *Gate) signUserToken(email string, iat, exp int64) string {
	rawPayload := fmt.Sprintf("%s|%d|%d", strings.ToLower(strings.TrimSpace(email)), iat, exp)
	b64 := base64.RawURLEncoding.EncodeToString([]byte(rawPayload))
	mac := hmac.New(sha256.New, g.secret)
	mac.Write([]byte(b64))
	return b64 + "." + hex.EncodeToString(mac.Sum(nil))
}

func (g *Gate) verifyUserToken(tok string) (User, bool) {
	parts := strings.SplitN(tok, ".", 2)
	if len(parts) != 2 {
		return User{}, false
	}
	mac := hmac.New(sha256.New, g.secret)
	mac.Write([]byte(parts[0]))
	expectedSig := hex.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(parts[1]), []byte(expectedSig)) != 1 {
		return User{}, false
	}

	// Support legacy numeric-only exp payload used in passcode unit tests
	if exp, err := strconv.ParseInt(parts[0], 10, 64); err == nil {
		if time.Now().Unix() > exp {
			return User{}, false
		}
		return User{Email: g.primaryOwner, IsOwner: true}, true
	}

	decoded, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return User{}, false
	}
	fields := strings.SplitN(string(decoded), "|", 3)
	if len(fields) != 3 {
		return User{}, false
	}
	email := fields[0]
	iat, err1 := strconv.ParseInt(fields[1], 10, 64)
	exp, err2 := strconv.ParseInt(fields[2], 10, 64)
	if err1 != nil || err2 != nil || time.Now().Unix() > exp {
		return User{}, false
	}

	u, ok := g.isAllowedEmail(email)
	if !ok {
		return User{}, false
	}

	g.mu.Lock()
	revAfter := g.revokedAt[u.Email]
	g.mu.Unlock()
	if revAfter > 0 && iat <= revAfter {
		return User{}, false
	}
	return u, true
}

func (g *Gate) issueSessionCookie(w http.ResponseWriter, r *http.Request, email string) {
	now := time.Now()
	exp := now.Add(sessionTTL)
	tok := g.signUserToken(email, now.Unix(), exp.Unix())
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    tok,
		Path:     "/",
		Expires:  exp,
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
	})
}

func (g *Gate) authenticateRequest(w http.ResponseWriter, r *http.Request) (User, bool) {
	if !g.Enabled() || IsLocalDirect(r) {
		return User{Email: g.primaryOwner, IsOwner: true}, true
	}

	// 1. Check first-party signed session cookie
	if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
		if u, ok := g.verifyUserToken(c.Value); ok {
			return u, true
		}
	}

	// 2. Check Cloudflare Zero Trust Access JWT header if configured
	if cfJWT := strings.TrimSpace(r.Header.Get("Cf-Access-Jwt-Assertion")); cfJWT != "" && g.cfTeamDomain != "" {
		if email, err := g.verifyCFAccessJWT(r.Context(), cfJWT); err == nil {
			if u, ok := g.isAllowedEmail(email); ok {
				g.issueSessionCookie(w, r, u.Email)
				return u, true
			}
		}
	}

	return User{}, false
}

// Wrap protects an http.Handler with the hybrid auth gate for non-local requests.
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
				g.handlePasscodePost(w, r)
			} else {
				g.handleLoginPage(w, r, "", http.StatusOK)
			}
			return
		case "/auth/google/login":
			g.handleGoogleLoginStart(w, r)
			return
		case "/auth/google/callback":
			if r.Method == http.MethodPost {
				g.handleGoogleIDTokenPost(w, r)
			} else {
				g.handleGoogleCallback(w, r)
			}
			return
		case "/auth/google/token":
			g.handleGoogleIDTokenPost(w, r)
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
		case "/auth/revoke":
			g.handleRevoke(w, r)
			return
		}

		if u, ok := g.authenticateRequest(w, r); ok {
			next.ServeHTTP(w, r.WithContext(WithUser(r.Context(), u)))
			return
		}

		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/ui/") || r.Header.Get("HX-Request") == "true" {
			w.Header().Set("HX-Redirect", "/auth/login")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"unauthorized"}`)
			return
		}
		g.handleLoginPage(w, r, "", http.StatusOK)
	})
}

func (g *Gate) handleGoogleLoginStart(w http.ResponseWriter, r *http.Request) {
	if g.googleID == "" {
		g.handleLoginPage(w, r, "Google OAuth client ID is not configured.", http.StatusServiceUnavailable)
		return
	}
	var stateBytes [16]byte
	_, _ = rand.Read(stateBytes[:])
	stateVal := hex.EncodeToString(stateBytes[:])

	http.SetCookie(w, &http.Cookie{
		Name:     oauthStateC,
		Value:    stateVal,
		Path:     "/",
		MaxAge:   600,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
	})

	redirectURI := externalBaseURL(r) + "/auth/google/callback"
	q := url.Values{
		"client_id":     {g.googleID},
		"redirect_uri":  {redirectURI},
		"response_type": {"code"},
		"scope":         {"openid email profile"},
		"state":         {stateVal},
		"prompt":        {"select_account"},
	}
	http.Redirect(w, r, "https://accounts.google.com/o/oauth2/v2/auth?"+q.Encode(), http.StatusFound)
}

func (g *Gate) handleGoogleCallback(w http.ResponseWriter, r *http.Request) {
	stateCookie, err := r.Cookie(oauthStateC)
	if err != nil || stateCookie.Value == "" || stateCookie.Value != r.URL.Query().Get("state") {
		g.handleLoginPage(w, r, "Invalid OAuth state. Please try signing in again.", http.StatusBadRequest)
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" {
		g.handleLoginPage(w, r, "Google sign-in was canceled.", http.StatusBadRequest)
		return
	}

	redirectURI := externalBaseURL(r) + "/auth/google/callback"
	form := url.Values{
		"code":          {code},
		"client_id":     {g.googleID},
		"client_secret": {g.googleSecret},
		"redirect_uri":  {redirectURI},
		"grant_type":    {"authorization_code"},
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "https://oauth2.googleapis.com/token", strings.NewReader(form.Encode()))
	if err != nil {
		g.handleLoginPage(w, r, "Failed to build token request.", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := g.httpCli.Do(req)
	if err != nil {
		g.handleLoginPage(w, r, "Could not reach Google OAuth token endpoint.", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		g.handleLoginPage(w, r, "Google token exchange failed.", http.StatusBadGateway)
		return
	}

	var tokResp struct {
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &tokResp); err != nil || tokResp.IDToken == "" {
		g.handleLoginPage(w, r, "Invalid Google token response.", http.StatusBadGateway)
		return
	}

	email, err := g.extractVerifiedGoogleEmail(r.Context(), tokResp.IDToken, false)
	if err != nil {
		g.handleLoginPage(w, r, "Google identity verification failed: "+err.Error(), http.StatusForbidden)
		return
	}
	u, ok := g.isAllowedEmail(email)
	if !ok {
		g.handleLoginPage(w, r, fmt.Sprintf("Account %s is not authorized for this station.", email), http.StatusForbidden)
		return
	}

	g.issueSessionCookie(w, r, u.Email)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (g *Gate) handleGoogleIDTokenPost(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		g.handleLoginPage(w, r, "Bad request.", http.StatusBadRequest)
		return
	}
	cred := strings.TrimSpace(r.FormValue("credential"))
	if cred == "" {
		g.handleLoginPage(w, r, "Missing Google credential token.", http.StatusBadRequest)
		return
	}
	email, err := g.extractVerifiedGoogleEmail(r.Context(), cred, true)
	if err != nil {
		g.handleLoginPage(w, r, "Google verification failed: "+err.Error(), http.StatusForbidden)
		return
	}
	u, ok := g.isAllowedEmail(email)
	if !ok {
		g.handleLoginPage(w, r, fmt.Sprintf("Account %s is not authorized for this station.", email), http.StatusForbidden)
		return
	}
	g.issueSessionCookie(w, r, u.Email)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// extractVerifiedGoogleEmail extracts and verifies email from a Google ID token.
// When verifyWithGoogle is true (for browser-posted GIS tokens), it validates the signature
// and audience via Google's tokeninfo endpoint; when false (direct TLS response from
// oauth2.googleapis.com/token), it decodes the JWT claims directly and checks aud/exp/email_verified.
func (g *Gate) extractVerifiedGoogleEmail(ctx context.Context, idToken string, verifyWithGoogle bool) (string, error) {
	if verifyWithGoogle {
		u := "https://oauth2.googleapis.com/tokeninfo?id_token=" + url.QueryEscape(idToken)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return "", err
		}
		resp, err := g.httpCli.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", errors.New("invalid ID token")
		}
		var info struct {
			Aud           string `json:"aud"`
			Email         string `json:"email"`
			EmailVerified string `json:"email_verified"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<10)).Decode(&info); err != nil {
			return "", err
		}
		if g.googleID != "" && info.Aud != g.googleID {
			return "", errors.New("client_id audience mismatch")
		}
		if info.EmailVerified != "true" {
			return "", errors.New("email is not verified")
		}
		return strings.ToLower(strings.TrimSpace(info.Email)), nil
	}

	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return "", errors.New("malformed jwt")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", err
	}
	var claims struct {
		Aud           string `json:"aud"`
		Email         string `json:"email"`
		EmailVerified any    `json:"email_verified"`
		Exp           int64  `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", err
	}
	if claims.Exp > 0 && time.Now().Unix() > claims.Exp {
		return "", errors.New("expired id_token")
	}
	if g.googleID != "" && claims.Aud != g.googleID {
		return "", errors.New("audience mismatch")
	}
	verified := claims.EmailVerified == true || claims.EmailVerified == "true"
	if !verified {
		return "", errors.New("email not verified")
	}
	return strings.ToLower(strings.TrimSpace(claims.Email)), nil
}

// verifyCFAccessJWT verifies a Cloudflare Access edge JWT against the team's JWKS endpoint.
func (g *Gate) verifyCFAccessJWT(ctx context.Context, token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("malformed cf access jwt")
	}
	hdrBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", err
	}
	var hdr struct {
		Kid string `json:"kid"`
		Alg string `json:"alg"`
	}
	if err := json.Unmarshal(hdrBytes, &hdr); err != nil || hdr.Alg != "RS256" || hdr.Kid == "" {
		return "", errors.New("unsupported jwt header")
	}

	pubKey, err := g.getCFAccessKey(ctx, hdr.Kid)
	if err != nil {
		return "", err
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", err
	}
	signedInput := parts[0] + "." + parts[1]
	digest := sha256.Sum256([]byte(signedInput))
	if err := rsa.VerifyPKCS1v15(pubKey, crypto.SHA256, digest[:], sig); err != nil {
		return "", err
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", err
	}
	var claims struct {
		Email string `json:"email"`
		Exp   int64  `json:"exp"`
		Aud   any    `json:"aud"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", err
	}
	if claims.Exp > 0 && time.Now().Unix() > claims.Exp {
		return "", errors.New("cf jwt expired")
	}
	if g.cfAUD != "" && !audMatches(claims.Aud, g.cfAUD) {
		return "", errors.New("cf jwt aud mismatch")
	}
	return strings.ToLower(strings.TrimSpace(claims.Email)), nil
}

func audMatches(aud any, want string) bool {
	switch v := aud.(type) {
	case string:
		return v == want
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}

func (g *Gate) getCFAccessKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	g.mu.Lock()
	if time.Now().Before(g.jwksExpires) {
		if k, ok := g.jwksCache[kid]; ok {
			g.mu.Unlock()
			return k, nil
		}
	}
	g.mu.Unlock()

	domain := strings.TrimPrefix(strings.TrimPrefix(g.cfTeamDomain, "https://"), "http://")
	certsURL := "https://" + strings.TrimSuffix(domain, "/") + "/cdn-cgi/access/certs"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, certsURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := g.httpCli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var jwks struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 128<<10)).Decode(&jwks); err != nil {
		return nil, err
	}

	parsed := make(map[string]*rsa.PublicKey)
	for _, k := range jwks.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		nb, err1 := base64.RawURLEncoding.DecodeString(k.N)
		eb, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil {
			continue
		}
		eInt := 0
		for _, b := range eb {
			eInt = (eInt << 8) | int(b)
		}
		parsed[k.Kid] = &rsa.PublicKey{
			N: new(big.Int).SetBytes(nb),
			E: eInt,
		}
	}

	g.mu.Lock()
	g.jwksCache = parsed
	g.jwksExpires = time.Now().Add(time.Hour)
	key, ok := g.jwksCache[kid]
	g.mu.Unlock()
	if !ok {
		return nil, errors.New("kid not found in cf jwks")
	}
	return key, nil
}

// RevokeUser invalidates all existing session cookies for email issued at or before now.
func (g *Gate) RevokeUser(email string) {
	clean := strings.ToLower(strings.TrimSpace(email))
	if clean == "" {
		return
	}
	g.mu.Lock()
	g.revokedAt[clean] = time.Now().Unix()
	g.saveRevokedLocked()
	g.mu.Unlock()
}

func (g *Gate) loadRevoked() {
	if g.revokedPath == "" {
		return
	}
	b, err := os.ReadFile(g.revokedPath)
	if err != nil {
		return
	}
	_ = json.Unmarshal(b, &g.revokedAt)
}

func (g *Gate) saveRevokedLocked() {
	if g.revokedPath == "" {
		return
	}
	if b, err := json.MarshalIndent(g.revokedAt, "", "  "); err == nil {
		_ = os.WriteFile(g.revokedPath, b, 0o600)
	}
}

func (g *Gate) handleRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	u, ok := g.authenticateRequest(w, r)
	if !ok || !u.IsOwner {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	target := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("email")))
	if target == "" {
		_ = r.ParseForm()
		target = strings.ToLower(strings.TrimSpace(r.FormValue("email")))
	}
	if target == "" {
		http.Error(w, "missing email", http.StatusBadRequest)
		return
	}
	g.RevokeUser(target)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"revoked":%q}`, target)
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

func (g *Gate) handlePasscodePost(w http.ResponseWriter, r *http.Request) {
	if g.passcode == "" {
		g.handleLoginPage(w, r, "Passcode login is disabled. Sign in with Google.", http.StatusForbidden)
		return
	}
	ip := clientIP(r)
	if !g.allowAttempt(ip) {
		g.handleLoginPage(w, r, "Too many attempts. Wait 60 seconds and try again.", http.StatusTooManyRequests)
		return
	}
	if err := r.ParseForm(); err != nil {
		g.handleLoginPage(w, r, "Bad form submission.", http.StatusBadRequest)
		return
	}
	code := strings.TrimSpace(r.FormValue("passcode"))
	if subtle.ConstantTimeCompare([]byte(code), []byte(g.passcode)) != 1 {
		g.recordFailure(ip)
		g.handleLoginPage(w, r, "Invalid passcode.", http.StatusUnauthorized)
		return
	}

	g.issueSessionCookie(w, r, g.primaryOwner)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (g *Gate) handleLoginPage(w http.ResponseWriter, r *http.Request, errMsg string, status int) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if status != 0 && status != http.StatusOK {
		w.WriteHeader(status)
	}
	errHTML := ""
	if errMsg != "" {
		errHTML = `<div class="err">` + html.EscapeString(errMsg) + `</div>`
	}

	googleSection := ""
	if g.googleID != "" {
		if g.googleSecret != "" {
			googleSection = `
    <a class="oauth-btn" href="/auth/google/login">
      <svg viewBox="0 0 24 24" width="18" height="18"><path fill="#EA4335" d="M12 5c1.6 0 3 .6 4.1 1.7l3.1-3.1C17.3 1.8 14.8 1 12 1 7.4 1 3.5 3.6 1.6 7.4l3.7 2.8C6.2 7.2 8.9 5 12 5z"/><path fill="#4285F4" d="M23.5 12.3c0-.8-.1-1.6-.2-2.3H12v4.5h6.5c-.3 1.5-1.1 2.8-2.4 3.6l3.7 2.9c2.2-2 3.7-5 3.7-8.7z"/><path fill="#FBBC05" d="M5.3 14.8c-.2-.8-.4-1.6-.4-2.5s.2-1.7.4-2.5L1.6 7C.6 9 0 11.2 0 13.5s.6 4.5 1.6 6.5l3.7-2.9z"/><path fill="#34A853" d="M12 24c3.2 0 6-1.1 8-3l-3.7-2.9c-1.1.7-2.5 1.2-4.3 1.2-3.1 0-5.8-2.1-6.7-5l-3.7 2.8C3.5 21.4 7.4 24 12 24z"/></svg>
      <span>Sign in with Google</span>
    </a>`
		} else {
			loginURI := externalBaseURL(r) + "/auth/google/callback"
			googleSection = fmt.Sprintf(`
    <script src="https://accounts.google.com/gsi/client" async defer></script>
    <div id="g_id_onload"
         data-client_id=%q
         data-context="signin"
         data-ux_mode="redirect"
         data-login_uri=%q
         data-auto_prompt="false">
    </div>
    <div class="g-wrap">
      <div class="g_id_signin"
           data-type="standard"
           data-shape="pill"
           data-theme="filled_black"
           data-text="continue_with"
           data-size="large"
           data-logo_alignment="left"
           data-width="310">
      </div>
    </div>`, g.googleID, loginURI)
		}
	}

	passcodeSection := ""
	if g.passcode != "" && g.googleID == "" {
		passcodeSection = `
    <form method="POST" action="/auth/login">
      <input type="password" name="passcode" placeholder="Station passcode" autocomplete="current-password" autofocus required>
      <button type="submit">Unlock Station</button>
    </form>`
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
    --bg: #07080d;
    --panel: #12141f;
    --border: rgba(255,255,255,0.12);
    --text: #eef0f7;
    --muted: #9198ad;
    --accent: #5eead4;
    --danger: #fb7185;
  }
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body {
    min-height: 100vh;
    display: flex;
    align-items: center;
    justify-content: center;
    padding: 24px;
    background:
      radial-gradient(900px 500px at 20%% 0%%, rgba(167,139,250,.16), transparent 60%%),
      radial-gradient(800px 500px at 90%% 10%%, rgba(94,234,212,.12), transparent 55%%),
      var(--bg);
    color: var(--text);
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
  }
  .card {
    width: 100%%;
    max-width: 380px;
    background: var(--panel);
    border: 1px solid var(--border);
    border-radius: 20px;
    padding: 32px 26px;
    box-shadow: 0 28px 64px rgba(0,0,0,.6);
    text-align: center;
  }
  .dot {
    width: 12px; height: 12px; border-radius: 50%%;
    background: var(--accent);
    box-shadow: 0 0 14px var(--accent);
    margin: 0 auto 14px auto;
  }
  h1 {
    font-size: 21px;
    font-weight: 700;
    letter-spacing: 0.5px;
    margin-bottom: 6px;
  }
  h1 span { color: var(--accent); }
  p.sub {
    color: var(--muted);
    font-size: 13.5px;
    margin-bottom: 24px;
    line-height: 1.45;
  }
  .err {
    background: rgba(251,113,133,.12);
    border: 1px solid rgba(251,113,133,.35);
    color: var(--danger);
    padding: 10px 12px;
    border-radius: 10px;
    font-size: 13px;
    margin-bottom: 18px;
  }
  .g-wrap {
    display: flex;
    justify-content: center;
    margin-bottom: 12px;
  }
  .oauth-btn {
    display: flex;
    align-items: center;
    justify-content: center;
    gap: 10px;
    width: 100%%;
    padding: 13px 16px;
    border-radius: 999px;
    border: 1px solid var(--border);
    background: #1a1d2d;
    color: var(--text);
    text-decoration: none;
    font-size: 14.5px;
    font-weight: 600;
    transition: transform .12s ease, border-color .15s ease;
    -webkit-tap-highlight-color: transparent;
  }
  .oauth-btn:active {
    transform: scale(0.96);
    border-color: var(--accent);
  }
  input[type="password"] {
    width: 100%%;
    padding: 13px 14px;
    border-radius: 11px;
    border: 1px solid var(--border);
    background: #0b0d14;
    color: var(--text);
    font-size: 16px;
    margin-bottom: 14px;
    outline: none;
  }
  button {
    width: 100%%;
    padding: 13px 16px;
    border-radius: 11px;
    border: none;
    background: var(--accent);
    color: #052622;
    font-size: 15px;
    font-weight: 700;
    cursor: pointer;
  }
</style>
</head>
<body>
  <div class="card">
    <div class="dot"></div>
    <h1>MY<span>STATION</span></h1>
    <p class="sub">Private family radio. Sign in with your authorized Google account.</p>
    %s
    %s
    %s
  </div>
</body>
</html>`, errHTML, googleSection, passcodeSection)
}
