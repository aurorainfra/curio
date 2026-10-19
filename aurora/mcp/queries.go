package mcp

// The query catalog. Every database read done by the MCP is one of these
// constants, run with typed $n parameters inside a READ ONLY transaction
// (see ro). TestQueryCatalogReadOnly checks that every constant in this file
// is a plain SELECT / WITH ... SELECT that calls no non-builtin functions.
//
// Keep this file to SQL constants only.

// ---- cluster ----

const qMachineList = `
SELECT hm.id, hm.host_and_port, hm.last_contact,
       COALESCE(NULLIF(hmd.machine_name, ''), hm.host_and_port) AS name
FROM harmony_machines hm
LEFT JOIN harmony_machine_details hmd ON hmd.machine_id = hm.id
ORDER BY hm.id`

const qMachines = `
SELECT hm.id, hm.host_and_port,
       COALESCE(NULLIF(hmd.machine_name, ''), hm.host_and_port) AS name,
       hm.last_contact,
       EXTRACT(EPOCH FROM (now() - hm.last_contact))::bigint AS since_contact_s,
       hm.cpu, hm.ram, hm.gpu,
       COALESCE(hm.unschedulable, FALSE) AS unschedulable,
       hm.restart_request,
       COALESCE(hmd.tasks, '') AS tasks,
       COALESCE(hmd.layers, '') AS layers,
       COALESCE(hmd.miners, '') AS miners,
       hmd.startup_time,
       COALESCE(hmd.version, '') AS version,
       (SELECT count(*) FROM harmony_task t WHERE t.owner_id = hm.id) AS running_tasks
FROM harmony_machines hm
LEFT JOIN harmony_machine_details hmd ON hmd.machine_id = hm.id
ORDER BY hm.id`

const qAlertConditions = `
SELECT system, subsystem, condition, message, created_at, last_seen_at, repeat_count
FROM alert_conditions
ORDER BY last_seen_at DESC
LIMIT $1`

const qAlertHistory = `
SELECT id, alert_name, message, COALESCE(machine_name, '') AS machine_name, created_at,
       COALESCE(acknowledged, FALSE) AS acknowledged
FROM alert_history
WHERE created_at >= $1
ORDER BY created_at DESC
LIMIT $2`

// ---- tasks ----

const qTaskCounts = `
SELECT name,
       count(*) FILTER (WHERE owner_id IS NULL) AS queued,
       count(*) FILTER (WHERE owner_id IS NOT NULL) AS running,
       min(posted_time) AS oldest_posted
FROM harmony_task
GROUP BY name
ORDER BY name`

const qTasksActive = `
SELECT t.id, t.name, t.owner_id,
       COALESCE(NULLIF(hmd.machine_name, ''), hm.host_and_port, '') AS owner,
       t.posted_time, t.update_time, t.retries, t.initiated_by, t.previous_task,
       EXTRACT(EPOCH FROM (now() - t.posted_time))::bigint AS age_s,
       EXTRACT(EPOCH FROM (now() - t.update_time))::bigint AS since_update_s
FROM harmony_task t
LEFT JOIN harmony_machines hm ON hm.id = t.owner_id
LEFT JOIN harmony_machine_details hmd ON hmd.machine_id = t.owner_id
WHERE ($1::text = '' OR t.name = $1::text)
  AND ($2::bigint < 0 OR t.owner_id = $2::bigint)
  AND (NOT $3::bool OR t.owner_id IS NOT NULL)
ORDER BY t.posted_time
LIMIT $4`

// $1 history window start, $2 owner contact cutoff, $3 minimum running
// seconds, $4 queued cutoff, $5 limit
const qTasksStuck = `
WITH p AS (
    SELECT name,
           percentile_cont(0.99) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (work_end - work_start)))::float8 AS p99_s,
           count(*) AS n
    FROM harmony_task_history
    WHERE work_end >= $1 AND result
    GROUP BY name
)
SELECT t.id, t.name, t.owner_id,
       COALESCE(NULLIF(hmd.machine_name, ''), hm.host_and_port, '') AS owner,
       hm.last_contact AS owner_last_contact,
       (t.owner_id IS NOT NULL AND hm.id IS NULL) AS owner_gone,
       t.posted_time, t.update_time, t.retries,
       EXTRACT(EPOCH FROM (now() - t.posted_time))::bigint AS age_s,
       EXTRACT(EPOCH FROM (now() - t.update_time))::bigint AS since_update_s,
       p.p99_s, COALESCE(p.n, 0) AS history_n
FROM harmony_task t
LEFT JOIN p ON p.name = t.name
LEFT JOIN harmony_machines hm ON hm.id = t.owner_id
LEFT JOIN harmony_machine_details hmd ON hmd.machine_id = t.owner_id
WHERE (t.owner_id IS NOT NULL AND (
          hm.id IS NULL
          OR hm.last_contact < $2
          OR EXTRACT(EPOCH FROM (now() - t.update_time)) > GREATEST($3::float8, COALESCE(p.p99_s * 2, 0))))
   OR (t.owner_id IS NULL AND t.posted_time < $4)
ORDER BY t.update_time
LIMIT $5`

