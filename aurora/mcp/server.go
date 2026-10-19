// Package mcp serves a read-only diagnostics MCP (Model Context Protocol)
// server for AI agents investigating a Curio cluster: task failures and
// stalls, WindowPoSt problems, storage path and sector location issues,
// cross-node logs and goroutine dumps.
//
// Every tool is read-only. Database access goes through a fixed catalog of
// reviewed SELECT queries (queries.go) run in READ ONLY transactions; node
// access goes through the nodediag endpoint, which only exposes fixed
// read-only operations. Agents authenticate with named bearer tokens
// configured in Aurora.MCP.Tokens; every tool call is recorded in
// aurora_mcp_calls.
package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	logging "github.com/ipfs/go-log/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/curio/build"
	"github.com/filecoin-project/curio/deps"
)

var log = logging.Logger("aurora/mcp")

// PathPrefix is where Handler is mounted on the GUI server.
const PathPrefix = "/mcp"

const maxRequestBody = 1 << 20

const serverInstructions = `Read-only diagnostics for a Curio Filecoin storage provider cluster.
Typical flows:
- Failed/stuck tasks: tasks_active, tasks_stuck, task_history (failed_only), task_detail, then log_grep around the task's work window on the machine that ran it, and goroutines (live pprof goroutine dump, not logs) on that machine for hangs.
- WindowPoSt: wdpost_deadlines for an overview, wdpost_partition for skipped/faulty sectors grouped by storage path, then storage_paths / storage_diag / sector_locate for the affected paths.
- Missing storage: storage_paths (stale heartbeats, dead URLs), missing_refs, storage_path_sectors, path_partitions (blast radius), partition_sectors (which paths hold a partition).
- Storage hardware: storage_overview (per machine, problems_only), storage_disks (SMART verdicts, filter/page), storage_disk_detail, storage_sas (HBA/expander/phy errors, JBOD slots).
- Deals rejected / "busy": market_backpressure (counters vs limits and cached verdicts per market node), market_decisions (history with reasons and CIDGravity responses), market_pipeline, config_effective (Ingest limits per node), tasks_stuck for the queued tasks behind the counters.
- Logs are per machine and can be hundreds of GB: log_query scans them as background jobs (parallel, filter stages, time windows, group/count aggregation such as group_by template to find what spams) and keeps matches on the node; refine with log_query job=<id>, page with log_job_lines, read context with log_read. log_grep is a quick synchronous first-matches search.
- Metrics: node_metrics (Prometheus, by prefix).
Machines are selected by ID, name or host:port (see cluster_machines); some tools accept "all".`

// Server is the MCP server.
type Server struct {
	d     *deps.Deps
	srv   *mcp.Server
	auth  *tokenAuth
	nodes *nodeClient
	audit *auditLog
}

// Handler returns the /mcp HTTP handler. When the MCP is disabled in config
// it serves 404.
func Handler(d *deps.Deps) http.Handler {
	if d == nil || d.Cfg == nil || !d.Cfg.Aurora.MCP.Enable {
		return http.NotFoundHandler()
	}
	s, err := newServer(d)
	if err != nil {
		log.Errorw("starting aurora MCP server", "error", err)
		return http.NotFoundHandler()
	}
	log.Infow("aurora MCP server enabled", "path", PathPrefix)

	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.srv }, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
	})
	return s.auth.middleware(http.MaxBytesHandler(h, maxRequestBody))
}

func newServer(d *deps.Deps) (*Server, error) {
	s := &Server{
		d:     d,
		auth:  newTokenAuth(d.Cfg.Aurora.MCP.Tokens),
		nodes: newNodeClient(d),
		audit: newAuditLog(d.DB, func() *int64 { return d.MachineID }),
	}
	s.srv = mcp.NewServer(&mcp.Implementation{
		Name:    "curio-aurora-diag",
		Title:   "Curio diagnostics (read-only)",
		Version: build.UserVersion(),
	}, &mcp.ServerOptions{
		Instructions: serverInstructions,
	})

	s.registerClusterTools()
	s.registerTaskTools()
	s.registerWdPostTools()
	s.registerStorageTools()
	s.registerStorageTools2()
	s.registerMarketTools()
	s.registerNodeTools()
	s.registerLogTools()
	s.registerDebugTools()

	go s.audit.pruneLoop(context.Background())
	return s, nil
}

func (s *Server) maxOutputBytes() int {
	if v := s.d.Cfg.Aurora.MCP.MaxOutputBytes.Get(); v > 0 {
		return v
	}
	return 256 << 10
}

func (s *Server) toolTimeout() time.Duration {
	if v := s.d.Cfg.Aurora.MCP.ToolTimeout.Get(); v > 0 {
		return v
	}
	return time.Minute
}

var readOnlyAnnotations = &mcp.ToolAnnotations{
	ReadOnlyHint:   true,
	IdempotentHint: true,
	OpenWorldHint:  new(false),
}

// addTool registers a read-only tool. The handler's result is returned to
// the agent as compact JSON, redacted and capped at MaxOutputBytes; every
// call is recorded in the audit log.
func addTool[In any](s *Server, name, description string, fn func(ctx context.Context, in In) (any, error)) {
	mcp.AddTool(s.srv, &mcp.Tool{
		Name:        name,
		Description: description,
		Annotations: readOnlyAnnotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		start := time.Now()
		tctx, cancel := context.WithTimeout(ctx, s.toolTimeout())
		defer cancel()

		out, err := fn(tctx, in)

		var text string
		var truncated bool
		if err == nil {
			b, merr := json.Marshal(out)
			if merr != nil {
				err = xerrors.Errorf("encoding result: %w", merr)
			} else {
				text, truncated = capOutput(redact(string(b), s.secrets()), s.maxOutputBytes())
			}
		}
		if err != nil {
			err = xerrors.New(redact(err.Error(), s.secrets()))
		}

		var args json.RawMessage
		if req != nil && req.Params != nil {
			args = req.Params.Arguments
		}
		s.audit.record(agentFromContext(ctx), name, args, time.Since(start), len(text), truncated, err)

		if err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
	})
}
