package mcp

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-bitfield"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/curio/harmony/harmonydb"

	lapi "github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/chain/actors/builtin/miner"
	"github.com/filecoin-project/lotus/chain/types"
)

const (
	maxSectorList = 2000
	maxPathGroup  = 50
)

type wdpostProofRow struct {
	SpID               int64  `db:"sp_id"`
	ProvingPeriodStart int64  `db:"proving_period_start"`
	Deadline           int64  `db:"deadline"`
	Partition          int64  `db:"partition"`
	SubmitAtEpoch      int64  `db:"submit_at_epoch"`
	SubmitByEpoch      int64  `db:"submit_by_epoch"`
	ProofParams        []byte `db:"proof_params"`
	SubmitTaskID       *int64 `db:"submit_task_id"`
	MessageCID         string `db:"message_cid"`
	ExecutedEpoch      *int64 `db:"executed_tsk_epoch"`
	ExitCode           *int64 `db:"executed_rcpt_exitcode"`
	WaitTracked        bool   `db:"wait_tracked"`
}

type wdpostTaskRow struct {
	TaskID             int64      `db:"task_id"`
	SpID               int64      `db:"sp_id"`
	ProvingPeriodStart int64      `db:"proving_period_start"`
	Deadline           int64      `db:"deadline_index"`
	Partition          int64      `db:"partition_index"`
	TaskLive           bool       `db:"task_live"`
	OwnerID            *int64     `db:"owner_id"`
	LastResult         *bool      `db:"last_result"`
	LastErr            string     `db:"last_err"`
	LastWorkStart      *time.Time `db:"last_work_start"`
	LastWorkEnd        *time.Time `db:"last_work_end"`
	LastMachine        string     `db:"last_machine"`
}

type wdpostDeadlinesArgs struct {
	SP        string `json:"sp" jsonschema:"storage provider, e.g. f01234"`
	Deadlines int    `json:"deadlines,omitempty" jsonschema:"number of closed deadlines to show before the open one, default 6, max 48 (one proving period)"`
}

type wdpostPartitionArgs struct {
	SP                 string `json:"sp" jsonschema:"storage provider, e.g. f01234"`
	Deadline           int64  `json:"deadline" jsonschema:"deadline index 0-47"`
	Partition          int64  `json:"partition" jsonschema:"partition index within the deadline"`
	ProvingPeriodStart int64  `json:"proving_period_start,omitempty" jsonschema:"proving period start epoch; default the latest one with local proof or task records"`
}

type localProof struct {
	MessageCID    string `json:",omitempty"`
	SubmitAtEpoch int64
	SubmitByEpoch int64
	SubmitTaskID  *int64 `json:",omitempty"`
	WaitTracked   bool
	ExecutedEpoch *int64 `json:",omitempty"`
	ExitCode      *int64 `json:",omitempty"`
	Skipped       uint64
	DecodeError   string `json:",omitempty"`
}

type partitionTask struct {
	TaskID        int64
	Live          bool
	OwnerID       *int64     `json:",omitempty"`
	LastResult    *bool      `json:",omitempty"`
	LastErr       string     `json:",omitempty"`
	LastWorkStart *time.Time `json:",omitempty"`
	LastWorkEnd   *time.Time `json:",omitempty"`
	LastMachine   string     `json:",omitempty"`
}

type partitionStatus struct {
	Index         uint64
	Live          uint64
	Active        uint64
	Faulty        uint64
	Recovering    uint64
	ProvenOnChain *bool          `json:",omitempty"`
	Proof         *localProof    `json:",omitempty"`
	Task          *partitionTask `json:",omitempty"`
	Status        string
}

type deadlineStatus struct {
	Index       uint64
	PeriodStart int64
	Open        int64
	Close       int64
	OpenTime    time.Time
	CloseTime   time.Time
	Current     bool
	Partitions  []partitionStatus
}

