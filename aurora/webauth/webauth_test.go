package webauth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"

	"github.com/filecoin-project/curio/deps/config"
)

func hash(t *testing.T, pw string) string {
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.MinCost)
	require.NoError(t, err)
	return string(h)
}

type env struct {
	cfg *config.CurioConfig
	h   http.Handler
}

func newEnv(t *testing.T, users ...config.AuroraWebUser) *env {
	cfg := config.DefaultCurioConfig()
	cfg.Aurora.WebUI.Users.Set(users)
	cfg.HTTP.CORSOrigins = []string{"https://dash.example"}
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok:" + User(r.Context())))
	})
	return &env{cfg: cfg, h: Wrap(cfg, ok)}
}

func (e *env) do(method, path string, mod func(r *http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://gui:4701"+path, nil)
	r.RemoteAddr = "10.0.0.5:1234"
	if mod != nil {
		mod(r)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func basic(u, p string) func(r *http.Request) {
	return func(r *http.Request) { r.SetBasicAuth(u, p) }
}

func TestNoUsersRefusesEverything(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/", "/api/webrpc/v0", "/debug/pprof/", "/pages/sql/", "/static/x.js"} {
		w := e.do(http.MethodGet, p, nil)
		require.Equal(t, http.StatusUnauthorized, w.Code, p)
		require.Contains(t, w.Header().Get("WWW-Authenticate"), "Basic")
	}
	// own auth / preflight
	require.Equal(t, http.StatusOK, e.do(http.MethodPost, "/mcp", nil).Code)
	require.Equal(t, http.StatusOK, e.do(http.MethodOptions, "/api/webrpc/v0", nil).Code)
	require.Equal(t, http.StatusUnauthorized, e.do(http.MethodGet, "/mcpx", nil).Code)
}

func TestBasicLoginAndSession(t *testing.T) {
	e := newEnv(t, config.AuroraWebUser{Name: "ops", PasswordBcrypt: hash(t, "correct horse battery")})

	require.Equal(t, http.StatusUnauthorized, e.do(http.MethodGet, "/", nil).Code)
	require.Equal(t, http.StatusUnauthorized, e.do(http.MethodGet, "/", basic("ops", "wrong")).Code)
	require.Equal(t, http.StatusUnauthorized, e.do(http.MethodGet, "/", basic("nobody", "correct horse battery")).Code)

	w := e.do(http.MethodGet, "/", basic("ops", "correct horse battery"))
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "ok:ops", w.Body.String())
	cookies := w.Result().Cookies()
	require.Len(t, cookies, 1)
	c := cookies[0]
	require.Equal(t, cookieName, c.Name)
	require.True(t, c.HttpOnly)
	require.Equal(t, http.SameSiteStrictMode, c.SameSite)

	// the session cookie alone authenticates, including websocket upgrades
	w = e.do(http.MethodGet, "/api/webrpc/v0", func(r *http.Request) {
		r.AddCookie(c)
		r.Header.Set("Upgrade", "websocket")
		r.Header.Set("Origin", "http://gui:4701")
	})
	require.Equal(t, http.StatusOK, w.Code)

	// tampered cookie
	bad := *c
	bad.Value = c.Value[:len(c.Value)-2] + "xx"
	require.Equal(t, http.StatusUnauthorized, e.do(http.MethodGet, "/", func(r *http.Request) { r.AddCookie(&bad) }).Code)

	// password change ends the session
	e.cfg.Aurora.WebUI.Users.Set([]config.AuroraWebUser{{Name: "ops", PasswordBcrypt: hash(t, "a new long password")}})
	require.Equal(t, http.StatusUnauthorized, e.do(http.MethodGet, "/", func(r *http.Request) { r.AddCookie(c) }).Code)
	require.Equal(t, http.StatusUnauthorized, e.do(http.MethodGet, "/", basic("ops", "correct horse battery")).Code)
	require.Equal(t, http.StatusOK, e.do(http.MethodGet, "/", basic("ops", "a new long password")).Code)

	// disabling the user ends access
	e.cfg.Aurora.WebUI.Users.Set([]config.AuroraWebUser{{Name: "ops", PasswordBcrypt: hash(t, "a new long password"), Disabled: true}})
	require.Equal(t, http.StatusUnauthorized, e.do(http.MethodGet, "/", basic("ops", "a new long password")).Code)
}

func TestCrossOriginRefused(t *testing.T) {
	e := newEnv(t, config.AuroraWebUser{Name: "ops", PasswordBcrypt: hash(t, "correct horse battery")})
	login := basic("ops", "correct horse battery")
	withOrigin := func(o string, upgrade bool) func(r *http.Request) {
		return func(r *http.Request) {
			login(r)
			r.Header.Set("Origin", o)
			if upgrade {
				r.Header.Set("Upgrade", "websocket")
			}
		}
	}
	require.Equal(t, http.StatusForbidden, e.do(http.MethodPost, "/api/config/layers/base", withOrigin("https://evil.example", false)).Code)
	require.Equal(t, http.StatusForbidden, e.do(http.MethodGet, "/api/webrpc/v0", withOrigin("https://evil.example", true)).Code)
	require.Equal(t, http.StatusForbidden, e.do(http.MethodPost, "/x", withOrigin("null", false)).Code)
	require.Equal(t, http.StatusOK, e.do(http.MethodPost, "/x", withOrigin("http://gui:4701", false)).Code)
	require.Equal(t, http.StatusOK, e.do(http.MethodPost, "/x", withOrigin("https://dash.example", false)).Code)
	// plain cross-origin GETs (no state change) are only subject to auth
	require.Equal(t, http.StatusOK, e.do(http.MethodGet, "/x", withOrigin("https://evil.example", false)).Code)
}

func TestFailedLoginLockout(t *testing.T) {
	e := newEnv(t, config.AuroraWebUser{Name: "ops", PasswordBcrypt: hash(t, "correct horse battery")})
	for i := 0; i < maxFailures; i++ {
		require.Equal(t, http.StatusUnauthorized, e.do(http.MethodGet, "/", basic("ops", "guess")).Code)
	}
	require.Equal(t, http.StatusTooManyRequests, e.do(http.MethodGet, "/", basic("ops", "correct horse battery")).Code)
	// other clients are unaffected
	w := e.do(http.MethodGet, "/", func(r *http.Request) {
		r.RemoteAddr = "10.0.0.6:1"
		r.SetBasicAuth("ops", "correct horse battery")
	})
	require.Equal(t, http.StatusOK, w.Code)
}

func TestInvalidHashIgnored(t *testing.T) {
	e := newEnv(t, config.AuroraWebUser{Name: "ops", PasswordBcrypt: "plaintext-password"})
	require.Equal(t, http.StatusUnauthorized, e.do(http.MethodGet, "/", basic("ops", "plaintext-password")).Code)
}
