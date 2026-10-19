package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
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