const qTaskHistory = `
SELECT h.id, h.task_id, h.name, h.posted, h.work_start, h.work_end, h.result,
       left(COALESCE(h.err, ''), $8::int) AS err,
       h.completed_by_host_and_port,
       EXTRACT(EPOCH FROM (h.work_end - h.work_start))::float8 AS duration_s,
       EXTRACT(EPOCH FROM (h.work_start - h.posted))::float8 AS wait_s
FROM harmony_task_history h
WHERE h.work_end >= $1 AND h.work_end <= $2
  AND ($3::text = '' OR h.name = $3::text)
  AND ($4::text = '' OR h.completed_by_host_and_port = $4::text)
  AND (NOT $5::bool OR NOT h.result)
  AND ($6::bigint < 0 OR h.task_id = $6::bigint)
ORDER BY h.work_end DESC
LIMIT $7`

const qTaskByID = `
SELECT t.id, t.name, t.owner_id,
       COALESCE(NULLIF(hmd.machine_name, ''), hm.host_and_port, '') AS owner,
       t.posted_time, t.update_time, t.retries, t.initiated_by, t.previous_task,
       EXTRACT(EPOCH FROM (now() - t.posted_time))::bigint AS age_s,
       EXTRACT(EPOCH FROM (now() - t.update_time))::bigint AS since_update_s
FROM harmony_task t
LEFT JOIN harmony_machines hm ON hm.id = t.owner_id
LEFT JOIN harmony_machine_details hmd ON hmd.machine_id = t.owner_id
WHERE t.id = $1`

const qTaskRefsSDR = `
SELECT sp_id, sector_number
FROM sectors_sdr_pipeline
WHERE $1::bigint IN (task_id_sdr, task_id_tree_d, task_id_tree_c, task_id_tree_r, task_id_synth,
                     task_id_precommit_msg, task_id_porep, task_id_finalize, task_id_move_storage,
                     task_id_commit_msg)`

const qTaskRefsSnap = `
SELECT sp_id, sector_number
FROM sectors_snap_pipeline
WHERE $1::bigint IN (task_id_encode, task_id_prove, task_id_submit, task_id_move_storage)`

const qTaskRefsWdPost = `
SELECT 'WdPost' AS kind, sp_id, proving_period_start, deadline_index, partition_index
FROM wdpost_partition_tasks WHERE task_id = $1
UNION ALL
SELECT 'WdPostRecover' AS kind, sp_id, proving_period_start, deadline_index, partition_index
FROM wdpost_recovery_tasks WHERE task_id = $1
UNION ALL
SELECT 'WdPostSubmit' AS kind, sp_id, proving_period_start, deadline, partition
FROM wdpost_proofs WHERE submit_task_id = $1`

// $1 window start, $2 by machine, $3 task name filter
const qTaskTiming = `
SELECT h.name,
       CASE WHEN $2::bool THEN h.completed_by_host_and_port ELSE '' END AS machine,
       count(*) AS n,
       count(*) FILTER (WHERE h.result) AS ok,
       count(*) FILTER (WHERE NOT h.result) AS failed,
       percentile_cont(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (h.work_end - h.work_start)))::float8 AS p50_s,
       percentile_cont(0.9) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (h.work_end - h.work_start)))::float8 AS p90_s,
       percentile_cont(0.99) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (h.work_end - h.work_start)))::float8 AS p99_s,
       max(EXTRACT(EPOCH FROM (h.work_end - h.work_start)))::float8 AS max_s,
       percentile_cont(0.5) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (h.work_start - h.posted)))::float8 AS p50_wait_s,
       percentile_cont(0.9) WITHIN GROUP (ORDER BY EXTRACT(EPOCH FROM (h.work_start - h.posted)))::float8 AS p90_wait_s
FROM harmony_task_history h
WHERE h.work_end >= $1
  AND ($3::text = '' OR h.name = $3::text)
GROUP BY 1, 2
ORDER BY 1, 2`

