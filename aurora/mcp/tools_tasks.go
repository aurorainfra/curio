package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"golang.org/x/xerrors"

	"github.com/filecoin-project/curio/harmony/harmonydb"
)

type taskRow struct {
	ID           int64     `db:"id"`
	Name         string    `db:"name"`
	OwnerID      *int64    `db:"owner_id"`
	Owner        string    `db:"owner"`
	PostedTime   time.Time `db:"posted_time"`
	UpdateTime   time.Time `db:"update_time"`
	Retries      int64     `db:"retries"`
	InitiatedBy  *int64    `db:"initiated_by"`
	PreviousTask *int64    `db:"previous_task"`
	AgeS         int64     `db:"age_s"`
	SinceUpdateS int64     `db:"since_update_s"`
}

type taskCount struct {
	Name         string    `db:"name"`
	Queued       int64     `db:"queued"`
	Running      int64     `db:"running"`
	OldestPosted time.Time `db:"oldest_posted"`
}

type historyRow struct {
	ID         int64     `db:"id"`
	TaskID     int64     `db:"task_id"`
	Name       string    `db:"name"`
	Posted     time.Time `db:"posted"`
	WorkStart  time.Time `db:"work_start"`
	WorkEnd    time.Time `db:"work_end"`
	Result     bool      `db:"result"`
	Err        string    `db:"err"`
	Machine    string    `db:"completed_by_host_and_port"`
	DurationS  float64   `db:"duration_s"`
	QueueWaitS float64   `db:"wait_s"`
}

type stuckRow struct {
	ID               int64      `db:"id"`
	Name             string     `db:"name"`
	OwnerID          *int64     `db:"owner_id"`
	Owner            string     `db:"owner"`
	OwnerLastContact *time.Time `db:"owner_last_contact"`
	OwnerGone        bool       `db:"owner_gone"`
	PostedTime       time.Time  `db:"posted_time"`
	UpdateTime       time.Time  `db:"update_time"`
	Retries          int64      `db:"retries"`
	AgeS             int64      `db:"age_s"`
	SinceUpdateS     int64      `db:"since_update_s"`
	P99S             *float64   `db:"p99_s"`
	HistoryN         int64      `db:"history_n"`
	Reason           string     `db:"-"`
}

type timingRow struct {
	Name     string   `db:"name"`
	Machine  string   `db:"machine"`
	N        int64    `db:"n"`
	OK       int64    `db:"ok"`
	Failed   int64    `db:"failed"`
	P50S     *float64 `db:"p50_s"`
	P90S     *float64 `db:"p90_s"`
	P99S     *float64 `db:"p99_s"`
	MaxS     *float64 `db:"max_s"`
	P50WaitS *float64 `db:"p50_wait_s"`
	P90WaitS *float64 `db:"p90_wait_s"`
}

type tasksActiveArgs struct {
	Name        string `json:"name,omitempty" jsonschema:"task type name filter, e.g. WdPost, SDR, PoRep"`
	Machine     string `json:"machine,omitempty" jsonschema:"only tasks owned by this machine (ID, name or host:port)"`
	RunningOnly bool   `json:"running_only,omitempty" jsonschema:"only tasks claimed by a machine"`
	Limit       int    `json:"limit,omitempty" jsonschema:"max tasks listed, default 200, max 2000"`
}

type tasksStuckArgs struct {
	RunningMinutes int `json:"running_minutes,omitempty" jsonschema:"flag running tasks older than this many minutes (default 0: only use 2x the task type's p99 duration over the last 7 days)"`
	QueuedMinutes  int `json:"queued_minutes,omitempty" jsonschema:"flag unclaimed tasks queued longer than this, default 60"`
	Limit          int `json:"limit,omitempty" jsonschema:"max tasks, default 200, max 2000"`
}

