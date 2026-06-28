package main

// Stage 16: the api authenticates every request (except /health) with an X-API-Key header
// that resolves to a team, mirroring E2B's api-key->team model. The key is hashed (sha256)
// before it touches the store, which holds only the hash; the resolved team rides in the
// request context so the handlers can scope reads and authorise writes to it. A missing or
// unknown key is 401; a store/DB failure is 500 (distinct, so a database hiccup isn't
// mistaken for a bad key). See docs/STAGE16_DESIGN.md.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
)

// teamCtxKey carries the authenticated team from withAuth into the handlers.
type teamCtxKey struct{}

// hashKey is the one-way function from a plaintext API key to the value the store holds. A
// real system would salt/peppers; for this learning project a plain sha256 hex keeps the
// store free of plaintext keys while staying trivially reproducible (the seed path hashes the
// same way).
func hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// newAccessToken mints the per-sandbox data-plane secret (Stage 16): "sbx_" + 128 random bits
// of hex. client-proxy requires it (X-Access-Token) before routing to the in-VM control
// services. crypto/rand failing is catastrophic (no entropy), so the caller treats the error
// as fatal to the create rather than hand out a weak token.
func newAccessToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "sbx_" + hex.EncodeToString(b[:]), nil
}

// withAuth wraps a handler so it runs only for a request carrying a valid X-API-Key. On
// success the resolved team is stored in the request context (read with teamFromContext).
func (a *api) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimSpace(r.Header.Get("X-API-Key"))
		if key == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing X-API-Key header"})
			return
		}
		team, ok, err := a.store.ResolveAPIKey(hashKey(key))
		if err != nil {
			// The store (DB) is unreachable: a dependency failure, not a bad key. 500, not 401,
			// so a transient outage isn't reported to the caller as "your key is wrong".
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "auth lookup failed: " + err.Error()})
			return
		}
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid API key"})
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), teamCtxKey{}, team)))
	}
}

// teamFromContext returns the team withAuth stored. It is only called from handlers behind
// withAuth, so the value is always present; the empty-string fallback is defensive.
func teamFromContext(ctx context.Context) string {
	team, _ := ctx.Value(teamCtxKey{}).(string)
	return team
}

// seedAPIKeys parses a "key=team,key2=team2" spec and seeds each pair (creating the team and
// registering the hashed key, both idempotently). A bare entry "key" (no '=') maps to the
// "default" team. Empty spec seeds nothing (a deployment that provisions keys out-of-band).
// This is the learning-scale stand-in for E2B's team/key admin -- keys come from config, not
// a management API.
func (a *api) seedAPIKeys(spec string) error {
	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		key, team := entry, "default"
		if i := strings.IndexByte(entry, '='); i >= 0 {
			key, team = strings.TrimSpace(entry[:i]), strings.TrimSpace(entry[i+1:])
		}
		if key == "" || team == "" {
			continue
		}
		if err := a.store.EnsureTeam(team, team); err != nil {
			return err
		}
		if err := a.store.InsertAPIKey(hashKey(key), team); err != nil {
			return err
		}
	}
	return nil
}
