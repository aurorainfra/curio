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
| `aurora/marketlog` + `20260625-aurora-market-decisions.sql` | market nodes | Records every MK12 deal decision (reason, backpressure verdicts seen, CIDGravity response) in `aurora_market_decisions`, kept 7 days. |
| `market/backpressure/aurora_inspect.go` | market nodes | Read-only breakdown of the backpressure checks, plus the cached verdicts deals currently see. |

Upstream files are touched only by one-line hooks:
- `web/srv.go` mounts `/mcp`;
- `cmd/curio/rpc/rpc.go` mounts `/aurora/diag/v0`;
- `web/api/webrpcporep/routes.go` embeds `AuroraRPC`;
- `deps/config/types.go` adds the `Aurora.MCP` section;
- `web/static/ux/curio-ux.mjs` adds the nav entry;
- `market/backpressure/backpressure.go` registers each backpressure cache for inspection (1 line);
- `market/mk12/mk12.go` records decisions in `ExecuteDeal` (2 lines) and the backpressure verdicts in `maybeApplyBackpressure`;
- `market/mk12/cidgravity.go` records the CIDGravity response (1 line).

`market/mk12/aurora_marketlog.go` holds the helpers these hooks call.

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
- **Bounded output.** Results are capped at `MaxOutputBytes` and calls at `ToolTimeout`. Synchronous log reads and greps are capped by bytes, matches and time, with at most 4 concurrent per node. `log_query` jobs share a per-node worker pool, are capped at 8 running per node, and are bounded by `MaxLogJobRuntime` and the scratch space budget.
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

3. Optional root-only diagnostics, when curio does not run as root (`/etc/sudoers.d/curio-diag`):

   ```
   Cmnd_Alias CURIO_DIAG = /usr/sbin/smartctl -a -j /dev/sd*, /usr/sbin/smartctl -a -j /dev/nvme*, \
                           /usr/sbin/smartctl -x -j /dev/sd*, /usr/sbin/smartctl -x -j /dev/nvme*, \
                           /usr/sbin/nvme smart-log -o json /dev/nvme*, /usr/sbin/nvme error-log -o json /dev/nvme*, \
                           /usr/bin/sg_ses --page=2 /dev/sg*, /usr/bin/dmesg -T --level=err\,warn
   curio ALL=(root) NOPASSWD: CURIO_DIAG
   ```

   Adjust the binary paths to your distribution; the node looks in `/usr/sbin`, `/sbin`, `/usr/bin`, `/bin` and `/usr/local/{s,}bin`. Without the rule:
   - SMART verdicts show as `unknown`;
   - enclosure element status (`ses`) is unavailable;
   - everything read from sysfs still works, including the SAS topology, phy error counters and enclosure slot status.

4. Connect an agent, for example Claude Code:

   ```sh
   claude mcp add --transport http curio http://<gui-host>:4701/mcp --header "Authorization: Bearer $tok"
   ```

## Tools

| Area | Tools |
|---|---|
| Cluster | `cluster_machines`, `alerts`, `config_effective` (merged config per node, allowlisted sections, secrets redacted), `node_metrics` (Prometheus, by prefix/regex) |
| Tasks | `tasks_active`, `tasks_stuck`, `task_history`, `task_detail`, `task_timing_stats` |
| WindowPoSt | `wdpost_deadlines`, `wdpost_partition`, `partition_sectors` (partition → storage paths, missing sealed/cache) |
| Storage index | `storage_paths`, `sector_locate`, `storage_path_sectors`, `missing_refs`, `path_partitions` (path → partitions at risk, sole copies) |
| Storage hardware | `storage_overview` (per-path health incl. hung mounts and mounts fallen back to `/`), `storage_disks` (SMART verdicts, hundreds of drives, filter/page), `storage_disk_detail`, `storage_sas` (HBAs, expanders, phy error counters with deltas, JBOD slots, optional SES), `storage_diag` (one path in depth) |
| Market | `market_backpressure` (counters vs limits per market node, cached verdicts and their expiry), `market_decisions` (deal decision history with reasons, backpressure and CIDGravity responses), `market_pipeline` |
| Logs (per machine or `all`) | `log_files`, `log_query`, `log_job`, `log_job_lines`, `log_job_cancel`, `log_jobs`, `log_grep`, `log_read`, `log_tail`, `log_find_time` |
| Runtime | `goroutines` (live pprof goroutine profile, grouped by identical stacks) |

### Logs at scale

`log_query` runs a background job on each selected node and returns within about 50 s, either with the result or with progress; poll with `log_job`. How a job works:

- **Parallel scan.** Uncompressed files are split into chunks scanned in parallel (`Aurora.MCP.LogScanWorkers`, default a quarter of the CPUs). Gzip files are streamed.
- **Time window.** With `since`/`until`, the scanned range of each uncompressed file is first narrowed by bisecting on line timestamps, so an hour inside a 300 GB file costs only that hour. Rotated files last written before `since` are skipped.
- **Filter stages.** Stages run in order: `include`/`exclude` (literal or RE2), `field`/`exclude_field` (go-log JSON fields such as `msg`, `caller`, `error`), `level`, and `logger`/`exclude_logger`.
- **Output modes.** `lines`, `count`, or `group` by up to 4 keys: `template` (messages with numbers, CIDs, addresses, UUIDs and hex normalized, best for "what is spamming"), `logger`, `level`, `caller`, `msg`, `file`, time buckets (`minute`, `10m`, `hour`, `day`), `field:<key>`, or a regex capture. With `machine: "all"`, counts and groups are also merged across machines.
- **Chaining.** Matched lines are kept on the node, in `Aurora.MCP.ScratchDir` (default `/tmp`), capped by `MaxScratchBytes` and at half the free space, for an hour. `log_job_lines` pages through them with their original file and offset. A new `log_query` with `job=<id>` filters them again without rescanning, so a first pass over gigabytes can be refined cheaply many times.
- **Limits.** Each job is bounded by `Aurora.MCP.MaxLogJobRuntime` (default 1 h) and an optional `max_scan_bytes`. At most 8 jobs run per node.

### Storage hardware at scale

SMART results are cached for 15 minutes and refreshed in the background, 8 disks at a time. Disks still being checked show `pending`. Every tool leads with a summary (verdict counts, `Problems`) and supports `problems_only`, filters and paging.

`storage_sas` reports phy error counter deltas since its previous call on that node. Calling it twice a few minutes apart shows whether link errors are still increasing.

### Market "busy"

1. `market_backpressure` on the market node shows which counter is over which `Ingest` limit, and whether deals still see a cached verdict and for how long.
2. `market_decisions` shows when rejections started, why, and what CIDGravity answered (decision, internal/external message, matching rule).
3. `market_pipeline`, `tasks_active` and `tasks_stuck` show what is behind the counters.

`market_backpressure` re-runs the upstream checks for comparison, which logs their usual `backpressure` info lines when a check trips.
