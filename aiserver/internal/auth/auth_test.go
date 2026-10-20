package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestAuthGateLocalBypassAndRemoteCookie(t *testing.T) {
	gate := NewGate("secret-pass-123")
	inner := gate.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))

	// 1. Direct loopback request bypasses auth
	reqLocal := httptest.NewRequest("GET", "/api/state", nil)
	reqLocal.RemoteAddr = "127.0.0.1:54321"
	recLocal := httptest.NewRecorder()
	inner.ServeHTTP(recLocal, reqLocal)
	if recLocal.Code != http.StatusOK {
		t.Fatalf("local loopback request got %d, want 200", recLocal.Code)
	}

	// 2. Remote request via Cloudflare tunnel without cookie gets 401 on /api/*
	reqRemoteAPI := httptest.NewRequest("GET", "/api/state", nil)
	reqRemoteAPI.RemoteAddr = "127.0.0.1:54321"
	reqRemoteAPI.Header.Set("Cf-Connecting-Ip", "203.0.113.10")
	recRemoteAPI := httptest.NewRecorder()
	inner.ServeHTTP(recRemoteAPI, reqRemoteAPI)
	if recRemoteAPI.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated remote API request got %d, want 401", recRemoteAPI.Code)
	}

	// 3. Login with valid passcode sets cookie and redirects to /
	form := url.Values{"passcode": {"secret-pass-123"}}
	reqLogin := httptest.NewRequest("POST", "/auth/login", strings.NewReader(form.Encode()))
	reqLogin.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqLogin.Header.Set("Cf-Connecting-Ip", "203.0.113.10")
	recLogin := httptest.NewRecorder()
	inner.ServeHTTP(recLogin, reqLogin)
	if recLogin.Code != http.StatusSeeOther {
		t.Fatalf("login POST got %d, want 303", recLogin.Code)
	}
	cookies := recLogin.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatalf("expected session cookie to be set")
	}

	// 4. Subsequent remote request with session cookie succeeds
	reqAuthed := httptest.NewRequest("GET", "/api/state", nil)
	reqAuthed.Header.Set("Cf-Connecting-Ip", "203.0.113.10")
	reqAuthed.AddCookie(cookies[0])
	recAuthed := httptest.NewRecorder()
	inner.ServeHTTP(recAuthed, reqAuthed)
	if recAuthed.Code != http.StatusOK {
		t.Fatalf("authenticated remote request got %d, want 200", recAuthed.Code)
	}
}

func TestOAuthAllowlistAndPerEmailRevocation(t *testing.T) {
	gate := NewOAuthGate(Config{
		OwnerEmails:   []string{"jonathan.corners@gmail.com", "jonathan.corners@voxell.ai"},
		AllowedEmails: []string{"riki.corners@gmail.com", "seana.corners@gmail.com"},
		GoogleClientID: "test-client-id.apps.googleusercontent.com",
		SecretSeed:    "test-seed-secret",
		DataDir:       t.TempDir(),
	})

	var lastUser User
	inner := gate.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastUser = UserFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	now := time.Now().Unix()
	exp := now + 3600

	ownerTok := gate.signUserToken("jonathan.corners@gmail.com", now, exp)
	rikiTok := gate.signUserToken("riki.corners@gmail.com", now, exp)
	strangerTok := gate.signUserToken("stranger@gmail.com", now, exp)

	// Stranger is rejected even with a signed token because they aren't in AllowedEmails
	reqStranger := httptest.NewRequest("GET", "/api/state", nil)
	reqStranger.Header.Set("Cf-Connecting-Ip", "203.0.113.10")
	reqStranger.AddCookie(&http.Cookie{Name: cookieName, Value: strangerTok})
	recStranger := httptest.NewRecorder()
	inner.ServeHTTP(recStranger, reqStranger)
	if recStranger.Code != http.StatusUnauthorized {
		t.Fatalf("stranger got %d, want 401", recStranger.Code)
	}

	// Riki is allowed with IsOwner=false
	reqRiki := httptest.NewRequest("GET", "/api/state", nil)
	reqRiki.Header.Set("Cf-Connecting-Ip", "203.0.113.10")
	reqRiki.AddCookie(&http.Cookie{Name: cookieName, Value: rikiTok})
	recRiki := httptest.NewRecorder()
	inner.ServeHTTP(recRiki, reqRiki)
	if recRiki.Code != http.StatusOK || lastUser.Email != "riki.corners@gmail.com" || lastUser.IsOwner {
		t.Fatalf("riki auth failed: code=%d user=%+v", recRiki.Code, lastUser)
	}

	// Revoking Riki invalidates Riki's cookie while keeping Owner's cookie valid
	gate.RevokeUser("riki.corners@gmail.com")

	recRikiAfter := httptest.NewRecorder()
	inner.ServeHTTP(recRikiAfter, reqRiki)
	if recRikiAfter.Code != http.StatusUnauthorized {
		t.Fatalf("revoked riki got %d, want 401", recRikiAfter.Code)
	}

	reqOwner := httptest.NewRequest("GET", "/api/state", nil)
	reqOwner.Header.Set("Cf-Connecting-Ip", "203.0.113.10")
	reqOwner.AddCookie(&http.Cookie{Name: cookieName, Value: ownerTok})
	recOwner := httptest.NewRecorder()
	inner.ServeHTTP(recOwner, reqOwner)
	if recOwner.Code != http.StatusOK || !lastUser.IsOwner {
		t.Fatalf("owner after riki revoke got %d user=%+v", recOwner.Code, lastUser)
	}
}