// ---- windowPoSt ----

const qWdPostProofs = `
SELECT p.sp_id, p.proving_period_start, p.deadline, p.partition,
       p.submit_at_epoch, p.submit_by_epoch, p.proof_params,
       p.submit_task_id, COALESCE(p.message_cid, '') AS message_cid,
       mw.executed_tsk_epoch, mw.executed_rcpt_exitcode,
       (mw.signed_message_cid IS NOT NULL) AS wait_tracked
FROM wdpost_proofs p
LEFT JOIN message_waits mw ON mw.signed_message_cid = p.message_cid
WHERE p.sp_id = $1 AND p.proving_period_start = ANY($2::bigint[]) AND p.deadline = ANY($3::bigint[])
ORDER BY p.proving_period_start, p.deadline, p.partition`

const qWdPostTasks = `
SELECT pt.task_id, pt.sp_id, pt.proving_period_start, pt.deadline_index, pt.partition_index,
       (t.id IS NOT NULL) AS task_live, t.owner_id,
       h.result AS last_result, left(COALESCE(h.err, ''), 2000) AS last_err,
       h.work_start AS last_work_start, h.work_end AS last_work_end,
       COALESCE(h.completed_by_host_and_port, '') AS last_machine
FROM wdpost_partition_tasks pt
LEFT JOIN harmony_task t ON t.id = pt.task_id
LEFT JOIN LATERAL (
    SELECT hh.result, hh.err, hh.work_start, hh.work_end, hh.completed_by_host_and_port
    FROM harmony_task_history hh
    WHERE hh.task_id = pt.task_id
    ORDER BY hh.work_end DESC
    LIMIT 1
) h ON TRUE
WHERE pt.sp_id = $1 AND pt.proving_period_start = ANY($2::bigint[]) AND pt.deadline_index = ANY($3::bigint[])
ORDER BY pt.proving_period_start, pt.deadline_index, pt.partition_index`

const qWdPostLatestPeriod = `
SELECT max(proving_period_start) FROM (
    SELECT proving_period_start FROM wdpost_proofs WHERE sp_id = $1 AND deadline = $2 AND partition = $3
    UNION ALL
    SELECT proving_period_start FROM wdpost_partition_tasks WHERE sp_id = $1 AND deadline_index = $2 AND partition_index = $3
) x`

const qWdPostTaskHistory = `
SELECT h.id, h.task_id, h.name, h.posted, h.work_start, h.work_end, h.result,
       left(COALESCE(h.err, ''), 4000) AS err,
       h.completed_by_host_and_port,
       EXTRACT(EPOCH FROM (h.work_end - h.work_start))::float8 AS duration_s,
       EXTRACT(EPOCH FROM (h.work_start - h.posted))::float8 AS wait_s
FROM harmony_task_history h
WHERE h.task_id = ANY($1::bigint[])
ORDER BY h.work_end DESC
LIMIT 50`

const qWdPostPartitionTaskIDs = `
SELECT 'WdPost' AS kind, task_id FROM wdpost_partition_tasks
WHERE sp_id = $1 AND proving_period_start = $2 AND deadline_index = $3 AND partition_index = $4
UNION ALL
SELECT 'WdPostRecover' AS kind, task_id FROM wdpost_recovery_tasks
WHERE sp_id = $1 AND proving_period_start = $2 AND deadline_index = $3 AND partition_index = $4
UNION ALL
SELECT 'WdPostSubmit' AS kind, submit_task_id FROM wdpost_proofs
WHERE sp_id = $1 AND proving_period_start = $2 AND deadline = $3 AND partition = $4 AND submit_task_id IS NOT NULL`

const qWdPostRecentFailures = `
SELECT h.id, h.task_id, h.name, h.posted, h.work_start, h.work_end, h.result,
       left(COALESCE(h.err, ''), 2000) AS err,
       h.completed_by_host_and_port,
       EXTRACT(EPOCH FROM (h.work_end - h.work_start))::float8 AS duration_s,
       EXTRACT(EPOCH FROM (h.work_start - h.posted))::float8 AS wait_s
FROM harmony_task_history h
WHERE h.name IN ('WdPost', 'WdPostSubmit', 'WdPostRecover')
  AND NOT h.result AND h.work_end >= $1
ORDER BY h.work_end DESC
LIMIT $2`

// ---- storage and sectors ----