func decodeSkipped(params []byte, partition int64) (uint64, []uint64, error) {
	var p miner.SubmitWindowedPoStParams
	if err := p.UnmarshalCBOR(bytes.NewReader(params)); err != nil {
		return 0, nil, err
	}
	var n uint64
	var list []uint64
	for _, pp := range p.Partitions {
		if int64(pp.Index) != partition {
			continue
		}
		c, err := pp.Skipped.Count()
		if err != nil {
			return 0, nil, err
		}
		n += c
		err = pp.Skipped.ForEach(func(s uint64) error {
			if len(list) < maxSectorList {
				list = append(list, s)
			}
			return nil
		})
		if err != nil {
			return 0, nil, err
		}
	}
	return n, list, nil
}

func bfCount(bf bitfield.BitField) uint64 {
	n, err := bf.Count()
	if err != nil {
		return 0
	}
	return n
}

func bfList(bf bitfield.BitField, limit int) []uint64 {
	var out []uint64
	_ = bf.ForEach(func(s uint64) error {
		if len(out) >= limit {
			return bitfield.ErrBitFieldTooMany
		}
		out = append(out, s)
		return nil
	})
	return out
}

func classify(ps partitionStatus, closed bool) string {
	switch {
	case ps.Live == 0:
		return "empty partition"
	case ps.Proof == nil && !closed:
		return "pending: no local proof yet"
	case ps.Proof == nil:
		return "MISSED: no local proof recorded"
	case ps.Proof.MessageCID == "":
		if closed {
			return "MISSED: proof computed but no message sent"
		}
		return "pending: proof computed, not sent yet"
	case !ps.Proof.WaitTracked:
		return "message not tracked in message_waits"
	case ps.Proof.ExecutedEpoch == nil:
		if closed {
			return "MISSED: message did not land on chain"
		}
		return "pending: message sent, not landed yet"
	case ps.Proof.ExitCode == nil:
		return "landed, no receipt exit code recorded"
	case *ps.Proof.ExitCode != 0:
		return fmt.Sprintf("FAILED on chain: exit code %d", *ps.Proof.ExitCode)
	case ps.Proof.Skipped > 0:
		return fmt.Sprintf("proven with %d skipped sectors", ps.Proof.Skipped)
	default:
		return "proven"
	}
}

