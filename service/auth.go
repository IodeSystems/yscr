// auth.go — optional bearer-token auth for the API surface. Off by default:
// the service is LAN/VPN-only today; this is for the day it leaves that zone.
// When cfg.Auth.Token is set, every /api/* route requires a matching
// "Authorization: Bearer <token>" header (constant-time compare). The PWA
// shell and static assets stay open — they carry no state.
package service

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

func authMiddleware(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") && !bearerOK(r, token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="yscr"`)
			http.Error(w, "unauthorized: bearer token required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerOK(r *http.Request, token string) bool {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) <= len(p) || !strings.EqualFold(h[:len(p)], p) {
		return false
	}
	got := strings.TrimSpace(h[len(p):])
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}