const qStoragePaths = `
SELECT sp.storage_id, COALESCE(sp.urls, '') AS urls, sp.weight, sp.max_storage,
       COALESCE(sp.can_seal, FALSE) AS can_seal, COALESCE(sp.can_store, FALSE) AS can_store,
       COALESCE(sp.groups, '') AS groups, COALESCE(sp.allow_to, '') AS allow_to,
       COALESCE(sp.allow_types, '') AS allow_types, COALESCE(sp.deny_types, '') AS deny_types,
       COALESCE(sp.allow_miners, '') AS allow_miners, COALESCE(sp.deny_miners, '') AS deny_miners,
       sp.capacity, sp.available, sp.fs_available, sp.reserved, sp.used,
       sp.last_heartbeat,
       EXTRACT(EPOCH FROM (now() - sp.last_heartbeat))::bigint AS heartbeat_age_s
FROM storage_path sp
ORDER BY sp.storage_id`

const qStorageURLLiveness = `
SELECT storage_id, url, last_checked, last_live, last_dead, COALESCE(last_dead_reason, '') AS last_dead_reason
FROM sector_path_url_liveness
ORDER BY storage_id, url`

const qStoragePathFileCounts = `
SELECT storage_id, sector_filetype, count(*) AS files
FROM sector_location
GROUP BY storage_id, sector_filetype
ORDER BY storage_id, sector_filetype`

const qSectorLocations = `
SELECT sl.sector_filetype, sl.storage_id, sl.is_primary, sl.read_ts, sl.read_refs, sl.write_ts,
       COALESCE(sl.write_lock_owner, '') AS write_lock_owner,
       COALESCE(sp.urls, '') AS urls, sp.last_heartbeat,
       (sp.storage_id IS NULL) AS path_missing
FROM sector_location sl
LEFT JOIN storage_path sp ON sp.storage_id = sl.storage_id
WHERE sl.miner_id = $1 AND sl.sector_num = $2
ORDER BY sl.sector_filetype, sl.storage_id`

const qSectorMeta = `
SELECT reg_seal_proof, ticket_epoch, orig_sealed_cid, cur_sealed_cid, cur_unsealed_cid,
       COALESCE(msg_cid_precommit, '') AS msg_cid_precommit,
       COALESCE(msg_cid_commit, '') AS msg_cid_commit,
       COALESCE(msg_cid_update, '') AS msg_cid_update,
       is_cc, expiration_epoch, deadline, partition, is_live, is_faulty, has_sector_key
FROM sectors_meta
WHERE sp_id = $1 AND sector_num = $2`

const qSectorSDRPipeline = `
SELECT (to_jsonb(p) - 'ticket_value' - 'seed_value' - 'porep_proof' - 'precommit_msg_tsk' - 'commit_msg_tsk')::text
FROM sectors_sdr_pipeline p
WHERE p.sp_id = $1 AND p.sector_number = $2`

const qSectorSnapPipeline = `
SELECT (to_jsonb(p) - 'proof' - 'prove_msg_tsk')::text
FROM sectors_snap_pipeline p
WHERE p.sp_id = $1 AND p.sector_number = $2`

const qPathSectorCounts = `
SELECT sector_filetype, count(*) AS files, count(DISTINCT miner_id) AS miners
FROM sector_location
WHERE storage_id = $1
GROUP BY sector_filetype
ORDER BY sector_filetype`

const qPathSectorSample = `
SELECT miner_id, sector_num, sector_filetype, is_primary
FROM sector_location
WHERE storage_id = $1 AND ($2::int = 0 OR sector_filetype = $2::int)
ORDER BY miner_id, sector_num, sector_filetype
LIMIT $3`

// live sectors with no sealed (sealed/update) or no cache (cache/update-cache)
// file in the sector index
const qMissingSectorFiles = `
SELECT m.sp_id, m.sector_num, m.deadline, m.partition, m.is_faulty, m.has_sector_key,
       COALESCE(bool_or(sl.sector_filetype IN (2, 8)), FALSE) AS has_sealed,
       COALESCE(bool_or(sl.sector_filetype IN (4, 16)), FALSE) AS has_cache
FROM sectors_meta m
LEFT JOIN sector_location sl
       ON sl.miner_id = m.sp_id AND sl.sector_num = m.sector_num AND sl.sector_filetype IN (2, 4, 8, 16)
WHERE m.is_live AND ($1::bigint < 0 OR m.sp_id = $1::bigint)
GROUP BY m.sp_id, m.sector_num, m.deadline, m.partition, m.is_faulty, m.has_sector_key
HAVING NOT COALESCE(bool_or(sl.sector_filetype IN (2, 8)), FALSE)
    OR NOT COALESCE(bool_or(sl.sector_filetype IN (4, 16)), FALSE)
ORDER BY m.sp_id, m.sector_num
LIMIT $2`

