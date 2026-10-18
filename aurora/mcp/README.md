# Aurora diagnostics MCP

A read-only [MCP](https://modelcontextprotocol.io) server that lets AI agents investigate a Curio cluster. Typical uses:
- failed or stuck tasks;
- WindowPoSt failures and skipped sectors;
- task timing;
- storage paths that dropped and the sector index entries that went missing with them;
- logs across all machines;
- live goroutine dumps.

## Pieces

| Package / file | Runs on | What it does |
|---|---|---|
| `aurora/mcp` | the GUI node (`Subsystems.GuiAddress`) | MCP server at `/mcp` (streamable HTTP, stateless, JSON responses). Token auth, the query catalog, tool-call audit log. |
| `aurora/nodediag` | every node, on the node RPC port | `/aurora/diag/v0/*`: log files, byte-range read, streaming grep, time bisection, pprof goroutine dump, fixed storage diagnostics. |
| `web/static/pages/aurora-mcp` | GUI | Lists the configured agents and the tool-call log (Tools → MCP). |
| `harmony/harmonydb/sql/20260623-aurora-mcp-audit.sql` | DB | The `aurora_mcp_calls` tool-call log. Rows are pruned after 30 days. |

Upstream files are touched only by one-line hooks:
- `web/srv.go` mounts `/mcp`;
- `cmd/curio/rpc/rpc.go` mounts `/aurora/diag/v0`;
- `web/api/webrpcporep/routes.go` embeds `AuroraRPC`;
- `deps/config/types.go` adds the `Aurora.MCP` section;
- `web/static/ux/curio-ux.mjs` adds the nav entry.

## Safety model

- **No writes.**
  - Database reads are fixed `SELECT` constants in `queries.go`, with typed parameters only. They run in a `READ ONLY` transaction that is always rolled back.
  - `TestQueryCatalogReadOnly` rejects any constant that is not a single SELECT, or that calls a function outside a builtin allowlist.
  - There is no raw SQL tool. The DB user is a superuser, many plpgsql functions write, and YSQL cannot prove an arbitrary query free of side effects.
- **No agent-controlled paths or commands.**
  - Log files are chosen by name from a list the node builds itself: the go-log file, stdout/stderr redirected to a file, `Aurora.MCP.LogFiles`, and their rotated siblings.
  - Storage diagnostics take only a storage path ID, which must be attached to that node. The node resolves the mount and backing disks itself from `/proc` and `/sys`, validates device names, and runs a fixed command list via `exec` with no shell.
- **Auth.**
  - `/mcp` requires `Authorization: Bearer <token>` matching an enabled entry in `Aurora.MCP.Tokens`. Only the token's SHA-256 is stored in config, and the comparison is constant-time. Each agent may have at most 8 requests in flight.
  - Node fan-out uses a JWT signed with the cluster storage RPC secret that carries only the `aurora-diag` permission. That permission grants nothing on `/rpc/v0` or `/remote`, and `/aurora/diag/v0` accepts nothing else, not even admin.
- **Bounded output.** Results are capped at `MaxOutputBytes` and calls at `ToolTimeout`. Log scans are capped by bytes, matches and time, with at most 4 concurrent scans per node.
- **Redaction.** JWTs, `Authorization` headers, PEM private keys, the storage RPC secret and chain API tokens are removed from all tool output.
- **Audit.** Every call is logged to `aurora_mcp_calls` and to the `aurora/mcp` logger, with agent, tool, arguments, duration, size and error.

### Not changed by this (operator note)

The GUI port has no authentication upstream. That includes the SQL console (`/pages/sql`, arbitrary SQL as the DB superuser) and pprof. `/debug/pprof` on the node RPC port is also unauthenticated. Keep both ports firewalled to operator networks.

## Setup

1. Enable it on the GUI node's layer and add a token:

   ```sh
   tok=$(openssl rand -hex 32); echo "token: $tok"; echo -n "$tok" | sha256sum
   ```

   ```toml
   [Aurora.MCP]
     Enable = true            # restart required
     ToolTimeout = "1m0s"
     MaxOutputBytes = 262144

     [[Aurora.MCP.Tokens]]
       Name = "claude-ops"
       TokenSHA256 = "<sha256 from above>"
   ```

   You can add, rotate and disable tokens (`Disabled = true`) without a restart.

2. Logs: log tools find the go-log file (`GOLOG_FILE`), or stdout/stderr when they are redirected to a file. When logs are written somewhere else, list them in `Aurora.MCP.LogFiles` in a node-specific layer. JSON logs (`GOLOG_LOG_FMT=json`) give the best level, logger and time filtering, though plaintext and colour output are parsed too. Rotation should use `copytruncate` or rename. Rotated `.N` and `.gz` files are listed.

3. Optional SMART and kernel log access for `storage_diag`, when curio does not run as root (`/etc/sudoers.d/curio-diag`):

   ```
   curio ALL=(root) NOPASSWD: /usr/sbin/smartctl -a -j /dev/sd*, /usr/sbin/smartctl -a -j /dev/nvme*, /usr/sbin/nvme smart-log -o json /dev/nvme*, /usr/bin/dmesg -T --level=err\,warn
   ```

   Adjust the binary paths to your distribution; the node looks in `/usr/sbin`, `/sbin`, `/usr/bin`, `/bin` and `/usr/local/{s,}bin`. Without the rule, these checks report `Unavailable` and the rest still runs.

4. Connect an agent, for example Claude Code:

   ```sh
   claude mcp add --transport http curio http://<gui-host>:4701/mcp --header "Authorization: Bearer $tok"
   ```

## Tools

| Area | Tools |
|---|---|
| Cluster | `cluster_machines`, `alerts` |
| Tasks | `tasks_active`, `tasks_stuck`, `task_history`, `task_detail`, `task_timing_stats` |
| WindowPoSt | `wdpost_deadlines`, `wdpost_partition` |
| Storage | `storage_paths`, `sector_locate`, `storage_path_sectors`, `missing_refs`, `storage_diag` |
| Logs (per machine or `all`) | `log_files`, `log_read`, `log_grep`, `log_tail`, `log_find_time` |
| Runtime | `goroutines` (live pprof goroutine profile, grouped by identical stacks) |

Working with huge logs:
1. `log_find_time` turns a time into a byte offset.
2. `log_grep` from that offset with `since`/`until` stops shortly after `until`.
3. A stopped scan resumes from `ScannedUntil`.
4. `log_read` reads around a match's `Offset`.
