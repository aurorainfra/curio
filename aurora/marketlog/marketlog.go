// Package marketlog records storage deal acceptance decisions in
// aurora_market_decisions, so "why is the market rejecting deals" can be
// answered from history instead of from whichever node's log handled each
// proposal. A decision carries the backpressure verdicts the deal saw and, with
// CIDGravity, the CIDGravity decision and matching rule.
package marketlog

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"time"

	logging "github.com/ipfs/go-log/v2"

	"github.com/filecoin-project/curio/harmony/harmonydb"
)

var log = logging.Logger("aurora/marketlog")

const (
	retention = 7 * 24 * time.Hour
	queueLen  = 4096
	maxText   = 2000
)

// CIDGravity is the CIDGravity check response for a deal.
type CIDGravity struct {
	Decision                string `json:",omitempty"`
	InternalDecision        string `json:",omitempty"`
	ExternalDecision        string `json:",omitempty"`
	InternalMessage         string `json:",omitempty"`
	ExternalMessage         string `json:",omitempty"`
	CustomMessage           string `json:",omitempty"`
	MatchingRule            int    `json:",omitempty"`
	MatchingAcceptanceLogic string `json:",omitempty"`
	MatchingPricing         string `json:",omitempty"`
	// UnderBackPressure is what was reported to CIDGravity.
	UnderBackPressure *bool `json:",omitempty"`
}

// Decision is one deal proposal decision being recorded.
type Decision struct {
	w      *writer
	start  time.Time
	market string

	mu         sync.Mutex
	dealUUID   string
	spID       *int64
	client     string
	pieceCID   string
	pieceSize  int64
	verified   bool
	offline    bool
	bpMK12     *bool
	bpSector   *bool
	cidgravity *CIDGravity
}

// Outcome is implemented by deal responses.
type Outcome interface {
	Outcome() (accepted bool, reason string)
}

type ctxKey struct{}

// Start begins recording a decision. Methods on a nil *Decision are no-ops.
func Start(ctx context.Context, db *harmonydb.DB, market, dealUUID string, spID int64, client, pieceCID string, pieceSize int64, verified, offline bool) (context.Context, *Decision) {
	if db == nil {
		return ctx, nil
	}
	d := &Decision{
		w: getWriter(db), start: time.Now(), market: market,
		dealUUID: dealUUID, spID: &spID, client: client, pieceCID: pieceCID, pieceSize: pieceSize,
		verified: verified, offline: offline,
	}
	return context.WithValue(ctx, ctxKey{}, d), d
}

func from(ctx context.Context) *Decision {
	d, _ := ctx.Value(ctxKey{}).(*Decision)
	return d
}

// NoteBackpressure records a backpressure verdict ("mk12" or "sector") seen
// while deciding.
func NoteBackpressure(ctx context.Context, kind string, busy bool) {
	d := from(ctx)
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	switch kind {
	case "mk12", "mk20":
		d.bpMK12 = &busy
	case "sector":
		d.bpSector = &busy
	}
}

// NoteCIDGravity records the CIDGravity response.
func NoteCIDGravity(ctx context.Context, cg CIDGravity) {
	d := from(ctx)
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.bpMK12 != nil || d.bpSector != nil {
		busy := (d.bpMK12 != nil && *d.bpMK12) || (d.bpSector != nil && *d.bpSector)
		cg.UnderBackPressure = &busy
	}
	d.cidgravity = &cg
}

// Finish records the outcome.
func (d *Decision) Finish(o Outcome, err error) {
	if d == nil {
		return
	}
	var accepted bool
	var reason string
	if o != nil {
		accepted, reason = o.Outcome()
	}
	d.mu.Lock()
	r := row{
		market: d.market, dealUUID: d.dealUUID, spID: d.spID, client: d.client, pieceCID: d.pieceCID,
		pieceSize: d.pieceSize, verified: d.verified, offline: d.offline, accepted: accepted && err == nil,
		reason: trunc(reason), bpMK12: d.bpMK12, bpSector: d.bpSector,
		durationMS: time.Since(d.start).Milliseconds(),
	}
	if err != nil {
		e := trunc(err.Error())
		r.err = &e
	}
	if d.cidgravity != nil {
		b, _ := json.Marshal(d.cidgravity)
		s := string(b)
		r.cidgravity = &s
	}
	d.mu.Unlock()
	d.w.enqueue(r)
}

func trunc(s string) string {
	if len(s) > maxText {
		return s[:maxText]
	}
	return s
}

// ---- writer ----

type row struct {
	market, dealUUID, client, pieceCID string
	spID                               *int64
	pieceSize                          int64
	verified, offline, accepted        bool
	reason                             string
	err, cidgravity                    *string
	bpMK12, bpSector                   *bool
	durationMS                         int64
}

type writer struct {
	db      *harmonydb.DB
	q       chan row
	machine string
}

var (
	wOnce sync.Once
	w     *writer
)

func getWriter(db *harmonydb.DB) *writer {
	wOnce.Do(func() {
		host, _ := os.Hostname()
		w = &writer{db: db, q: make(chan row, queueLen), machine: host}
		go w.run()
		go w.prune()
	})
	return w
}

func (w *writer) enqueue(r row) {
	select {
	case w.q <- r:
	default:
		log.Warnw("market decision queue full, dropping", "deal", r.dealUUID)
	}
}

func (w *writer) run() {
	for r := range w.q {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, err := w.db.Exec(ctx, `INSERT INTO aurora_market_decisions
			(machine, market, deal_uuid, sp_id, client, piece_cid, piece_size, verified, offline, accepted, reason, error, bp_mk12, bp_sector, cidgravity, duration_ms)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15::jsonb, $16)`,
			w.machine, r.market, r.dealUUID, r.spID, r.client, r.pieceCID, r.pieceSize, r.verified, r.offline, r.accepted,
			r.reason, r.err, r.bpMK12, r.bpSector, r.cidgravity, r.durationMS)
		cancel()
		if err != nil {
			log.Warnw("recording market decision", "deal", r.dealUUID, "error", err)
		}
	}
}

func (w *writer) prune() {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for ; ; <-t.C {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		if _, err := w.db.Exec(ctx, `DELETE FROM aurora_market_decisions WHERE ts < $1`, time.Now().Add(-retention)); err != nil {
			log.Warnw("pruning market decisions", "error", err)
		}
		cancel()
	}
}
