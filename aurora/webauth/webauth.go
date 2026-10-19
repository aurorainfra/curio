// Package webauth authenticates everything served on the web UI (GUI)
// address. Users are configured in Aurora.WebUI.Users with bcrypt password
// hashes. Browsers log in with HTTP Basic auth and then carry a signed,
// HttpOnly, SameSite=Strict session cookie, which also covers websocket
// connections. Non-browser clients can send Basic credentials on every
// request.
//
// Exempt are CORS preflight requests (they carry no credentials and the
// CORS handler answers them without running any endpoint) and /mcp, which
// requires its own bearer tokens. Unsafe requests and websocket upgrades
// from another origin are refused unless the origin is listed in
// HTTP.CORSOrigins.
package webauth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	logging "github.com/ipfs/go-log/v2"
	"golang.org/x/crypto/bcrypt"

	"github.com/filecoin-project/curio/deps/config"
)

var log = logging.Logger("aurora/webauth")

const (
	cookieName = "curio_ui_session"
	realm      = "Curio"

	// verified Basic credentials are cached so non-browser clients do not
	// pay a bcrypt verification per request
	credCacheTTL = 5 * time.Minute

	maxFailures   = 10
	failureWindow = time.Minute
	lockout       = time.Minute

	defaultSessionTTL = 12 * time.Hour
)

// exemptPrefix has its own authentication.
const exemptPrefix = "/mcp"

type userKey struct{}

// User returns the authenticated web UI user of a request.
func User(ctx context.Context) string {
	u, _ := ctx.Value(userKey{}).(string)
	return u
}

type credEntry struct {
	user    string
	fp      string
	expires time.Time
}

type failures struct {
	count int
	first time.Time
	until time.Time
}

// Auth is the web UI authentication middleware.
type Auth struct {
	users       *config.Dynamic[[]config.AuroraWebUser]
	ttl         *config.Dynamic[time.Duration]
	corsOrigins []string
	key         []byte

	mu       sync.Mutex
	creds    map[[32]byte]credEntry
	failures map[string]*failures
	warned   map[string]bool
}

func New(cfg *config.CurioConfig) *Auth {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	a := &Auth{
		users:       cfg.Aurora.WebUI.Users,
		ttl:         cfg.Aurora.WebUI.SessionTTL,
		corsOrigins: cfg.HTTP.CORSOrigins,
		key:         key,
		creds:       map[[32]byte]credEntry{},
		failures:    map[string]*failures{},
		warned:      map[string]bool{},
	}
	if len(a.enabledUsers()) == 0 {
		log.Errorw("no web UI users configured: every request to the web UI will be refused; add users to Aurora.WebUI.Users (e.g. with 'curio config edit base')")
	}
	return a
}

// Wrap returns next guarded by web UI authentication.
func Wrap(cfg *config.CurioConfig, next http.Handler) http.Handler {
	return New(cfg).Middleware(next)
}

func (a *Auth) enabledUsers() map[string]config.AuroraWebUser {
	out := map[string]config.AuroraWebUser{}
	if a.users == nil {
		return out
	}
	for _, u := range a.users.Get() {
		if u.Disabled || u.Name == "" || !strings.HasPrefix(u.PasswordBcrypt, "$2") {
			if !u.Disabled && u.Name != "" {
				a.warnOnce("invalid:"+u.Name, "ignoring web UI user without a bcrypt PasswordBcrypt", "user", u.Name)
			}
			continue
		}
		out[u.Name] = u
	}
	return out
}

func (a *Auth) warnOnce(key, msg string, kv ...any) {
	a.mu.Lock()
	seen := a.warned[key]
	a.warned[key] = true
	a.mu.Unlock()
	if !seen {
		log.Warnw(msg, kv...)
	}
}

// fingerprint changes when a user's password changes, ending its sessions.
func fingerprint(u config.AuroraWebUser) string {
	h := sha256.Sum256([]byte(u.PasswordBcrypt))
	return hex.EncodeToString(h[:8])
}

func (a *Auth) sessionTTL() time.Duration {
	if a.ttl != nil {
		if v := a.ttl.Get(); v > 0 {
			return v
		}
	}
	return defaultSessionTTL
}