// sector_location rows pointing at storage paths that no longer exist
const qDanglingLocations = `
SELECT sl.storage_id, count(*) AS files, count(DISTINCT (sl.miner_id, sl.sector_num)) AS sectors
FROM sector_location sl
LEFT JOIN storage_path sp ON sp.storage_id = sl.storage_id
WHERE sp.storage_id IS NULL
GROUP BY sl.storage_id
ORDER BY sl.storage_id`

const qSectorsByPath = `
SELECT sl.storage_id, sl.sector_num, sl.sector_filetype
FROM sector_location sl
WHERE sl.miner_id = $1 AND sl.sector_num = ANY($2::bigint[]) AND sl.sector_filetype IN (2, 4, 8, 16)
ORDER BY sl.storage_id, sl.sector_num`

// ---- audit log ----

const qMCPCalls = `
SELECT id, ts, agent, tool, args::text AS args, duration_ms, result_bytes, truncated, COALESCE(error, '') AS error
FROM aurora_mcp_calls
WHERE ($1::text = '' OR agent = $1::text) AND ($2::text = '' OR tool = $2::text) AND ($3::bigint <= 0 OR id < $3::bigint)
ORDER BY id DESC
LIMIT $4`

const qMCPAgentActivity = `
SELECT agent, max(ts) AS last_call, count(*) FILTER (WHERE ts >= $1) AS calls_24h
FROM aurora_mcp_calls
GROUP BY agent`

// ---- market ----

const qMarketDecisions = `
SELECT id, ts, machine, market, deal_uuid, sp_id, COALESCE(client, '') AS client,
       COALESCE(piece_cid, '') AS piece_cid, COALESCE(piece_size, 0) AS piece_size,
       COALESCE(verified, FALSE) AS verified, COALESCE(offline, FALSE) AS offline,
       accepted, COALESCE(reason, '') AS reason, COALESCE(error, '') AS error,
       bp_mk12, bp_sector, COALESCE(cidgravity::text, '') AS cidgravity, COALESCE(duration_ms, 0) AS duration_ms
FROM aurora_market_decisions
WHERE ts >= $1 AND ts <= $2
  AND ($3::bigint < 0 OR sp_id = $3::bigint)
  AND ($4::text = '' OR client = $4::text)
  AND ($5::text = 'all' OR ($5::text = 'accepted') = accepted)
  AND ($6::text = '' OR strpos(lower(COALESCE(reason, '') || ' ' || COALESCE(cidgravity::text, '')), lower($6::text)) > 0)
ORDER BY ts DESC
LIMIT $7`

const qMarketDecisionReasons = `
SELECT accepted, left(COALESCE(NULLIF(reason, ''), error, ''), 200) AS reason,
       count(*) AS n, min(ts) AS first_ts, max(ts) AS last_ts,
       count(*) FILTER (WHERE bp_mk12) AS bp_mk12, count(*) FILTER (WHERE bp_sector) AS bp_sector
FROM aurora_market_decisions
WHERE ts >= $1 AND ts <= $2 AND ($3::bigint < 0 OR sp_id = $3::bigint)
GROUP BY 1, 2
ORDER BY n DESC
LIMIT 100`

const qMarketDecisionHourly = `
SELECT date_trunc('hour', ts) AS hour,
       count(*) FILTER (WHERE accepted) AS accepted,
       count(*) FILTER (WHERE NOT accepted) AS rejected,
       count(*) FILTER (WHERE bp_mk12 OR bp_sector) AS under_backpressure
FROM aurora_market_decisions
WHERE ts >= $1 AND ts <= $2 AND ($3::bigint < 0 OR sp_id = $3::bigint)
GROUP BY 1
ORDER BY 1`

const qMK12PipelineStages = `
SELECT count(*) AS active,
       count(*) FILTER (WHERE NOT started) AS not_started,
       count(*) FILTER (WHERE started AND NOT after_commp) AS before_commp,
       count(*) FILTER (WHERE after_commp AND NOT after_psd) AS before_publish,
       count(*) FILTER (WHERE after_psd AND NOT after_find_deal) AS before_find_deal,
       count(*) FILTER (WHERE after_find_deal AND sector IS NULL) AS waiting_for_sector,
       count(*) FILTER (WHERE sector IS NOT NULL AND NOT sealed) AS sealing,
       count(*) FILTER (WHERE sealed AND should_index AND NOT indexed) AS indexing,
       count(*) FILTER (WHERE offline) AS offline,
       min(created_at) AS oldest
FROM market_mk12_deal_pipeline
WHERE NOT complete`