func (s *Server) registerWdPostTools() {
	addTool(s, "wdpost_deadlines",
		"WindowPoSt overview for one SP: the open deadline and the last N closed deadlines, per non-empty partition the chain sector counts (live/active/faulty/recovering), the local proof (message, landed epoch, exit code, skipped sector count), the WdPost task's last result/error and a Status. Also recent failed WdPost/WdPostSubmit/WdPostRecover runs.",
		func(ctx context.Context, in wdpostDeadlinesArgs) (any, error) {
			if s.d.Chain == nil {
				return nil, xerrors.New("no chain connection on this node")
			}
			spID, maddr, err := s.spID(ctx, in.SP)
			if err != nil {
				return nil, err
			}
			n := clamp(in.Deadlines, 6, 48)

			head, err := s.d.Chain.ChainHead(ctx)
			if err != nil {
				return nil, err
			}
			headTime := time.Unix(int64(head.MinTimestamp()), 0)
			di, err := s.d.Chain.StateMinerProvingDeadline(ctx, maddr, head.Key())
			if err != nil {
				return nil, xerrors.Errorf("getting proving deadline: %w", err)
			}
			if di.WPoStPeriodDeadlines == 0 {
				return nil, xerrors.New("invalid deadline info")
			}

			var dls []deadlineStatus
			idx, periodStart, open := di.Index, int64(di.PeriodStart), int64(di.Open)
			for i := 0; i <= n; i++ {
				cl := open + int64(di.WPoStChallengeWindow)
				dls = append(dls, deadlineStatus{
					Index: idx, PeriodStart: periodStart, Open: open, Close: cl, Current: i == 0,
					OpenTime:  epochTime(int64(head.Height()), headTime, open),
					CloseTime: epochTime(int64(head.Height()), headTime, cl),
				})
				open -= int64(di.WPoStChallengeWindow)
				if idx == 0 {
					idx = di.WPoStPeriodDeadlines - 1
					periodStart -= int64(di.WPoStProvingPeriod)
				} else {
					idx--
				}
			}

			var proven bitfield.BitField
			haveProven := false
			if chainDls, err := s.d.Chain.StateMinerDeadlines(ctx, maddr, head.Key()); err == nil && int(di.Index) < len(chainDls) {
				proven, haveProven = chainDls[di.Index].PostSubmissions, true
			}

			periods := map[int64]bool{}
			var periodList, dlList []int64
			for _, d := range dls {
				if !periods[d.PeriodStart] {
					periods[d.PeriodStart] = true
					periodList = append(periodList, d.PeriodStart)
				}
				dlList = append(dlList, int64(d.Index))
			}

			var proofs []wdpostProofRow
			var tasks []wdpostTaskRow
			var out struct {
				SP               string
				HeadEpoch        int64
				ProvingDeadline  uint64
				Deadlines        []deadlineStatus
				RecentFailedRuns []historyRow
			}
			err = s.ro(ctx, func(tx *harmonydb.Tx) error {
				if err := tx.Select(&proofs, qWdPostProofs, spID, periodList, dlList); err != nil {
					return err
				}
				if err := tx.Select(&tasks, qWdPostTasks, spID, periodList, dlList); err != nil {
					return err
				}
				return tx.Select(&out.RecentFailedRuns, qWdPostRecentFailures, time.Now().Add(-24*time.Hour), 20)
			})
			if err != nil {
				return nil, err
			}
			type pkey struct{ ps, dl, part int64 }
			proofBy := map[pkey]wdpostProofRow{}
			for _, p := range proofs {
				proofBy[pkey{p.ProvingPeriodStart, p.Deadline, p.Partition}] = p
			}
			taskBy := map[pkey]wdpostTaskRow{}
			for _, t := range tasks {
				taskBy[pkey{t.ProvingPeriodStart, t.Deadline, t.Partition}] = t
			}

			partsByIdx := map[uint64][]lapi.Partition{}
			for i := range dls {
				d := &dls[i]
				parts, ok := partsByIdx[d.Index]
				if !ok {
					parts, err = s.d.Chain.StateMinerPartitions(ctx, maddr, d.Index, head.Key())
					if err != nil {
						return nil, xerrors.Errorf("getting partitions of deadline %d: %w", d.Index, err)
					}
					partsByIdx[d.Index] = parts
				}
				closed := !d.Current
				for pi, p := range parts {
					ps := partitionStatus{
						Index:      uint64(pi),
						Live:       bfCount(p.LiveSectors),
						Active:     bfCount(p.ActiveSectors),
						Faulty:     bfCount(p.FaultySectors),
						Recovering: bfCount(p.RecoveringSectors),
					}
					if d.Current && haveProven {
						v, _ := proven.IsSet(uint64(pi))
						ps.ProvenOnChain = &v
					}
					k := pkey{d.PeriodStart, int64(d.Index), int64(pi)}
					if pr, ok := proofBy[k]; ok {
						lp := &localProof{MessageCID: pr.MessageCID, SubmitAtEpoch: pr.SubmitAtEpoch, SubmitByEpoch: pr.SubmitByEpoch,
							SubmitTaskID: pr.SubmitTaskID, WaitTracked: pr.WaitTracked, ExecutedEpoch: pr.ExecutedEpoch, ExitCode: pr.ExitCode}
						if len(pr.ProofParams) > 0 {
							if n, _, err := decodeSkipped(pr.ProofParams, int64(pi)); err != nil {
								lp.DecodeError = err.Error()
							} else {
								lp.Skipped = n
							}
						}
						ps.Proof = lp
					}
					if t, ok := taskBy[k]; ok {
						ps.Task = &partitionTask{TaskID: t.TaskID, Live: t.TaskLive, OwnerID: t.OwnerID, LastResult: t.LastResult,
							LastErr: t.LastErr, LastWorkStart: t.LastWorkStart, LastWorkEnd: t.LastWorkEnd, LastMachine: t.LastMachine}
					}
					ps.Status = classify(ps, closed)
					if ps.ProvenOnChain != nil && *ps.ProvenOnChain && ps.Live > 0 {
						ps.Status = "proven (on chain, current deadline)"
					}
					d.Partitions = append(d.Partitions, ps)
				}
			}

			out.SP = maddr.String()
			out.HeadEpoch = int64(head.Height())
			out.ProvingDeadline = di.Index
			out.Deadlines = dls
			return out, nil
		})

	addTool(s, "wdpost_partition",
		"Deep dive into one WindowPoSt partition: decoded skipped sectors from the local proof, current chain faulty/recovering sectors, the skipped+faulty sectors grouped by the storage path holding their sealed/cache files (or missing from the sector index), the run history of the WdPost/WdPostSubmit/WdPostRecover tasks with errors, and a log hint (machine and time window) for log_grep.",
		func(ctx context.Context, in wdpostPartitionArgs) (any, error) {
			spID, maddr, err := s.spID(ctx, in.SP)
			if err != nil {
				return nil, err
			}
			if in.Deadline < 0 || in.Deadline >= 48 || in.Partition < 0 {
				return nil, xerrors.New("invalid deadline or partition")
			}
			return s.wdpostPartition(ctx, spID, maddr, in)
		})
}

