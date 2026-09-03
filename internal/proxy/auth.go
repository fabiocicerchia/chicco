package proxy

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// auth.go guards chicco's own inbound endpoints with the optional shared
// secret: bearer token for API clients, cookie for a browser on /dashboard.

// withAuth - Guards every endpoint except /health with the optional shared
// secret (top-level api_key in chicco.yaml). With no key configured chicco
// stays open, as before — fine on 127.0.0.1. Set a key when binding a public
// addr so only callers presenting `Authorization: Bearer <key>` get through.
// /health is always open so liveness probes need no secret. The key is read per
// request (under r.mu) so a SIGHUP reload can add, change, or remove it without
// a restart.
//
// A browser cannot set an Authorization header by typing a URL, so /dashboard
// also accepts the key once as ?key=<secret> and hands back a cookie; the page
// and its /v1/status polling then authenticate with that cookie.
func (r *Rotator) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		key := r.currentAuthKey()
		if key != "" && req.URL.Path != "/health" && !authorized(req, key) {
			if req.URL.Path == "/dashboard" && secretMatches(req.URL.Query().Get("key"), key) {
				http.SetCookie(w, &http.Cookie{
					Name:     authCookie,
					Value:    key,
					Path:     "/",
					HttpOnly: true,
					SameSite: http.SameSiteStrictMode,
				})
				// Redirect so the secret stops riding in the address bar.
				http.Redirect(w, req, "/dashboard", http.StatusFound)
				return
			}
			writeError(w, http.StatusUnauthorized, "chicco: missing or invalid API key")
			return
		}
		next.ServeHTTP(w, req)
	})
}

// authCookie is the browser-side carrier for the inbound shared secret, set by
// a /dashboard?key=<secret> visit.
const authCookie = "chicco_key"

// authorized - Reports whether a request presents the shared secret, either as
// a bearer token (API clients) or as the dashboard cookie (browsers).
func authorized(req *http.Request, key string) bool {
	if bearerMatches(req.Header.Get("Authorization"), key) {
		return true
	}
	c, err := req.Cookie(authCookie)
	return err == nil && secretMatches(c.Value, key)
}

// currentAuthKey - Returns the inbound shared secret under r.mu, so a reload
// writing it doesn't race the auth check reading it.
func (r *Rotator) currentAuthKey() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.authKey
}

// bearerMatches - Reports whether an Authorization header carries the expected
// bearer token, compared in constant time so a wrong key leaks no timing
// signal.
func bearerMatches(header, key string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	return secretMatches(strings.TrimSpace(header[len(prefix):]), key)
}

// secretMatches - Constant-time equality for the inbound shared secret, so a
// wrong key leaks no timing signal whichever way it was presented.
func secretMatches(got, key string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(key)) == 1
}
