package mcp

import (
	"context"
	"time"

	"golang.org/x/xerrors"

	"github.com/filecoin-project/curio/aurora/nodediag"
	"github.com/filecoin-project/curio/harmony/harmonydb"
	"github.com/filecoin-project/curio/market/backpressure"
)

type marketBackpressureArgs struct {
	Machine string `json:"machine" jsonschema:"market node receiving deals (ID, name or host:port), or \"all\" to compare nodes; each node uses its own layered Ingest limits"`
}

type marketDecisionsArgs struct {
	Since   string `json:"since,omitempty" jsonschema:"window start: RFC3339 or duration ago, default 24h (decisions are kept 7 days)"`
	Until   string `json:"until,omitempty" jsonschema:"window end, default now"`
	SP      string `json:"sp,omitempty" jsonschema:"storage provider filter"`
	Client  string `json:"client,omitempty" jsonschema:"client address filter (as in the proposal, e.g. f1...)"`
	Outcome string `json:"outcome,omitempty" jsonschema:"all (default), accepted or rejected"`
	Reason  string `json:"reason,omitempty" jsonschema:"case-insensitive substring of the rejection reason or CIDGravity response"`
	Limit   int    `json:"limit,omitempty" jsonschema:"decisions listed, default 50, max 1000"`
}

type marketPipelineArgs struct {
	Since  string `json:"since,omitempty" jsonschema:"window for failed deals: RFC3339 or duration ago, default 24h"`
	Oldest int    `json:"oldest,omitempty" jsonschema:"oldest active MK12 pipelines and open deal sectors listed, default 20, max 500"`
}

type decisionRow struct {
	ID         int64     `db:"id"`
	TS         time.Time `db:"ts"`
	Machine    string    `db:"machine"`
	Market     string    `db:"market"`
	DealUUID   string    `db:"deal_uuid"`
	SpID       *int64    `db:"sp_id"`
	Client     string    `db:"client"`
	PieceCID   string    `db:"piece_cid"`
	PieceSize  int64     `db:"piece_size"`
	Verified   bool      `db:"verified"`
	Offline    bool      `db:"offline"`
	Accepted   bool      `db:"accepted"`
	Reason     string    `db:"reason"`
	Error      string    `db:"error"`
	BPMK12     *bool     `db:"bp_mk12"`
	BPSector   *bool     `db:"bp_sector"`
	CIDGravity rawJSON   `db:"cidgravity" json:",omitempty"`
	DurationMS int64     `db:"duration_ms"`
}

