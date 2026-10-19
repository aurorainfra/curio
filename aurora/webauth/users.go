package webauth

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/curio/deps/config"
)

// BcryptCost is the cost used for new password hashes.
const BcryptCost = 12

// MinPasswordLen is the shortest password accepted for new hashes.
const MinPasswordLen = 12

const usersKey = "aurora.webui.users"

var userNameRe = regexp.MustCompile(`^[A-Za-z0-9_.@-]{1,64}$`)

// ValidUserName reports whether a web UI user name is acceptable.
func ValidUserName(n string) bool { return userNameRe.MatchString(n) }

// HashPassword returns a bcrypt hash for a new password.
func HashPassword(pw string) (string, error) {
	if len(pw) < MinPasswordLen {
		return "", xerrors.Errorf("password must be at least %d characters", MinPasswordLen)
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), BcryptCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// LayerUsers returns the web UI users a config layer sets, and whether the
// layer sets Aurora.WebUI.Users at all.
func LayerUsers(text string) ([]config.AuroraWebUser, bool, error) {
	cfg := config.DefaultCurioConfig()
	md, err := config.LoadConfigWithUpgrades(text, cfg)
	if err != nil {
		return nil, false, xerrors.Errorf("parsing layer: %w", err)
	}
	set := false
	for _, k := range md.Keys() {
		if strings.EqualFold(strings.Join(k, "."), usersKey) {
			set = true
			break
		}
	}
	return slices.Clone(cfg.Aurora.WebUI.Users.Get()), set, nil
}

// headerName returns the lower-cased table name of a [table] or
// [[array.table]] header line, and whether the line is a header.
func headerName(trimmed string) (string, bool, bool) {
	if !strings.HasPrefix(trimmed, "[") {
		return "", false, false
	}
	array := strings.HasPrefix(trimmed, "[[")
	end := strings.Index(trimmed, "]")
	if end < 0 {
		return "", false, false
	}
	name := strings.Trim(trimmed[:end], "[ \t")
	name = strings.ReplaceAll(strings.ReplaceAll(name, " ", ""), "\t", "")
	return strings.ToLower(name), array, true
}

// assignedKey returns the lower-cased dotted key of a "key = value" line.
func assignedKey(trimmed string) (string, string, bool) {
	if trimmed == "" || trimmed[0] == '#' {
		return "", "", false
	}
	k, v, ok := strings.Cut(trimmed, "=")
	if !ok {
		return "", "", false
	}
	k = strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(k), " ", ""), "\t", ""))
	return k, strings.TrimSpace(v), true
}

// bracketDelta counts unbalanced [ ] outside of strings and comments.
func bracketDelta(s string) int {
	d := 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '"' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '#':
			return d
		case c == '[':
			d++
		case c == ']':
			d--
		}
	}
	return d
}

func tomlString(s string) string { return strconv.Quote(s) }

// SetLayerUsers replaces the Aurora.WebUI.Users entries of a config layer
// and leaves every other line of the layer, including comments, as it was.
// An empty list removes the layer's users, so lower layers apply again.
func SetLayerUsers(text string, users []config.AuroraWebUser) (string, error) {
	seen := map[string]bool{}
	for _, u := range users {
		if !ValidUserName(u.Name) {
			return "", xerrors.Errorf("invalid user name %q: use letters, digits and _ . @ -", u.Name)
		}
		if seen[u.Name] {
			return "", xerrors.Errorf("duplicate user %q", u.Name)
		}
		seen[u.Name] = true
		if _, err := bcrypt.Cost([]byte(u.PasswordBcrypt)); err != nil {
			return "", xerrors.Errorf("user %q: PasswordBcrypt is not a bcrypt hash: %w", u.Name, err)
		}
	}

	var out []string
	table := ""
	skipTable := false
	skipValue := 0
	for _, l := range strings.Split(text, "\n") {
		t := strings.TrimSpace(l)
		if skipValue > 0 {
			skipValue += bracketDelta(t)
			continue
		}
		if name, _, ok := headerName(t); ok {
			table = name
			skipTable = name == usersKey
			if skipTable {
				continue
			}
		}
		if skipTable {
			continue
		}
		if k, v, ok := assignedKey(t); ok {
			full := k
			if table != "" {
				full = table + "." + k
			}
			if full == usersKey {
				skipValue = bracketDelta(v)
				continue
			}
		}
		out = append(out, l)
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}

	for _, u := range users {
		out = append(out, "", "[[Aurora.WebUI.Users]]",
			"  Name = "+tomlString(u.Name),
			"  PasswordBcrypt = "+tomlString(u.PasswordBcrypt))
		if u.Disabled {
			out = append(out, "  Disabled = true")
		}
	}
	res := strings.Join(out, "\n") + "\n"
	if len(out) == 0 {
		res = ""
	}

	got, set, err := LayerUsers(res)
	if err != nil {
		return "", xerrors.Errorf("updated layer does not parse: %w", err)
	}
	if set != (len(users) > 0) || (len(users) > 0 && !slices.Equal(got, users)) {
		return "", fmt.Errorf("updated layer does not decode to the requested users (layer defines users in an unsupported form?)")
	}
	return res, nil
}