func (a *Auth) mac(user, fp string, exp int64) string {
	m := hmac.New(sha256.New, a.key)
	m.Write([]byte(user + "\x00" + fp + "\x00" + strconv.FormatInt(exp, 10)))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (a *Auth) newCookie(user, fp string, secure bool) *http.Cookie {
	exp := time.Now().Add(a.sessionTTL())
	val := base64.RawURLEncoding.EncodeToString([]byte(user)) + "." + strconv.FormatInt(exp.Unix(), 10) + "." + a.mac(user, fp, exp.Unix())
	return &http.Cookie{
		Name:     cookieName,
		Value:    val,
		Path:     "/",
		Expires:  exp,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
	}
}

// checkCookie returns the user of a valid session cookie.
func (a *Auth) checkCookie(r *http.Request, users map[string]config.AuroraWebUser) (string, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return "", false
	}
	parts := strings.Split(c.Value, ".")
	if len(parts) != 3 {
		return "", false
	}
	ub, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", false
	}
	u, ok := users[string(ub)]
	if !ok {
		return "", false
	}
	want := a.mac(u.Name, fingerprint(u), exp)
	if subtle.ConstantTimeCompare([]byte(want), []byte(parts[2])) != 1 {
		return "", false
	}
	return u.Name, true
}

// checkBasic verifies Basic credentials.
func (a *Auth) checkBasic(r *http.Request, users map[string]config.AuroraWebUser) (string, string, bool) {
	name, pass, ok := r.BasicAuth()
	if !ok {
		return "", "", false
	}
	ck := sha256.Sum256([]byte(name + "\x00" + pass))
	u, exists := users[name]

	a.mu.Lock()
	ce, cached := a.creds[ck]
	a.mu.Unlock()
	if cached && exists && time.Now().Before(ce.expires) && ce.fp == fingerprint(u) {
		return u.Name, ce.fp, true
	}
	if !exists {
		// spend comparable time on unknown user names
		_ = bcrypt.CompareHashAndPassword(dummyHash(), []byte(pass))
		return "", "", false
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordBcrypt), []byte(pass)) != nil {
		return "", "", false
	}
	log.Infow("web UI login", "user", u.Name, "ip", clientIP(r))
	fp := fingerprint(u)
	a.mu.Lock()
	if len(a.creds) > 1024 {
		a.creds = map[[32]byte]credEntry{}
	}
	a.creds[ck] = credEntry{user: u.Name, fp: fp, expires: time.Now().Add(credCacheTTL)}
	a.mu.Unlock()
	return u.Name, fp, true
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (a *Auth) lockedOut(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	f, ok := a.failures[ip]
	return ok && time.Now().Before(f.until)
}

func (a *Auth) recordFailure(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	f, ok := a.failures[ip]
	if !ok || now.Sub(f.first) > failureWindow {
		if len(a.failures) > 4096 {
			a.failures = map[string]*failures{}
		}
		f = &failures{first: now}
		a.failures[ip] = f
	}
	f.count++
	if f.count >= maxFailures {
		f.until = now.Add(lockout)
		log.Warnw("too many failed web UI logins, locking out", "ip", ip, "for", lockout)
	}
}

// sameOrigin reports whether a browser request comes from the UI itself or
// an allowed CORS origin. Requests without an Origin header (non-browser
// clients, same-origin GETs) pass.
func (a *Auth) sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err == nil && strings.EqualFold(u.Host, r.Host) {
		return true
	}
	for _, o := range a.corsOrigins {
		if o == "*" || strings.EqualFold(o, origin) {
			return true
		}
	}
	return false
}

func unsafeRequest(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
	default:
		return true
	}
}

func exempt(r *http.Request) bool {
	if r.Method == http.MethodOptions {
		return true
	}
	return r.URL.Path == exemptPrefix || strings.HasPrefix(r.URL.Path, exemptPrefix+"/")
}

func unauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", `Basic realm="`+realm+`", charset="UTF-8"`)
	http.Error(w, msg, http.StatusUnauthorized)
}

// Middleware guards next.
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if exempt(r) {
			next.ServeHTTP(w, r)
			return
		}
		if unsafeRequest(r) && !a.sameOrigin(r) {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}

		users := a.enabledUsers()
		if len(users) == 0 {
			unauthorized(w, "web UI access is disabled until users are configured in Aurora.WebUI.Users")
			return
		}

		if user, ok := a.checkCookie(r, users); ok {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, user)))
			return
		}

		ip := clientIP(r)
		if a.lockedOut(ip) {
			http.Error(w, "too many failed logins, try again later", http.StatusTooManyRequests)
			return
		}
		user, fp, ok := a.checkBasic(r, users)
		if !ok {
			if _, _, sent := r.BasicAuth(); sent {
				a.recordFailure(ip)
				log.Warnw("failed web UI login", "ip", ip, "path", r.URL.Path)
			}
			unauthorized(w, "authentication required")
			return
		}

		http.SetCookie(w, a.newCookie(user, fp, r.TLS != nil))
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, user)))
	})
}

var (
	dummyOnce sync.Once
	dummy     []byte
)

func dummyHash() []byte {
	dummyOnce.Do(func() {
		pw := make([]byte, 16)
		_, _ = rand.Read(pw)
		dummy, _ = bcrypt.GenerateFromPassword(pw, 12)
	})
	return dummy
}
