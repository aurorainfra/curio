package mcp

import (
	"context"
	"encoding/json"
	"time"

	"github.com/filecoin-project/curio/harmony/harmonydb"
)

const (
	auditRetention   = 30 * 24 * time.Hour
	auditQueue       = 1024
	maxAuditArgBytes = 16 << 10
	maxAuditErrBytes = 4 << 10
)

type auditEntry struct {
	agent       string
	tool        string
	args        json.RawMessage
	duration    time.Duration
	resultBytes int
	truncated   bool
	err         string
}

// auditLog records tool calls in aurora_mcp_calls. Writes are queued so a
// slow database does not delay tool results; when the queue is full entries
// are dropped with a warning.
type auditLog struct {
	db        *harmonydb.DB
	machineID func() *int64
	q         chan auditEntry
}

func newAuditLog(db *harmonydb.DB, machineID func() *int64) *auditLog {
	a := &auditLog{db: db, machineID: machineID, q: make(chan auditEntry, auditQueue)}
	if db != nil {
		go a.writer()
	}
	return a
}

func (a *auditLog) record(agent, tool string, args json.RawMessage, d time.Duration, resultBytes int, truncated bool, err error) {
	e := auditEntry{agent: agent, tool: tool, args: args, duration: d, resultBytes: resultBytes, truncated: truncated}
	if len(e.args) == 0 || len(e.args) > maxAuditArgBytes || !json.Valid(e.args) {
		e.args = json.RawMessage(`{}`)
	}
	if err != nil {
		e.err = err.Error()
		if len(e.err) > maxAuditErrBytes {
			e.err = e.err[:maxAuditErrBytes]
		}
	}
	log.Infow("mcp tool call", "agent", agent, "tool", tool, "duration", d.Round(time.Millisecond), "bytes", resultBytes, "error", e.err)
	if a.db == nil {
		return
	}
	select {
	case a.q <- e:
	default:
		log.Warnw("mcp audit queue full, dropping entry", "agent", agent, "tool", tool)
	}
}

func (a *auditLog) writer() {
	for e := range a.q {
		var errText *string
		if e.err != "" {
			errText = &e.err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, err := a.db.Exec(ctx, `INSERT INTO aurora_mcp_calls (agent, machine_id, tool, args, duration_ms, result_bytes, truncated, error)
			VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7, $8)`,
			e.agent, a.machineID(), e.tool, string(e.args), e.duration.Milliseconds(), e.resultBytes, e.truncated, errText)
		cancel()
		if err != nil {
			log.Warnw("recording mcp tool call", "error", err)
		}
	}
}

func (a *auditLog) pruneLoop(ctx context.Context) {
	if a.db == nil {
		return
	}
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		n, err := a.db.Exec(ctx, `DELETE FROM aurora_mcp_calls WHERE ts < $1`, time.Now().Add(-auditRetention))
		if err != nil {
			log.Warnw("pruning mcp tool call log", "error", err)
		} else if n > 0 {
			log.Infow("pruned mcp tool call log", "rows", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