type taskHistoryArgs struct {
	Name       string `json:"name,omitempty" jsonschema:"task type name filter"`
	Machine    string `json:"machine,omitempty" jsonschema:"only runs on this machine (ID, name or host:port)"`
	FailedOnly bool   `json:"failed_only,omitempty" jsonschema:"only failed runs"`
	TaskID     int64  `json:"task_id,omitempty" jsonschema:"only runs of this task ID"`
	Since      string `json:"since,omitempty" jsonschema:"window start: RFC3339 time or duration ago, default 24h"`
	Until      string `json:"until,omitempty" jsonschema:"window end, default now"`
	Limit      int    `json:"limit,omitempty" jsonschema:"max rows, default 100, max 2000"`
	ErrBytes   int    `json:"err_bytes,omitempty" jsonschema:"truncate error text to this many bytes, default 1000, max 20000"`
}

type taskDetailArgs struct {
	TaskID int64 `json:"task_id" jsonschema:"harmony task ID"`
}

type taskTimingArgs struct {
	Name      string `json:"name,omitempty" jsonschema:"task type name filter"`
	Since     string `json:"since,omitempty" jsonschema:"window start: RFC3339 time or duration ago, default 24h"`
	ByMachine bool   `json:"by_machine,omitempty" jsonschema:"break stats down per machine"`
}

type taskRef struct {
	Kind               string `db:"kind"`
	SpID               int64  `db:"sp_id"`
	ProvingPeriodStart int64  `db:"proving_period_start"`
	Deadline           int64  `db:"deadline_index"`
	Partition          int64  `db:"partition_index"`
}

type sectorRef struct {
	SpID         int64 `db:"sp_id"`
	SectorNumber int64 `db:"sector_number"`
}

