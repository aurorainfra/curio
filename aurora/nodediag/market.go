package nodediag

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strings"

	"github.com/filecoin-project/curio/deps/config"
	"github.com/filecoin-project/curio/market/backpressure"
)

// marketBackpressure explains this node's market backpressure with its own
// (layered) ingest config.
func (h *handler) marketBackpressure(ctx context.Context, _ struct{}) (*backpressure.Report, error) {
	if h.d == nil || h.d.Cfg == nil || h.d.DB == nil {
		return nil, errors.New("no config or database on this node")
	}
	return backpressure.Inspect(ctx, &h.d.Cfg.Ingest, h.d.DB)
}

// config sections that may be returned; Apis and Alerting hold credentials
var configSections = map[string]func(c *config.CurioConfig) any{
	"Subsystems": func(c *config.CurioConfig) any { return &c.Subsystems },
	"Fees":       func(c *config.CurioConfig) any { return &c.Fees },
	"Proving":    func(c *config.CurioConfig) any { return &c.Proving },
	"HTTP":       func(c *config.CurioConfig) any { return &c.HTTP },
	"Market":     func(c *config.CurioConfig) any { return &c.Market },
	"Ingest":     func(c *config.CurioConfig) any { return &c.Ingest },
	"Seal":       func(c *config.CurioConfig) any { return &c.Seal },
	"Batching":   func(c *config.CurioConfig) any { return &c.Batching },
	"Cuzk":       func(c *config.CurioConfig) any { return &c.Cuzk },
	"Aurora":     func(c *config.CurioConfig) any { return &c.Aurora },
}

// keys whose values are never returned
var secretKey = regexp.MustCompile(`(?i)^(\s*#?\s*)([A-Za-z0-9_]*(token|secret|password|passwd|key|webhook|apiinfo|sha256|auth|credential)[A-Za-z0-9_]*)(\s*=\s*)(.*)$`)

func redactTOML(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if m := secretKey.FindStringSubmatch(l); m != nil {
			lines[i] = m[1] + m[2] + m[4] + `"[REDACTED]"`
		}
	}
	return strings.Join(lines, "\n")
}

type ConfigRequest struct {
	// Sections to return; empty returns the list of available sections.
	Sections []string `json:",omitempty"`
}

type ConfigResponse struct {
	// Layers this node was started with, in order.
	Layers    []string
	Available []string
	// Sections maps section names to their effective (merged) TOML.
	Sections map[string]string `json:",omitempty"`
}

func (h *handler) effectiveConfig(_ context.Context, req ConfigRequest) (*ConfigResponse, error) {
	if h.d == nil || h.d.Cfg == nil {
		return nil, errors.New("no config on this node")
	}
	resp := &ConfigResponse{Layers: h.d.Layers, Sections: map[string]string{}}
	for k := range configSections {
		resp.Available = append(resp.Available, k)
	}
	sort.Strings(resp.Available)
	for _, s := range req.Sections {
		get, ok := configSections[s]
		if !ok {
			return nil, badRequest("section " + s + " is not available; available: " + strings.Join(resp.Available, ", "))
		}
		b, err := config.TransparentMarshal(get(h.d.Cfg))
		if err != nil {
			return nil, err
		}
		resp.Sections[s] = redactTOML(string(b))
	}
	return resp, nil
}