type pathGroup struct {
	StorageID string
	FileTypes []string
	Sectors   int
	Sample    []int64
	// Heartbeat age of the storage path; nil when the path is not in
	// storage_path.
	HeartbeatAgeS *int64 `json:",omitempty"`
	URLs          string `json:",omitempty"`
}

type logHint struct {
	Machine string
	Since   time.Time
	Until   time.Time
	Pattern string
	Note    string
}

func (s *Server) wdpostPartition(ctx context.Context, spID uint64, maddr address.Address, in wdpostPartitionArgs) (any, error) {
	var out struct {
		SP                 string
		Deadline           int64
		Partition          int64
		ProvingPeriodStart int64
		Proof              *localProof `json:",omitempty"`
		SkippedSectors     []uint64    `json:",omitempty"`
		ChainFaulty        []uint64    `json:",omitempty"`
		ChainFaultyCount   uint64
		ChainRecovering    []uint64 `json:",omitempty"`
		ChainLiveCount     uint64
		ChainError         string `json:",omitempty"`
		// SectorsByPath groups skipped and faulty sectors by the storage
		// paths holding their sealed/cache files.
		SectorsByPath []pathGroup
		// NotInIndex are skipped/faulty sectors with no sealed/cache file in
		// the sector index.
		NotInIndex []uint64 `json:",omitempty"`
		Tasks      []struct {
			Kind   string `db:"kind"`
			TaskID int64  `db:"task_id"`
		}
		History  []historyRow
		LogHints []logHint `json:",omitempty"`
	}
	out.SP = maddr.String()
	out.Deadline, out.Partition = in.Deadline, in.Partition

	period := in.ProvingPeriodStart
	err := s.ro(ctx, func(tx *harmonydb.Tx) error {
		if period == 0 {
			var p *int64
			if err := tx.QueryRow(qWdPostLatestPeriod, spID, in.Deadline, in.Partition).Scan(&p); err != nil {
				return err
			}
			if p != nil {
				period = *p
			}
		}
		var proofs []wdpostProofRow
		if err := tx.Select(&proofs, qWdPostProofs, spID, []int64{period}, []int64{in.Deadline}); err != nil {
			return err
		}
		for _, pr := range proofs {
			if pr.Partition != in.Partition {
				continue
			}
			lp := &localProof{MessageCID: pr.MessageCID, SubmitAtEpoch: pr.SubmitAtEpoch, SubmitByEpoch: pr.SubmitByEpoch,
				SubmitTaskID: pr.SubmitTaskID, WaitTracked: pr.WaitTracked, ExecutedEpoch: pr.ExecutedEpoch, ExitCode: pr.ExitCode}
			if len(pr.ProofParams) > 0 {
				n, list, err := decodeSkipped(pr.ProofParams, in.Partition)
				if err != nil {
					lp.DecodeError = err.Error()
				}
				lp.Skipped, out.SkippedSectors = n, list
			}
			out.Proof = lp
		}
		if err := tx.Select(&out.Tasks, qWdPostPartitionTaskIDs, spID, period, in.Deadline, in.Partition); err != nil {
			return err
		}
		var ids []int64
		for _, t := range out.Tasks {
			ids = append(ids, t.TaskID)
		}
		if len(ids) > 0 {
			if err := tx.Select(&out.History, qWdPostTaskHistory, ids); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out.ProvingPeriodStart = period

	if s.d.Chain != nil {
		parts, err := s.d.Chain.StateMinerPartitions(ctx, maddr, uint64(in.Deadline), types.EmptyTSK)
		switch {
		case err != nil:
			out.ChainError = err.Error()
		case int(in.Partition) >= len(parts):
			out.ChainError = fmt.Sprintf("deadline %d has %d partitions on chain", in.Deadline, len(parts))
		default:
			p := parts[in.Partition]
			out.ChainFaulty = bfList(p.FaultySectors, maxSectorList)
			out.ChainFaultyCount = bfCount(p.FaultySectors)
			out.ChainRecovering = bfList(p.RecoveringSectors, maxSectorList)
			out.ChainLiveCount = bfCount(p.LiveSectors)
		}
	}

	// group skipped + faulty sectors by storage path
	sectorSet := map[int64]bool{}
	for _, l := range [][]uint64{out.SkippedSectors, out.ChainFaulty} {
		for _, sn := range l {
			sectorSet[int64(sn)] = true
		}
	}
	if len(sectorSet) > 0 {
		var sectors []int64
		for sn := range sectorSet {
			sectors = append(sectors, sn)
		}
		var locs []struct {
			StorageID string `db:"storage_id"`
			SectorNum int64  `db:"sector_num"`
			FileType  int    `db:"sector_filetype"`
		}
		var paths []storagePathRow
		err := s.ro(ctx, func(tx *harmonydb.Tx) error {
			if err := tx.Select(&locs, qSectorsByPath, spID, sectors); err != nil {
				return err
			}
			return tx.Select(&paths, qStoragePaths)
		})
		if err != nil {
			return nil, err
		}
		pathInfo := map[string]storagePathRow{}
		for _, p := range paths {
			pathInfo[p.StorageID] = p
		}
		groups := map[string]*pathGroup{}
		groupSectors := map[string]map[int64]bool{}
		groupTypes := map[string]map[string]bool{}
		located := map[int64]bool{}
		for _, l := range locs {
			located[l.SectorNum] = true
			g, ok := groups[l.StorageID]
			if !ok {
				g = &pathGroup{StorageID: l.StorageID}
				if p, ok := pathInfo[l.StorageID]; ok {
					g.HeartbeatAgeS, g.URLs = p.HeartbeatAgeS, p.URLs
				}
				groups[l.StorageID] = g
				groupSectors[l.StorageID] = map[int64]bool{}
				groupTypes[l.StorageID] = map[string]bool{}
			}
			groupTypes[l.StorageID][fileTypeName(l.FileType)] = true
			if !groupSectors[l.StorageID][l.SectorNum] {
				groupSectors[l.StorageID][l.SectorNum] = true
				g.Sectors++
				if len(g.Sample) < maxPathGroup {
					g.Sample = append(g.Sample, l.SectorNum)
				}
			}
		}
		for id, g := range groups {
			for t := range groupTypes[id] {
				g.FileTypes = append(g.FileTypes, t)
			}
			sort.Strings(g.FileTypes)
			out.SectorsByPath = append(out.SectorsByPath, *g)
		}
		sort.Slice(out.SectorsByPath, func(i, j int) bool { return out.SectorsByPath[i].Sectors > out.SectorsByPath[j].Sectors })
		for _, sn := range sectors {
			if !located[sn] {
				out.NotInIndex = append(out.NotInIndex, uint64(sn))
			}
		}
		sort.Slice(out.NotInIndex, func(i, j int) bool { return out.NotInIndex[i] < out.NotInIndex[j] })
	}

	for _, h := range out.History {
		if len(out.LogHints) >= 5 {
			break
		}
		out.LogHints = append(out.LogHints, logHint{
			Machine: h.Machine,
			Since:   h.WorkStart.Add(-time.Minute),
			Until:   h.WorkEnd.Add(time.Minute),
			Pattern: `(?i)(wdpost|windowpost|skipp|fault|bad sector|vanilla|post)`,
			Note:    fmt.Sprintf("%s task %d (result %v): use log_find_time on this machine for Since, then log_grep with since/until", h.Name, h.TaskID, h.Result),
		})
	}
	return out, nil
}
