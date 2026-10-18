package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/filecoin-project/curio/deps"
)

// AuroraRPC exposes the MCP tool-call log to the web UI. It is embedded in
// the CurioWeb webrpc handler, so method names carry an AuroraMCP prefix.
type AuroraRPC struct {
	d *deps.Deps
}

func NewAuroraRPC(d *deps.Deps) *AuroraRPC {
	return &AuroraRPC{d: d}
}

// MCPCall is one recorded tool call.
type MCPCall struct {
	ID          int64     `db:"id"`
	TS          time.Time `db:"ts"`
	Agent       string    `db:"agent"`
	Tool        string    `db:"tool"`
	Args        string    `db:"args"`
	DurationMS  int64     `db:"duration_ms"`
	ResultBytes int64     `db:"result_bytes"`
	Truncated   bool      `db:"truncated"`
	Error       string    `db:"error"`
}

// AuroraMCPCalls lists recorded tool calls, newest first. before pages by
// call ID (0 = newest).
func (a *AuroraRPC) AuroraMCPCalls(ctx context.Context, limit int, agent, tool string, before int64) ([]MCPCall, error) {
	limit = clamp(limit, 100, 1000)
	var out []MCPCall
	err := a.d.DB.Select(ctx, &out, qMCPCalls, agent, tool, before, limit)
	return out, err
}

// MCPAgent describes a configured agent token, without the token.
type MCPAgent struct {
	Name     string
	Disabled bool
	// Valid is false for entries the server ignores (no name, or
	// TokenSHA256 not 64 hex characters).
	Valid    bool
	LastCall *time.Time
	Calls24h int64
}

type MCPStatus struct {
	// Enabled reports whether this GUI node serves /mcp.
	Enabled bool
	Agents  []MCPAgent
}

// AuroraMCPStatus reports whether MCP is enabled on this node and the
// configured agents with their recent activity.
func (a *AuroraRPC) AuroraMCPStatus(ctx context.Context) (*MCPStatus, error) {
	st := &MCPStatus{Enabled: a.d.Cfg.Aurora.MCP.Enable}

	var activity []struct {
		Agent    string    `db:"agent"`
		LastCall time.Time `db:"last_call"`
		Calls24h int64     `db:"calls_24h"`
	}
	if err := a.d.DB.Select(ctx, &activity, qMCPAgentActivity, time.Now().Add(-24*time.Hour)); err != nil {
		return nil, err
	}
	byAgent := map[string]int{}
	for i, act := range activity {
		byAgent[act.Agent] = i
	}

	if a.d.Cfg.Aurora.MCP.Tokens != nil {
		for _, t := range a.d.Cfg.Aurora.MCP.Tokens.Get() {
			h, err := hex.DecodeString(strings.TrimSpace(t.TokenSHA256))
			ag := MCPAgent{Name: t.Name, Disabled: t.Disabled, Valid: err == nil && len(h) == sha256.Size && t.Name != ""}
			if i, ok := byAgent[t.Name]; ok {
				ag.LastCall = &activity[i].LastCall
				ag.Calls24h = activity[i].Calls24h
			}
			st.Agents = append(st.Agents, ag)
		}
	}
	return st, nil
}
