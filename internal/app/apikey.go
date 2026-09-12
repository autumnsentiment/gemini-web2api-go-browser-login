package app

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

// API key for /v1/* endpoints. Source priority:
//   1. CLI flag --api-key  (highest, locked at boot)
//   2. env API_KEY         (locked at boot)
//   3. kv("api_key") in DB (runtime-mutable via admin)
//   4. auto-generated on first boot if all of the above are empty
//
// `apiKeyLocked` is true if 1 or 2 set the value — admin UI then shows it as
// read-only (you'd need to redeploy to change it).
//
// `apiKeyRuntime` is the cached current value, refreshed every getAPIKey() call
// when not locked.

var (
	apiKeyLocked  bool
	apiKeyRuntime atomic.Value // string
	apiKeyMu      sync.Mutex
)

const apiKeyPrefix = "sk-gemini-"

func newAPIKey() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return apiKeyPrefix + hex.EncodeToString(b)
}

// initAPIKey decides the boot-time value. Called once from main after DB open.
// Returns the resolved key (or "" if explicitly disabled).
func initAPIKey(flagKey string) string {
	if flagKey != "" {
		apiKeyLocked = true
		apiKeyRuntime.Store(flagKey)
		return flagKey
	}
	if env := os.Getenv("API_KEY"); env != "" {
		apiKeyLocked = true
		apiKeyRuntime.Store(env)
		return env
	}
	// runtime-mutable from DB
	cur := kvGet("api_key")
	if cur == "" {
		// first boot ever — auto-generate
		cur = newAPIKey()
		_ = kvSet("api_key", cur)
	}
	apiKeyRuntime.Store(cur)
	return cur
}

func getAPIKey() string {
	if v, ok := apiKeyRuntime.Load().(string); ok {
		return v
	}
	return ""
}

// rotateAPIKey generates a new random key and persists it. Refuses if locked.
func rotateAPIKey() (string, error) {
	apiKeyMu.Lock()
	defer apiKeyMu.Unlock()
	if apiKeyLocked {
		return "", errMsg("api key is locked by --api-key flag or env, edit deployment to change")
	}
	k := newAPIKey()
	if err := kvSet("api_key", k); err != nil {
		return "", err
	}
	apiKeyRuntime.Store(k)
	return k, nil
}

// setAPIKey persists a user-provided key (must start with sk-, length >= 20).
// Refuses if locked.
func setAPIKey(k string) error {
	apiKeyMu.Lock()
	defer apiKeyMu.Unlock()
	if apiKeyLocked {
		return errMsg("api key is locked by --api-key flag or env")
	}
	if len(k) < 20 {
		return errMsg("api key too short (min 20 chars)")
	}
	if err := kvSet("api_key", k); err != nil {
		return err
	}
	apiKeyRuntime.Store(k)
	return nil
}

// requireAPIKey wraps a /v1/* handler with bearer-token auth.
// Accepts: "Authorization: Bearer xxx" (OpenAI SDK default) or "x-api-key: xxx".
func requireAPIKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := getAPIKey()
		if key == "" {
			next(w, r)
			return
		}
		got := ""
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			got = strings.TrimSpace(h[7:])
		}
		if got == "" {
			got = strings.TrimSpace(r.Header.Get("x-api-key"))
		}
		if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(key)) != 1 {
			writeJSON(w, 401, map[string]interface{}{
				"error": map[string]string{
					"message": "Invalid API key. Set Authorization: Bearer <key> or x-api-key header.",
					"type":    "invalid_request_error",
					"code":    "invalid_api_key",
				},
			})
			return
		}
		next(w, r)
	}
}

// helpers ────────────────────────────────────────────────────────────────────

type errString string

func (e errString) Error() string { return string(e) }
func errMsg(s string) error       { return errString(s) }