func (s *Server) registerMarketTools() {
	addTool(s, "market_backpressure",
		"Why the market is (not) under backpressure, as computed on a market node with its own config: every counter the checks compare (queued SDR/trees/PoRep or snap encode/prove, open deal sectors, running deal pipelines, queued downloads/CommP) with its Ingest limit and whether it trips; the fresh verdict of the upstream checks; and the cached verdicts deals currently see with their age and expiry (the sector verdict is cached 10 minutes, mk12/mk20 2 minutes). With CIDGravity, MK12Busy is what is reported as UnderBackPressure.",
		func(ctx context.Context, in marketBackpressureArgs) (any, error) {
			ms, err := s.selectMachines(ctx, in.Machine, true)
			if err != nil {
				return nil, err
			}
			return callNodes[backpressure.Report](ctx, s, ms, nodediag.OpBackpressure, struct{}{}), nil
		})

	addTool(s, "market_decisions",
		"History of MK12 deal proposal decisions (kept 7 days): accepted/rejected with reason, the backpressure verdicts the deal saw, and with CIDGravity its decision, messages and matching rule. Also counts by reason and per hour (accepted/rejected/under backpressure), to see when and why rejections started.",
		func(ctx context.Context, in marketDecisionsArgs) (any, error) {
			since, until, err := timeWindow(in.Since, in.Until, 24*time.Hour)
			if err != nil {
				return nil, err
			}
			sp := int64(-1)
			if in.SP != "" {
				id, _, err := s.spID(ctx, in.SP)
				if err != nil {
					return nil, err
				}
				sp = int64(id)
			}
			outcome := in.Outcome
			switch outcome {
			case "":
				outcome = "all"
			case "all", "accepted", "rejected":
			default:
				return nil, xerrors.New("outcome must be all, accepted or rejected")
			}
			var out struct {
				Reasons []struct {
					Accepted bool      `db:"accepted"`
					Reason   string    `db:"reason"`
					N        int64     `db:"n"`
					FirstTS  time.Time `db:"first_ts"`
					LastTS   time.Time `db:"last_ts"`
					BPMK12   int64     `db:"bp_mk12"`
					BPSector int64     `db:"bp_sector"`
				}
				Hourly []struct {
					Hour     time.Time `db:"hour"`
					Accepted int64     `db:"accepted"`
					Rejected int64     `db:"rejected"`
					UnderBP  int64     `db:"under_backpressure"`
				}
				Decisions []decisionRow
			}
			err = s.ro(ctx, func(tx *harmonydb.Tx) error {
				if err := tx.Select(&out.Reasons, qMarketDecisionReasons, since, until, sp); err != nil {
					return err
				}
				if err := tx.Select(&out.Hourly, qMarketDecisionHourly, since, until, sp); err != nil {
					return err
				}
				return tx.Select(&out.Decisions, qMarketDecisions, since, until, sp, in.Client, outcome, in.Reason, clamp(in.Limit, 50, 1000))
			})
			return out, err
		})

	addTool(s, "market_pipeline",
		"State of deal ingestion: active MK12 and MK20 deal pipelines by stage with the oldest entry, piece downloads (parked pieces: no task / queued / running), open deal sectors waiting to be filled (the MaxQueueDealSector input), the oldest active MK12 pipelines, and MK12 deals that failed in the window grouped by error.",
		func(ctx context.Context, in marketPipelineArgs) (any, error) {
			since, _, err := timeWindow(in.Since, "", 24*time.Hour)
			if err != nil {
				return nil, err
			}
			n := clamp(in.Oldest, 20, 500)
			var out struct {
				MK12 []struct {
					Active           int64      `db:"active"`
					NotStarted       int64      `db:"not_started"`
					BeforeCommP      int64      `db:"before_commp"`
					BeforePublish    int64      `db:"before_publish"`
					BeforeFindDeal   int64      `db:"before_find_deal"`
					WaitingForSector int64      `db:"waiting_for_sector"`
					Sealing          int64      `db:"sealing"`
					Indexing         int64      `db:"indexing"`
					Offline          int64      `db:"offline"`
					Oldest           *time.Time `db:"oldest"`
				}
				MK20 []struct {
					Active            int64      `db:"active"`
					Downloading       int64      `db:"downloading"`
					BeforeCommP       int64      `db:"before_commp"`
					BeforeAggregation int64      `db:"before_aggregation"`
					WaitingForSector  int64      `db:"waiting_for_sector"`
					Sealing           int64      `db:"sealing"`
					Indexing          int64      `db:"indexing"`
					Oldest            *time.Time `db:"oldest"`
				}
				Downloads []struct {
					Incomplete int64      `db:"incomplete"`
					NoTask     int64      `db:"no_task"`
					Queued     int64      `db:"queued"`
					Running    int64      `db:"running"`
					Oldest     *time.Time `db:"oldest"`
				}
				OpenDealSectors []struct {
					SpID         int64     `db:"sp_id"`
					SectorNumber int64     `db:"sector_number"`
					IsSnap       bool      `db:"is_snap"`
					Pieces       int64     `db:"pieces"`
					PieceBytes   int64     `db:"piece_bytes"`
					OldestPiece  time.Time `db:"oldest_piece"`
				}
				OldestMK12 []struct {
					UUID          string    `db:"uuid"`
					SpID          int64     `db:"sp_id"`
					CreatedAt     time.Time `db:"created_at"`
					Offline       bool      `db:"offline"`
					Started       bool      `db:"started"`
					AfterCommP    bool      `db:"after_commp"`
					AfterPSD      bool      `db:"after_psd"`
					AfterFindDeal bool      `db:"after_find_deal"`
					Sector        *int64    `db:"sector"`
					Sealed        bool      `db:"sealed"`
					Indexed       bool      `db:"indexed"`
					CommPTask     *int64    `db:"commp_task_id"`
					PSDTask       *int64    `db:"psd_task_id"`
					FindDealTask  *int64    `db:"find_deal_task_id"`
					IndexingTask  *int64    `db:"indexing_task_id"`
				}
				FailedMK12 []struct {
					Error      string    `db:"error"`
					N          int64     `db:"n"`
					Last       time.Time `db:"last"`
					SampleUUID string    `db:"sample_uuid"`
				}
			}
			err = s.ro(ctx, func(tx *harmonydb.Tx) error {
				for _, q := range []func() error{
					func() error { return tx.Select(&out.MK12, qMK12PipelineStages) },
					func() error { return tx.Select(&out.MK20, qMK20PipelineStages) },
					func() error { return tx.Select(&out.Downloads, qParkedPieces) },
					func() error { return tx.Select(&out.OpenDealSectors, qOpenDealSectors, n) },
					func() error { return tx.Select(&out.OldestMK12, qMK12PipelineOldest, n) },
					func() error { return tx.Select(&out.FailedMK12, qFailedMK12Deals, since) },
				} {
					if err := q(); err != nil {
						return err
					}
				}
				return nil
			})
			return out, err
		})
}