func (s *Server) registerTaskTools() {
	addTool(s, "tasks_active",
		"Queued and running harmony tasks: per-type queued/running counts, and the oldest matching tasks with owner machine, age, time since claimed/updated and retries.",
		func(ctx context.Context, in tasksActiveArgs) (any, error) {
			owner := int64(-1)
			if in.Machine != "" {
				m, err := s.selectMachine(ctx, in.Machine)
				if err != nil {
					return nil, err
				}
				owner = m.ID
			}
			var out struct {
				Counts []taskCount
				Tasks  []taskRow
			}
			err := s.ro(ctx, func(tx *harmonydb.Tx) error {
				if err := tx.Select(&out.Counts, qTaskCounts); err != nil {
					return err
				}
				return tx.Select(&out.Tasks, qTasksActive, in.Name, owner, in.RunningOnly, clamp(in.Limit, 200, 2000))
			})
			return out, err
		})

	addTool(s, "tasks_stuck",
		"Tasks that look stuck: running on a machine that is gone or has not checked in for 2 minutes, running for longer than 2x the task type's p99 duration (last 7 days) or running_minutes, or queued unclaimed for longer than queued_minutes. Each row has a Reason.",
		func(ctx context.Context, in tasksStuckArgs) (any, error) {
			queued := time.Duration(clamp(in.QueuedMinutes, 60, 60*24*30)) * time.Minute
			running := float64(max(in.RunningMinutes, 0) * 60)
			now := time.Now()
			var rows []stuckRow
			err := s.ro(ctx, func(tx *harmonydb.Tx) error {
				return tx.Select(&rows, qTasksStuck, now.Add(-7*24*time.Hour), now.Add(-liveMachineWindow), running, now.Add(-queued), clamp(in.Limit, 200, 2000))
			})
			for i := range rows {
				r := &rows[i]
				switch {
				case r.OwnerID == nil:
					r.Reason = fmt.Sprintf("queued and unclaimed for %s", time.Duration(r.AgeS)*time.Second)
				case r.OwnerGone:
					r.Reason = "owner machine no longer exists"
				case r.OwnerLastContact != nil && now.Sub(*r.OwnerLastContact) > liveMachineWindow:
					r.Reason = fmt.Sprintf("owner machine last contact %s ago", now.Sub(*r.OwnerLastContact).Round(time.Second))
				case r.P99S != nil:
					r.Reason = fmt.Sprintf("running %s, type p99 is %s", time.Duration(r.SinceUpdateS)*time.Second, time.Duration(*r.P99S*float64(time.Second)).Round(time.Second))
				default:
					r.Reason = fmt.Sprintf("running %s (no successful runs of this type in 7 days)", time.Duration(r.SinceUpdateS)*time.Second)
				}
			}
			return rows, err
		})

	addTool(s, "task_history",
		"Completed task runs (harmony_task_history), newest first, with duration, queue wait, result, error text and the machine that ran it. Use failed_only to find failures.",
		func(ctx context.Context, in taskHistoryArgs) (any, error) {
			since, until, err := timeWindow(in.Since, in.Until, 24*time.Hour)
			if err != nil {
				return nil, err
			}
			hostPort := ""
			if in.Machine != "" {
				m, err := s.selectMachine(ctx, in.Machine)
				if err != nil {
					return nil, err
				}
				hostPort = m.HostPort
			}
			taskID := int64(-1)
			if in.TaskID > 0 {
				taskID = in.TaskID
			}
			var rows []historyRow
			err = s.ro(ctx, func(tx *harmonydb.Tx) error {
				return tx.Select(&rows, qTaskHistory, since, until, in.Name, hostPort, in.FailedOnly, taskID, clamp(in.Limit, 100, 2000), clamp(in.ErrBytes, 1000, 20000))
			})
			return rows, err
		})

	addTool(s, "task_detail",
		"One task: its harmony_task row if still queued/running, its run history, and the pipeline rows referencing it (sealing / snap sector, WindowPoSt partition). Pipeline references are usually cleared once a stage completes.",
		func(ctx context.Context, in taskDetailArgs) (any, error) {
			if in.TaskID <= 0 {
				return nil, xerrors.New("task_id is required")
			}
			var out struct {
				Task       *taskRow `json:",omitempty"`
				History    []historyRow
				SealSector []sectorRef `json:",omitempty"`
				SnapSector []sectorRef `json:",omitempty"`
				WdPost     []taskRef   `json:",omitempty"`
			}
			err := s.ro(ctx, func(tx *harmonydb.Tx) error {
				var t []taskRow
				if err := tx.Select(&t, qTaskByID, in.TaskID); err != nil {
					return err
				}
				if len(t) > 0 {
					out.Task = &t[0]
				}
				if err := tx.Select(&out.History, qTaskHistory, time.Unix(0, 0), time.Now().Add(time.Hour), "", "", false, in.TaskID, 50, 20000); err != nil {
					return err
				}
				if err := tx.Select(&out.SealSector, qTaskRefsSDR, in.TaskID); err != nil {
					return err
				}
				if err := tx.Select(&out.SnapSector, qTaskRefsSnap, in.TaskID); err != nil {
					return err
				}
				return tx.Select(&out.WdPost, qTaskRefsWdPost, in.TaskID)
			})
			return out, err
		})

	addTool(s, "task_timing_stats",
		"Task performance from run history: per task type (optionally per machine) run count, successes, failures, p50/p90/p99/max duration and p50/p90 queue wait (posted to start; includes retries) in seconds.",
		func(ctx context.Context, in taskTimingArgs) (any, error) {
			since, _, err := timeWindow(in.Since, "", 24*time.Hour)
			if err != nil {
				return nil, err
			}
			var rows []timingRow
			err = s.ro(ctx, func(tx *harmonydb.Tx) error {
				return tx.Select(&rows, qTaskTiming, since, in.ByMachine, in.Name)
			})
			return rows, err
		})
}

// rawJSON embeds a JSON document produced by the database.
type rawJSON string

func (r rawJSON) MarshalJSON() ([]byte, error) {
	if !json.Valid([]byte(r)) {
		return json.Marshal(string(r))
	}
	return []byte(r), nil
}
