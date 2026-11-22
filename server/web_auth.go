package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const sessionCookie = "monitor_session"
const sessionLifetime = 8 * time.Hour

type webSessions struct {
	mu       sync.Mutex
	tokens   map[[32]byte]time.Time
	failures map[string]loginAttempts
}
type loginAttempts struct {
	count int
	until time.Time
}

func sessionHash(r *http.Request) [32]byte {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return [32]byte{}
	}
	return sha256.Sum256([]byte(cookie.Value))
}

func (h *hub) authenticatedWeb(user, password string, next http.Handler) http.Handler {
	sessions := &webSessions{tokens: make(map[[32]byte]time.Time), failures: make(map[string]loginAttempts)}
	wantUser, wantPass := sha256.Sum256([]byte(user)), sha256.Sum256([]byte(password))
	validCredentials := func(u, p string) bool {
		gotUser, gotPass := sha256.Sum256([]byte(u)), sha256.Sum256([]byte(p))
		return subtle.ConstantTimeCompare(wantUser[:], gotUser[:])&subtle.ConstantTimeCompare(wantPass[:], gotPass[:]) == 1
	}
	protected := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		if r.URL.Path == "/api/login" && r.Method == "POST" {
			if r.Header.Get("Content-Type") != "application/json" {
				writeError(w, 415, "application/json required")
				return
			}
			ip, _, _ := net.SplitHostPort(r.RemoteAddr)
			now := time.Now()
			sessions.mu.Lock()
			for key, a := range sessions.failures {
				if !a.until.After(now) {
					delete(sessions.failures, key)
				}
			}
			a := sessions.failures[ip]
			if a.count >= 5 && a.until.After(now) {
				sessions.mu.Unlock()
				w.Header().Set("Retry-After", "60")
				writeError(w, 429, "登录尝试过多，请稍后再试")
				return
			}
			// Reserve an attempt before checking credentials, including concurrent requests.
			if len(sessions.failures) >= 1024 && a.count == 0 {
				sessions.mu.Unlock()
				writeError(w, 429, "登录繁忙，请稍后再试")
				return
			}
			a.count++
			a.until = now.Add(time.Minute)
			sessions.failures[ip] = a
			sessions.mu.Unlock()
			var input struct {
				Username string `json:"username"`
				Password string `json:"password"`
			}
			decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
			if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF {
				writeError(w, 400, "登录参数无效")
				return
			}
			if !validCredentials(input.Username, input.Password) {
				writeError(w, 401, "用户名或密码错误")
				return
			}
			token := make([]byte, 32)
			if _, err := rand.Read(token); err != nil {
				writeError(w, 500, "无法创建登录会话")
				return
			}
			value := hex.EncodeToString(token)
			sessions.mu.Lock()
			delete(sessions.failures, ip)
			for key, expiry := range sessions.tokens {
				if !expiry.After(now) {
					delete(sessions.tokens, key)
				}
			}
			if len(sessions.tokens) >= 1024 {
				sessions.mu.Unlock()
				writeError(w, 429, "登录会话已满，请稍后再试")
				return
			}
			sessions.tokens[sha256.Sum256([]byte(value))] = now.Add(sessionLifetime)
			sessions.mu.Unlock()
			http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: value, Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: int(sessionLifetime.Seconds())})
			writeJSON(w, 200, map[string]string{"username": user})
			return
		}
		public := r.URL.Path == "/healthz" || r.URL.Path == "/login.html" || r.URL.Path == "/login.js" || r.URL.Path == "/style.css" || r.URL.Path == "/theme.js"
		basicUser, basicPass, basicOK := r.BasicAuth()
		sessions.mu.Lock()
		expires, sessionOK := sessions.tokens[sessionHash(r)]
		sessionOK = sessionOK && expires.After(time.Now())
		if !sessionOK {
			delete(sessions.tokens, sessionHash(r))
		}
		sessions.mu.Unlock()
		if !public && !sessionOK && !(basicOK && validCredentials(basicUser, basicPass)) {
			if r.Method == "GET" && !strings.HasPrefix(r.URL.Path, "/api/") {
				http.Redirect(w, r, "/login.html", http.StatusSeeOther)
			} else {
				writeError(w, 401, "authentication required")
			}
			return
		}
		switch {
		case r.URL.Path == "/api/session" && r.Method == "GET":
			writeJSON(w, 200, map[string]string{"username": user})
			return
		case r.URL.Path == "/api/logout" && r.Method == "POST":
			sessions.mu.Lock()
			delete(sessions.tokens, sessionHash(r))
			sessions.mu.Unlock()
			http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: -1})
			writeJSON(w, 200, map[string]bool{"ok": true})
			return
		}
		next.ServeHTTP(w, r)
	})
	return http.NewCrossOriginProtection().Handler(protected)
}
