package mcp

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/filecoin-project/go-bitfield"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/curio/aurora/nodediag"
	"github.com/filecoin-project/curio/harmony/harmonydb"

	"github.com/filecoin-project/lotus/chain/types"
)

type storageOverviewArgs struct {
	Machine      string   `json:"machine" jsonschema:"machine ID, name or host:port, or \"all\""`
	PathIDs      []string `json:"path_ids,omitempty" jsonschema:"limit to these storage path IDs"`
	ProblemsOnly bool     `json:"problems_only,omitempty" jsonschema:"only paths with problems (recommended on machines with many paths)"`
	CountEntries bool     `json:"count_entries,omitempty" jsonschema:"count files per sector file type directory (slow on large paths)"`
}

type storageDisksArgs struct {
	Machine  string   `json:"machine" jsonschema:"machine ID, name or host:port, or \"all\""`
	Filter   string   `json:"filter,omitempty" jsonschema:"disk name, serial, WWN, SAS address, enclosure:slot, or a storage path ID"`
	Verdicts []string `json:"verdicts,omitempty" jsonschema:"keep disks with these SMART verdicts: ok, warn, fail, unknown, pending"`
	Offset   int      `json:"offset,omitempty"`
	Limit    int      `json:"limit,omitempty" jsonschema:"disks listed per machine, default 500, max 5000"`
	Refresh  bool     `json:"refresh,omitempty" jsonschema:"re-run smartctl even when results are fresh (cached 15 minutes)"`
}

type diskDetailArgs struct {
	Machine string `json:"machine" jsonschema:"machine ID, name or host:port"`
	Disk    string `json:"disk" jsonschema:"disk name (sdX, nvmeXnY), serial or WWN"`
}

type storageSASArgs struct {
	Machine      string `json:"machine" jsonschema:"machine ID, name or host:port, or \"all\""`
	ProblemsOnly bool   `json:"problems_only,omitempty" jsonschema:"only phys with link errors/degraded rates and enclosure slots needing attention"`
	SES          bool   `json:"ses,omitempty" jsonschema:"also run sg_ses on each enclosure for PSU/fan/temperature/element status (needs a sudo rule)"`
}

type partitionSectorsArgs struct {
	SP        string `json:"sp" jsonschema:"storage provider, e.g. f01234"`
	Deadline  int64  `json:"deadline" jsonschema:"deadline index 0-47"`
	Partition *int64 `json:"partition,omitempty" jsonschema:"partition index; default every partition of the deadline"`
	Sample    int    `json:"sample,omitempty" jsonschema:"sector numbers listed per path group and missing list, default 20, max 5000"`
}

type pathPartitionsArgs struct {
	PathID string `json:"path_id" jsonschema:"storage path ID"`
}

type pathSectorGroup struct {
	StorageID     string
	Sectors       int
	Faulty        int            `json:",omitempty"`
	Files         map[string]int `json:",omitempty"`
	Stale         bool           `json:",omitempty"`
	Missing       bool           `json:",omitempty"`
	HeartbeatAgeS *int64         `json:",omitempty"`
	Machines      []string       `json:",omitempty"`
	Sample        []int64        `json:",omitempty"`
}

type partitionMap struct {
	Partition  uint64
	Live       int
	Faulty     int
	Recovering int
	ByPath     []pathSectorGroup
	// NoSealed / NoCache: live sectors without a sealed (or update) / cache
	// (or update-cache) file in the sector index.
	NoSealedCount int
	NoSealed      []int64 `json:",omitempty"`
	NoCacheCount  int
	NoCache       []int64 `json:",omitempty"`
}

