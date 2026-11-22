package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testWebHandler(t *testing.T) http.Handler {
	t.Helper()
	db, err := openStore(filepath.Join(t.TempDir(), "web.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.db.Close() })
	return (&hub{store: db, offlineAfter: time.Second, sessions: map[string]*session{}}).webRoutes("monitor", "test")
}

func TestWebSessionLoginLogoutAndCSRF(t *testing.T) {
	handler := testWebHandler(t)
	request := func(method, path, body string, cookie *http.Cookie, origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "https://monitor.test"+path, strings.NewReader(body))
		if method == "POST" {
			req.Header.Set("Content-Type", "application/json")
		}
		if cookie != nil {
			req.AddCookie(cookie)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	if rec := request("GET", "/", "", nil, ""); rec.Code != 303 || rec.Header().Get("Location") != "/login.html" {
		t.Fatalf("missing login redirect: %v", rec)
	}
	if rec := request("GET", "/api/session", "", nil, ""); rec.Code != 401 || rec.Header().Get("WWW-Authenticate") != "" {
		t.Fatal("unauthorized API must not trigger a browser Basic Auth prompt")
	}
	for _, path := range []string{"/login.html", "/theme.js", "/style.css"} {
		if rec := request("GET", path, "", nil, ""); rec.Code != 200 {
			t.Fatalf("login appearance asset must be public: %s, status %d", path, rec.Code)
		}
	}
	rec := request("POST", "/api/login", `{"username":"monitor","password":"test"}`, nil, "https://evil.test")
	if rec.Code != 403 {
		t.Fatalf("cross-origin login accepted: %d", rec.Code)
	}
	rec = request("POST", "/api/login", `{"username":"monitor","password":"test"}`, nil, "https://monitor.test")
	if rec.Code != 200 || len(rec.Result().Cookies()) != 1 {
		t.Fatalf("login failed: %s", rec.Body.String())
	}
	cookie := rec.Result().Cookies()[0]
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode || cookie.MaxAge != 8*3600 {
		t.Fatalf("unsafe session cookie: %v", cookie)
	}
	if rec := request("GET", "/api/session", "", cookie, ""); rec.Code != 200 {
		t.Fatal("valid session rejected")
	}
	if rec := request("GET", "/api/session", "", &http.Cookie{Name: sessionCookie, Value: cookie.Value + "forged"}, ""); rec.Code != 401 {
		t.Fatal("forged session accepted")
	}
	if rec := request("POST", "/api/logout", "{}", cookie, "https://evil.test"); rec.Code != 403 {
		t.Fatal("cross-origin logout accepted")
	}
	if rec := request("POST", "/api/logout", "{}", cookie, "https://monitor.test"); rec.Code != 200 {
		t.Fatal("logout failed")
	}
	if rec := request("GET", "/api/session", "", cookie, ""); rec.Code != 401 {
		t.Fatal("logged-out session was replayable")
	}
}

func TestWebLoginAttemptLimit(t *testing.T) {
	handler := testWebHandler(t)
	for i := 0; i < 6; i++ {
		req := httptest.NewRequest("POST", "https://monitor.test/api/login", strings.NewReader(`{"username":"monitor","password":"wrong"}`))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		want := 401
		if i == 5 {
			want = 429
		}
		if rec.Code != want {
			t.Fatalf("attempt %d: status %d want %d", i, rec.Code, want)
		}
	}
}
