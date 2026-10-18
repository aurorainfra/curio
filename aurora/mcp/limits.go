package mcp

import (
	"fmt"
	"regexp"
	"strings"
)

// redactPatterns match credentials that may show up in log lines, task
// errors or command output.
var redactPatterns = []*regexp.Regexp{
	// JWTs (API tokens, storage auth headers)
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`),
	// Authorization headers
	regexp.MustCompile(`(?i)(authorization["']?\s*[:=]\s*["']?)(bearer|basic)\s+[^\s"',}]+`),
	// PEM private keys
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
	// Lotus-style "token:multiaddr" API info
	regexp.MustCompile(`\b[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}:/(ip4|ip6|dns|dns4|dns6)/`),
}

const redacted = "[REDACTED]"

// redact removes credentials from tool output: configured secrets verbatim
// and anything matching redactPatterns.
func redact(s string, secrets []string) string {
	for _, sec := range secrets {
		if len(sec) >= 8 {
			s = strings.ReplaceAll(s, sec, redacted)
		}
	}
	for i, re := range redactPatterns {
		if i == 1 {
			s = re.ReplaceAllString(s, "${1}"+redacted)
			continue
		}
		s = re.ReplaceAllString(s, redacted)
	}
	return s
}

// secrets lists configured secret values that must never be returned.
func (s *Server) secrets() []string {
	var out []string
	if s.d.Cfg.Apis.StorageRPCSecret != "" {
		out = append(out, s.d.Cfg.Apis.StorageRPCSecret)
	}
	for _, ai := range s.d.Cfg.Apis.ChainApiInfo {
		if tok, _, ok := strings.Cut(ai, ":"); ok {
			out = append(out, tok)
		}
	}
	out = append(out, s.auth.secrets()...)
	return out
}

// capOutput truncates s to max bytes.
func capOutput(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	return strings.ToValidUTF8(s[:max], "") + fmt.Sprintf("\n…[truncated: result was %d bytes, limit %d; narrow the query or use offsets/limits]", len(s), max), true
}