func (s *Server) registerStorageTools2() {
	addTool(s, "storage_overview",
		"Health of the storage paths attached to a machine (or all machines), one compact entry per path: mount, filesystem, usage and inodes, read-only, the backing disks with their worst SMART verdict, and problems: hung mounts (stat timeout), paths that fell back to the root filesystem (mount missing), sectorstore.json mismatch, full filesystems, failing disks. Use problems_only on machines with hundreds of paths.",
		func(ctx context.Context, in storageOverviewArgs) (any, error) {
			ms, err := s.selectMachines(ctx, in.Machine, true)
			if err != nil {
				return nil, err
			}
			return callNodes[nodediag.PathsResponse](ctx, s, ms, nodediag.OpStoragePaths, nodediag.PathsRequest{
				PathIDs: in.PathIDs, ProblemsOnly: in.ProblemsOnly, CountEntries: in.CountEntries, WaitMS: s.waitBudget(0, 30),
			}), nil
		})

	addTool(s, "storage_disks",
		"Disk inventory of a machine (or all) with SMART health: per disk a verdict (ok/warn/fail/unknown, pending while smartctl runs in the background) and reasons (reallocated/pending/uncorrectable sectors, grown defects, uncorrected errors, CRC errors, NVMe critical warnings/endurance/spare, temperature), plus model, serial, WWN, transport, HBA host, SAS address, enclosure slot, mounts and storage path IDs. Problems lists every warn/fail disk regardless of paging. Built for machines with hundreds of drives: filter and page.",
		func(ctx context.Context, in storageDisksArgs) (any, error) {
			ms, err := s.selectMachines(ctx, in.Machine, true)
			if err != nil {
				return nil, err
			}
			return callNodes[nodediag.DisksResponse](ctx, s, ms, nodediag.OpDisks, nodediag.DisksRequest{
				Filter: in.Filter, Verdicts: in.Verdicts, Offset: in.Offset, Limit: in.Limit, Refresh: in.Refresh, WaitMS: s.waitBudget(0, 40),
			}), nil
		})

	addTool(s, "storage_disk_detail",
		"Everything about one disk: inventory entry, full smartctl -x output (error logs, self-test logs, SAS error counter and protocol-specific pages), nvme smart-log/error-log for NVMe, lsblk, and kernel log lines about it.",
		func(ctx context.Context, in diskDetailArgs) (any, error) {
			m, err := s.selectMachine(ctx, in.Machine)
			if err != nil {
				return nil, err
			}
			return callNode[nodediag.DiskDetailResponse](ctx, s, m, nodediag.OpDisk, nodediag.DiskDetailRequest{Disk: in.Disk})
		})

	addTool(s, "storage_sas",
		"SAS topology and external JBOD health of a machine (or all): HBAs (driver, firmware), expanders, phys with negotiated/max link rate and error counters (invalid dword, disparity, loss of sync, reset problems) with the change since the previous call, and SES enclosures with every slot's status, fault LED and disk. Rising phy error counters point at cables, expanders or backplanes; slot status points at failed or pulled drives.",
		func(ctx context.Context, in storageSASArgs) (any, error) {
			ms, err := s.selectMachines(ctx, in.Machine, true)
			if err != nil {
				return nil, err
			}
			return callNodes[nodediag.SASResponse](ctx, s, ms, nodediag.OpSAS, nodediag.SASRequest{ProblemsOnly: in.ProblemsOnly, SES: in.SES}), nil
		})

	addTool(s, "partition_sectors",
		"Map the sectors of a WindowPoSt partition (or every partition of a deadline) to the storage paths holding them: per partition the live/faulty/recovering counts from chain, sectors grouped by storage path (with file types, faulty count, stale or missing path, serving machines, sample sectors), and live sectors with no sealed or no cache file in the sector index. Falls back to sectors_meta (crawled, may lag) without a chain connection.",
		func(ctx context.Context, in partitionSectorsArgs) (any, error) {
			return s.partitionSectors(ctx, in)
		})

	addTool(s, "path_partitions",
		"Blast radius of a storage path: the live sectors with files on it grouped by SP / deadline / partition (from sectors_meta), with how many are faulty and how many have no sealed copy on any other path, i.e. would fail WindowPoSt if this path is lost.",
		func(ctx context.Context, in pathPartitionsArgs) (any, error) {
			if in.PathID == "" {
				return nil, xerrors.New("path_id is required")
			}
			var rows []struct {
				SpID              int64  `db:"sp_id"`
				Deadline          *int64 `db:"deadline"`
				Partition         *int64 `db:"partition"`
				Sectors           int64  `db:"sectors"`
				Faulty            int64  `db:"faulty"`
				NotInMeta         int64  `db:"not_in_meta"`
				NoOtherSealedCopy int64  `db:"no_other_sealed_copy"`
			}
			err := s.ro(ctx, func(tx *harmonydb.Tx) error { return tx.Select(&rows, qPathPartitions, in.PathID) })
			return rows, err
		})
}