const qMK12PipelineOldest = `
SELECT uuid, sp_id, created_at, offline, COALESCE(started, FALSE) AS started,
       COALESCE(after_commp, FALSE) AS after_commp, COALESCE(after_psd, FALSE) AS after_psd,
       COALESCE(after_find_deal, FALSE) AS after_find_deal, sector, COALESCE(sealed, FALSE) AS sealed,
       COALESCE(indexed, FALSE) AS indexed, commp_task_id, psd_task_id, find_deal_task_id, indexing_task_id
FROM market_mk12_deal_pipeline
WHERE NOT complete
ORDER BY created_at
LIMIT $1`

const qMK20PipelineStages = `
SELECT count(*) AS active,
       count(*) FILTER (WHERE NOT downloaded) AS downloading,
       count(*) FILTER (WHERE downloaded AND NOT after_commp) AS before_commp,
       count(*) FILTER (WHERE after_commp AND NOT aggregated) AS before_aggregation,
       count(*) FILTER (WHERE aggregated AND sector IS NULL) AS waiting_for_sector,
       count(*) FILTER (WHERE sector IS NOT NULL AND NOT sealed) AS sealing,
       count(*) FILTER (WHERE sealed AND indexing AND NOT indexed) AS indexing,
       min(created_at) AS oldest
FROM market_mk20_pipeline
WHERE NOT complete`

const qParkedPieces = `
SELECT count(*) AS incomplete,
       count(*) FILTER (WHERE pp.task_id IS NULL) AS no_task,
       count(*) FILTER (WHERE pp.task_id IS NOT NULL AND t.owner_id IS NULL) AS queued,
       count(*) FILTER (WHERE t.owner_id IS NOT NULL) AS running,
       min(pp.created_at) AS oldest
FROM parked_pieces pp
LEFT JOIN harmony_task t ON t.id = pp.task_id
WHERE NOT pp.complete`

const qOpenDealSectors = `
SELECT sp_id, sector_number, is_snap, count(*) AS pieces, sum(piece_size) AS piece_bytes, min(created_at) AS oldest_piece
FROM open_sector_pieces
GROUP BY sp_id, sector_number, is_snap
ORDER BY min(created_at)
LIMIT $1`

const qFailedMK12Deals = `
SELECT left(error, 200) AS error, count(*) AS n, max(created_at) AS last, min(uuid) AS sample_uuid
FROM market_mk12_deals
WHERE error IS NOT NULL AND error <> '' AND created_at >= $1
GROUP BY 1
ORDER BY n DESC
LIMIT 50`

// ---- partitions and paths ----

const qMetaPartitionSectors = `
SELECT sector_num, partition, is_faulty
FROM sectors_meta
WHERE sp_id = $1 AND deadline = $2 AND ($3::bigint < 0 OR partition = $3::bigint) AND is_live
ORDER BY partition, sector_num`

// per deadline/partition, the live sectors with files on a storage path, and
// how many have no sealed/update copy on any other path
const qPathPartitions = `
WITH s AS (
    SELECT DISTINCT sl.miner_id, sl.sector_num
    FROM sector_location sl
    WHERE sl.storage_id = $1 AND sl.sector_filetype IN (2, 4, 8, 16)
)
SELECT s.miner_id AS sp_id, m.deadline, m.partition,
       count(*) AS sectors,
       count(*) FILTER (WHERE m.is_faulty) AS faulty,
       count(*) FILTER (WHERE m.sp_id IS NULL) AS not_in_meta,
       count(*) FILTER (WHERE NOT EXISTS (
           SELECT 1 FROM sector_location o
           WHERE o.miner_id = s.miner_id AND o.sector_num = s.sector_num
             AND o.sector_filetype IN (2, 8) AND o.storage_id <> $1)) AS no_other_sealed_copy
FROM s
LEFT JOIN sectors_meta m ON m.sp_id = s.miner_id AND m.sector_num = s.sector_num
WHERE m.sp_id IS NULL OR m.is_live
GROUP BY 1, 2, 3
ORDER BY 1, 2, 3`
