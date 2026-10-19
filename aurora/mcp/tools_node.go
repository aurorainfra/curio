package mcp

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/xerrors"

	"github.com/filecoin-project/curio/aurora/nodediag"
)

const (
	maxMetricsBody   = 64 << 20
	defaultMaxSeries = 500
	maxMaxSeries     = 5000
)

type nodeMetricsArgs struct {
	Machine   string   `json:"machine" jsonschema:"machine ID, name or host:port, or \"all\""`
	Prefixes  []string `json:"prefixes,omitempty" jsonschema:"metric name prefixes to return, e.g. curio_harmonytask_, curio_nvme_, go_goroutines; omit with list_names to discover names"`
	Match     string   `json:"match,omitempty" jsonschema:"RE2 matched against the whole series line (name, labels and value), e.g. a task name label"`
	ListNames bool     `json:"list_names,omitempty" jsonschema:"return metric names with their series counts instead of series"`
	MaxSeries int      `json:"max_series,omitempty" jsonschema:"series per machine, default 500, max 5000"`
}

type metricsOut struct {
	Series    []string       `json:",omitempty"`
	Names     map[string]int `json:",omitempty"`
	Truncated bool           `json:",omitempty"`
}

type configEffectiveArgs struct {
	Machine  string   `json:"machine" jsonschema:"machine ID, name or host:port, or \"all\""`
	Sections []string `json:"sections,omitempty" jsonschema:"config sections to return: Subsystems, Fees, Proving, HTTP, Market, Ingest, Seal, Batching, Cuzk, Aurora (Apis and Alerting are never returned; secret-looking keys are redacted). Empty lists the layers and available sections."`
}

func (s *Server) fetchMetrics(ctx context.Context, m machine, in nodeMetricsArgs, re *regexp.Regexp) (*metricsOut, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+m.HostPort+"/debug/metrics", nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.nodes.client.Do(req)
	if err != nil {
		return nil, xerrors.Errorf("%s: %w", m.label(), err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, xerrors.Errorf("%s: HTTP %d", m.label(), resp.StatusCode)
	}
	out := &metricsOut{}
	maxSeries := clamp(in.MaxSeries, defaultMaxSeries, maxMaxSeries)
	if in.ListNames {
		out.Names = map[string]int{}
	}
	sc := bufio.NewScanner(io.LimitReader(resp.Body, maxMetricsBody))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name := line
		if i := strings.IndexAny(line, "{ "); i > 0 {
			name = line[:i]
		}
		if len(in.Prefixes) > 0 {
			ok := false
			for _, p := range in.Prefixes {
				if strings.HasPrefix(name, p) {
					ok = true
					break
				}
			}
			if !ok {
				continue
			}
		}
		if re != nil && !re.MatchString(line) {
			continue
		}
		if in.ListNames {
			out.Names[name]++
			continue
		}
		if len(out.Series) >= maxSeries {
			out.Truncated = true
			break
		}
		out.Series = append(out.Series, line)
	}
	sort.Strings(out.Series)
	return out, sc.Err()
}

func (s *Server) registerNodeTools() {
	addTool(s, "node_metrics",
		"Prometheus metrics of a machine (or all), filtered by metric name prefixes and/or a regex: harmonytask queue and run metrics, market, sealing, NVMe, Go runtime and more. Use list_names first to discover metric names.",
		func(ctx context.Context, in nodeMetricsArgs) (any, error) {
			if len(in.Prefixes) == 0 && in.Match == "" && !in.ListNames {
				return nil, xerrors.New("pass prefixes or match, or list_names to discover metric names")
			}
			var re *regexp.Regexp
			if in.Match != "" {
				if len(in.Match) > 512 {
					return nil, xerrors.New("match longer than 512 bytes")
				}
				var err error
				if re, err = regexp.Compile(in.Match); err != nil {
					return nil, xerrors.Errorf("invalid match: %w", err)
				}
			}
			ms, err := s.selectMachines(ctx, in.Machine, true)
			if err != nil {
				return nil, err
			}
			out := make([]NodeResult[metricsOut], len(ms))
			done := make(chan struct{}, len(ms))
			for i, m := range ms {
				out[i] = NodeResult[metricsOut]{MachineID: m.ID, Machine: m.Name}
				go func() {
					defer func() { done <- struct{}{} }()
					r, err := s.fetchMetrics(ctx, m, in, re)
					if err != nil {
						out[i].Error = err.Error()
						return
					}
					out[i].Result = r
				}()
			}
			for range ms {
				<-done
			}
			return out, nil
		})

	addTool(s, "config_effective",
		"The effective (layered) configuration a machine runs with, per section, plus its layer list. Use it to check limits such as Ingest.MaxQueueSDR or which subsystems are enabled on a node.",
		func(ctx context.Context, in configEffectiveArgs) (any, error) {
			ms, err := s.selectMachines(ctx, in.Machine, true)
			if err != nil {
				return nil, err
			}
			return callNodes[nodediag.ConfigResponse](ctx, s, ms, nodediag.OpConfig, nodediag.ConfigRequest{Sections: in.Sections}), nil
		})
}
