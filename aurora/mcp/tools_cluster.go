package mcp

import (
	"context"
	"time"

	"github.com/filecoin-project/curio/harmony/harmonydb"
)

type machineRow struct {
	ID             int64      `db:"id"`
	HostPort       string     `db:"host_and_port"`
	Name           string     `db:"name"`
	LastContact    time.Time  `db:"last_contact"`
	SinceContactS  int64      `db:"since_contact_s"`
	CPU            int64      `db:"cpu"`
	RAM            int64      `db:"ram"`
	GPU            float64    `db:"gpu"`
	Unschedulable  bool       `db:"unschedulable"`
	RestartRequest *time.Time `db:"restart_request"`
	Tasks          string     `db:"tasks"`
	Layers         string     `db:"layers"`
	Miners         string     `db:"miners"`
	StartupTime    *time.Time `db:"startup_time"`
	Version        string     `db:"version"`
	RunningTasks   int64      `db:"running_tasks"`
	Live           bool       `db:"-"`
}

type noArgs struct{}

type alertsArgs struct {
	Since string `json:"since,omitempty" jsonschema:"alert history start: RFC3339 time or duration ago, default 24h"`
	Limit int    `json:"limit,omitempty" jsonschema:"max history entries, default 100, max 1000"`
}

type alertCondition struct {
	System      string    `db:"system"`
	Subsystem   string    `db:"subsystem"`
	Condition   string    `db:"condition"`
	Message     string    `db:"message"`
	CreatedAt   time.Time `db:"created_at"`
	LastSeenAt  time.Time `db:"last_seen_at"`
	RepeatCount int64     `db:"repeat_count"`
}

type alertHistoryRow struct {
	ID           int64     `db:"id"`
	AlertName    string    `db:"alert_name"`
	Message      string    `db:"message"`
	MachineName  string    `db:"machine_name"`
	CreatedAt    time.Time `db:"created_at"`
	Acknowledged bool      `db:"acknowledged"`
}

func (s *Server) registerClusterTools() {
	addTool(s, "cluster_machines",
		"List cluster machines: ID, name, host:port, last contact (Live = contacted in the last 2 minutes), version, enabled tasks and config layers, running task count.",
		func(ctx context.Context, _ noArgs) (any, error) {
			var rows []machineRow
			err := s.ro(ctx, func(tx *harmonydb.Tx) error {
				return tx.Select(&rows, qMachines)
			})
			for i := range rows {
				rows[i].Live = time.Since(rows[i].LastContact) < liveMachineWindow
			}
			return rows, err
		})

	addTool(s, "alerts",
		"Current alert conditions and recent alert history (WindowPoSt, task failures, missing sectors, balances, chain sync...).",
		func(ctx context.Context, in alertsArgs) (any, error) {
			since, _, err := timeWindow(in.Since, "", 24*time.Hour)
			if err != nil {
				return nil, err
			}
			limit := clamp(in.Limit, 100, 1000)
			var out struct {
				Ongoing []alertCondition
				History []alertHistoryRow
			}
			err = s.ro(ctx, func(tx *harmonydb.Tx) error {
				if err := tx.Select(&out.Ongoing, qAlertConditions, limit); err != nil {
					return err
				}
				return tx.Select(&out.History, qAlertHistory, since, limit)
			})
			return out, err
		})
}