func (s *Server) partitionSectors(ctx context.Context, in partitionSectorsArgs) (any, error) {
	spID, maddr, err := s.spID(ctx, in.SP)
	if err != nil {
		return nil, err
	}
	if in.Deadline < 0 || in.Deadline >= 48 {
		return nil, xerrors.New("invalid deadline")
	}
	sample := clamp(in.Sample, 20, 5000)

	type partSets struct {
		live, faulty, recovering map[int64]bool
	}
	parts := map[uint64]*partSets{}
	source := "chain"
	addTo := func(set map[int64]bool, bf bitfield.BitField) error {
		return bf.ForEach(func(n uint64) error { set[int64(n)] = true; return nil })
	}

	if s.d.Chain != nil {
		cps, err := s.d.Chain.StateMinerPartitions(ctx, maddr, uint64(in.Deadline), types.EmptyTSK)
		if err != nil {
			return nil, xerrors.Errorf("getting partitions: %w", err)
		}
		for i, p := range cps {
			if in.Partition != nil && int64(i) != *in.Partition {
				continue
			}
			ps := &partSets{map[int64]bool{}, map[int64]bool{}, map[int64]bool{}}
			if err := addTo(ps.live, p.LiveSectors); err != nil {
				return nil, err
			}
			if err := addTo(ps.faulty, p.FaultySectors); err != nil {
				return nil, err
			}
			if err := addTo(ps.recovering, p.RecoveringSectors); err != nil {
				return nil, err
			}
			parts[uint64(i)] = ps
		}
	} else {
		source = "sectors_meta (no chain connection; crawled data may lag)"
		pf := int64(-1)
		if in.Partition != nil {
			pf = *in.Partition
		}
		var rows []struct {
			SectorNum int64 `db:"sector_num"`
			Partition int64 `db:"partition"`
			IsFaulty  bool  `db:"is_faulty"`
		}
		if err := s.ro(ctx, func(tx *harmonydb.Tx) error {
			return tx.Select(&rows, qMetaPartitionSectors, spID, in.Deadline, pf)
		}); err != nil {
			return nil, err
		}
		for _, r := range rows {
			ps, ok := parts[uint64(r.Partition)]
			if !ok {
				ps = &partSets{map[int64]bool{}, map[int64]bool{}, map[int64]bool{}}
				parts[uint64(r.Partition)] = ps
			}
			ps.live[r.SectorNum] = true
			if r.IsFaulty {
				ps.faulty[r.SectorNum] = true
			}
		}
	}

	var all []int64
	for _, ps := range parts {
		for n := range ps.live {
			all = append(all, n)
		}
	}
	var locs []struct {
		StorageID string `db:"storage_id"`
		SectorNum int64  `db:"sector_num"`
		FileType  int    `db:"sector_filetype"`
	}
	var paths []storagePathRow
	err = s.ro(ctx, func(tx *harmonydb.Tx) error {
		if len(all) > 0 {
			if err := tx.Select(&locs, qSectorsByPath, spID, all); err != nil {
				return err
			}
		}
		return tx.Select(&paths, qStoragePaths)
	})
	if err != nil {
		return nil, err
	}
	ms, err := s.machines(ctx)
	if err != nil {
		return nil, err
	}
	pathInfo := map[string]storagePathRow{}
	for _, p := range paths {
		pathInfo[p.StorageID] = p
	}
	type sectorFiles struct {
		paths  map[string]bool
		sealed bool
		cache  bool
	}
	bySector := map[int64]*sectorFiles{}
	fileTypes := map[string]map[int64]int{} // storage -> sector -> filetype bits
	for _, l := range locs {
		sf, ok := bySector[l.SectorNum]
		if !ok {
			sf = &sectorFiles{paths: map[string]bool{}}
			bySector[l.SectorNum] = sf
		}
		sf.paths[l.StorageID] = true
		switch l.FileType {
		case 2, 8:
			sf.sealed = true
		case 4, 16:
			sf.cache = true
		}
		if fileTypes[l.StorageID] == nil {
			fileTypes[l.StorageID] = map[int64]int{}
		}
		fileTypes[l.StorageID][l.SectorNum] |= l.FileType
	}

	var out struct {
		SP         string
		Deadline   int64
		Source     string
		Partitions []partitionMap
	}
	out.SP, out.Deadline, out.Source = maddr.String(), in.Deadline, source
	var idxs []uint64
	for i := range parts {
		idxs = append(idxs, i)
	}
	sort.Slice(idxs, func(a, b int) bool { return idxs[a] < idxs[b] })

	for _, pi := range idxs {
		ps := parts[pi]
		pm := partitionMap{Partition: pi, Live: len(ps.live), Faulty: len(ps.faulty), Recovering: len(ps.recovering)}
		groups := map[string]*pathSectorGroup{}
		var live []int64
		for n := range ps.live {
			live = append(live, n)
		}
		sort.Slice(live, func(a, b int) bool { return live[a] < live[b] })
		for _, n := range live {
			sf := bySector[n]
			if sf == nil || !sf.sealed {
				pm.NoSealedCount++
				if len(pm.NoSealed) < sample {
					pm.NoSealed = append(pm.NoSealed, n)
				}
			}
			if sf == nil || !sf.cache {
				pm.NoCacheCount++
				if len(pm.NoCache) < sample {
					pm.NoCache = append(pm.NoCache, n)
				}
			}
			if sf == nil {
				continue
			}
			for sid := range sf.paths {
				g, ok := groups[sid]
				if !ok {
					g = &pathSectorGroup{StorageID: sid, Files: map[string]int{}}
					if p, ok := pathInfo[sid]; ok {
						g.HeartbeatAgeS = p.HeartbeatAgeS
						g.Stale = p.HeartbeatAgeS == nil || time.Duration(*p.HeartbeatAgeS)*time.Second > staleHeartbeat
						for _, m := range pathMachines(p.URLs, ms) {
							g.Machines = append(g.Machines, m.label())
						}
					} else {
						g.Missing = true
					}
					groups[sid] = g
				}
				g.Sectors++
				if ps.faulty[n] {
					g.Faulty++
				}
				ft := fileTypes[sid][n]
				for _, t := range []int{1, 2, 4, 8, 16, 32} {
					if ft&t != 0 {
						g.Files[fileTypeName(t)]++
					}
				}
				if len(g.Sample) < sample {
					g.Sample = append(g.Sample, n)
				}
			}
		}
		for _, g := range groups {
			pm.ByPath = append(pm.ByPath, *g)
		}
		sort.Slice(pm.ByPath, func(a, b int) bool { return pm.ByPath[a].Sectors > pm.ByPath[b].Sectors })
		out.Partitions = append(out.Partitions, pm)
	}
	if len(out.Partitions) == 0 {
		return nil, xerrors.Errorf("no partitions found for %s deadline %d%s", maddr, in.Deadline, func() string {
			if in.Partition != nil {
				return fmt.Sprintf(" partition %d", *in.Partition)
			}
			return ""
		}())
	}
	return out, nil
}
