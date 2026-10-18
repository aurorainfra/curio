package mcp

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"

	"github.com/filecoin-project/curio/deps/config"
)

// maxInflightPerAgent bounds concurrent requests per token.
const maxInflightPerAgent = 8

type agentKey struct{}

func agentFromContext(ctx context.Context) string {
	if a, ok := ctx.Value(agentKey{}).(string); ok {
		return a
	}
	return ""
}

// tokenAuth checks bearer tokens against Aurora.MCP.Tokens, which stores
// SHA-256 hashes of the tokens. The token list is dynamic, so tokens can be
// added, rotated and disabled without a restart.
type tokenAuth struct {
	tokens *config.Dynamic[[]config.AuroraMCPToken]

	lk       sync.Mutex
	inflight map[string]int
	warned   map[string]bool
}

func newTokenAuth(tokens *config.Dynamic[[]config.AuroraMCPToken]) *tokenAuth {
	return &tokenAuth{
		tokens:   tokens,
		inflight: map[string]int{},
		warned:   map[string]bool{},
	}
}

// authenticate returns the name of the agent owning the token.
func (a *tokenAuth) authenticate(token string) (string, bool) {
	if a.tokens == nil || len(token) < 32 {
		return "", false
	}
	sum := sha256.Sum256([]byte(token))

	var agent string
	for _, t := range a.tokens.Get() {
		want, err := hex.DecodeString(strings.TrimSpace(t.TokenSHA256))
		if err != nil || len(want) != sha256.Size || t.Name == "" {
			a.warnInvalid(t.Name)
			continue
		}
		// compare against every entry so timing does not depend on position
		if subtle.ConstantTimeCompare(sum[:], want) == 1 && !t.Disabled && agent == "" {
			agent = t.Name
		}
	}
	return agent, agent != ""
}

func (a *tokenAuth) warnInvalid(name string) {
	a.lk.Lock()
	defer a.lk.Unlock()
	if !a.warned[name] {
		a.warned[name] = true
		log.Warnw("ignoring MCP token with invalid name or TokenSHA256 (want 64 hex chars)", "name", name)
	}
}

func (a *tokenAuth) acquire(agent string) bool {
	a.lk.Lock()
	defer a.lk.Unlock()
	if a.inflight[agent] >= maxInflightPerAgent {
		return false
	}
	a.inflight[agent]++
	return true
}

func (a *tokenAuth) release(agent string) {
	a.lk.Lock()
	defer a.lk.Unlock()
	a.inflight[agent]--
}

// secrets returns strings that must never appear in tool output.
func (a *tokenAuth) secrets() []string {
	if a.tokens == nil {
		return nil
	}
	var out []string
	for _, t := range a.tokens.Get() {
		if s := strings.TrimSpace(t.TokenSHA256); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (a *tokenAuth) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := r.Header.Get("Authorization")
		token, ok := strings.CutPrefix(hdr, "Bearer ")
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="curio-mcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		agent, ok := a.authenticate(strings.TrimSpace(token))
		if !ok {
			log.Warnw("rejected MCP request with invalid token", "remote", r.RemoteAddr)
			w.Header().Set("WWW-Authenticate", `Bearer realm="curio-mcp", error="invalid_token"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !a.acquire(agent) {
			http.Error(w, "too many concurrent requests", http.StatusTooManyRequests)
			return
		}
		defer a.release(agent)

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), agentKey{}, agent)))
	})
}
