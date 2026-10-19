package mcp

import (
	"context"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/filecoin-project/go-state-types/abi"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/curio/aurora/nodediag"
	"github.com/filecoin-project/curio/harmony/harmonydb"
	"github.com/filecoin-project/curio/lib/storiface"
)

// storage paths whose heartbeat is older than this are reported stale
const staleHeartbeat = 5 * time.Minute

type storagePathRow struct {
	StorageID     string     `db:"storage_id"`
	URLs          string     `db:"urls"`
	Weight        *int64     `db:"weight"`
	MaxStorage    *int64     `db:"max_storage"`
	CanSeal       bool       `db:"can_seal"`
	CanStore      bool       `db:"can_store"`
	Groups        string     `db:"groups"`
	AllowTo       string     `db:"allow_to"`
	AllowTypes    string     `db:"allow_types"`
	DenyTypes     string     `db:"deny_types"`
	AllowMiners   string     `db:"allow_miners"`
	DenyMiners    string     `db:"deny_miners"`
	Capacity      *int64     `db:"capacity"`
	Available     *int64     `db:"available"`
	FSAvailable   *int64     `db:"fs_available"`
	Reserved      *int64     `db:"reserved"`
	Used          *int64     `db:"used"`
	LastHeartbeat *time.Time `db:"last_heartbeat"`
	HeartbeatAgeS *int64     `db:"heartbeat_age_s"`
}

type urlLiveness struct {
	StorageID      string     `db:"storage_id"`
	URL            string     `db:"url"`
	LastChecked    time.Time  `db:"last_checked"`
	LastLive       *time.Time `db:"last_live"`
	LastDead       *time.Time `db:"last_dead"`
	LastDeadReason string     `db:"last_dead_reason"`
}

type fileCount struct {
	StorageID string `db:"storage_id"`
	FileType  int    `db:"sector_filetype"`
	Files     int64  `db:"files"`
}

type storagePathsArgs struct {
	WithCounts bool `json:"with_counts,omitempty" jsonschema:"also count indexed sector files per path and file type (scans the whole sector index)"`
}

type storagePathOut struct {
	storagePathRow
	Stale    bool
	Machines []string         `json:",omitempty"`
	URLState []urlLiveness    `json:",omitempty"`
	Files    map[string]int64 `json:",omitempty"`
}

type sectorLocateArgs struct {
	SP     string `json:"sp" jsonschema:"storage provider, e.g. f01234"`
	Sector int64  `json:"sector" jsonschema:"sector number"`
	Check  bool   `json:"check,omitempty" jsonschema:"stat every indexed copy on its storage path (local stat or HTTP stat on the serving node; no data is read)"`
}

type sectorLocation struct {
	FileType       int        `db:"sector_filetype"`
	StorageID      string     `db:"storage_id"`
	IsPrimary      *bool      `db:"is_primary"`
	ReadTS         *time.Time `db:"read_ts"`
	ReadRefs       int64      `db:"read_refs"`
	WriteTS        *time.Time `db:"write_ts"`
	WriteLockOwner string     `db:"write_lock_owner"`
	URLs           string     `db:"urls"`
	LastHeartbeat  *time.Time `db:"last_heartbeat"`
	PathMissing    bool       `db:"path_missing"`
	FileTypeName   string     `db:"-"`
	Stat           any        `db:"-" json:",omitempty"`
	StatError      string     `db:"-" json:",omitempty"`
}

type sectorMeta struct {
	RegSealProof    int64  `db:"reg_seal_proof"`
	TicketEpoch     int64  `db:"ticket_epoch"`
	OrigSealedCID   string `db:"orig_sealed_cid"`
	CurSealedCID    string `db:"cur_sealed_cid"`
	CurUnsealedCID  string `db:"cur_unsealed_cid"`
	MsgCIDPrecommit string `db:"msg_cid_precommit"`
	MsgCIDCommit    string `db:"msg_cid_commit"`
	MsgCIDUpdate    string `db:"msg_cid_update"`
	IsCC            *bool  `db:"is_cc"`
	ExpirationEpoch *int64 `db:"expiration_epoch"`
	Deadline        *int64 `db:"deadline"`
	Partition       *int64 `db:"partition"`
	IsLive          bool   `db:"is_live"`
	IsFaulty        bool   `db:"is_faulty"`
	HasSectorKey    bool   `db:"has_sector_key"`
}

type pathSectorsArgs struct {
	PathID   string `json:"path_id" jsonschema:"storage path ID"`
	FileType string `json:"file_type,omitempty" jsonschema:"only list this file type: unsealed, sealed, cache, update, update-cache, piece"`
	Limit    int    `json:"limit,omitempty" jsonschema:"max sample rows, default 200, max 5000"`
}

type missingRefsArgs struct {
	SP    string `json:"sp,omitempty" jsonschema:"limit to one storage provider"`
	Limit int    `json:"limit,omitempty" jsonschema:"max sectors listed, default 500, max 10000"`
}

type storageDiagArgs struct {
	PathID  string `json:"path_id" jsonschema:"storage path ID (from storage_paths)"`
	Machine string `json:"machine,omitempty" jsonschema:"machine serving the path; default: machines whose host:port appears in the path URLs"`
}

func fileTypeName(ft int) string {
	return storiface.SectorFileType(ft).String()
}

func parseFileType(s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	for _, ft := range storiface.PathTypes {
		if ft.String() == s {
			return int(ft), nil
		}
	}
	return 0, xerrors.Errorf("unknown file type %q", s)
}

// pathMachines maps a storage path to the machines serving it, by matching
// the host:port of its URLs against harmony_machines.
func pathMachines(urls string, ms []machine) []machine {
	var out []machine
	for _, u := range strings.Split(urls, ",") {
		pu, err := url.Parse(strings.TrimSpace(u))
		if err != nil || pu.Host == "" {
			continue
		}
		for _, m := range ms {
			if m.HostPort == pu.Host {
				out = append(out, m)
			}
		}
	}
	return out
}

func (s *Server) registerStorageTools() {
	addTool(s, "storage_paths",
		"All storage paths in the sector index: URLs, seal/store flags, groups and filters, capacity/available/used, last heartbeat age (Stale when older than 5 minutes; a path whose node is gone stops heartbeating and is eventually removed together with its sector_location rows by storage_endpoint_gc), per-URL liveness checks, and the machines serving it.",
		func(ctx context.Context, in storagePathsArgs) (any, error) {
			var rows []storagePathRow
			var live []urlLiveness
			var counts []fileCount
			err := s.ro(ctx, func(tx *harmonydb.Tx) error {
				if err := tx.Select(&rows, qStoragePaths); err != nil {
					return err
				}
				if err := tx.Select(&live, qStorageURLLiveness); err != nil {
					return err
				}
				if in.WithCounts {
					return tx.Select(&counts, qStoragePathFileCounts)
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
			ms, err := s.machines(ctx)
			if err != nil {
				return nil, err
			}
			out := make([]storagePathOut, 0, len(rows))
			idx := map[string]int{}
			for _, r := range rows {
				o := storagePathOut{storagePathRow: r}
				o.Stale = r.HeartbeatAgeS == nil || time.Duration(*r.HeartbeatAgeS)*time.Second > staleHeartbeat
				for _, m := range pathMachines(r.URLs, ms) {
					o.Machines = append(o.Machines, m.label())
				}
				idx[r.StorageID] = len(out)
				out = append(out, o)
			}
			for _, l := range live {
				if i, ok := idx[l.StorageID]; ok {
					out[i].URLState = append(out[i].URLState, l)
				}
			}
			var unknown []fileCount
			for _, c := range counts {
				i, ok := idx[c.StorageID]
				if !ok {
					unknown = append(unknown, c)
					continue
				}
				if out[i].Files == nil {
					out[i].Files = map[string]int64{}
				}
				out[i].Files[fileTypeName(c.FileType)] += c.Files
			}
			return struct {
				Paths []storagePathOut
				// IndexedOnUnknownPaths counts sector files indexed on
				// storage IDs not in storage_path (with_counts only).
				IndexedOnUnknownPaths []fileCount `json:",omitempty"`
			}{out, unknown}, nil
		})

	addTool(s, "sector_locate",
		"Where a sector's files are: every sector_location entry (file type, storage path, primary, read/write locks) with the path's URLs and heartbeat, sectors_meta (on-chain state as last crawled: live/faulty, deadline/partition, CIDs, expiration) and the sealing/snap pipeline rows. With check=true, stats each indexed copy.",
		func(ctx context.Context, in sectorLocateArgs) (any, error) {
			spID, maddr, err := s.spID(ctx, in.SP)
			if err != nil {
				return nil, err
			}
			if in.Sector < 0 {
				return nil, xerrors.New("invalid sector number")
			}
			var out struct {
				SP           string
				Sector       int64
				Locations    []sectorLocation
				Meta         *sectorMeta `json:",omitempty"`
				SealPipeline rawJSON     `json:",omitempty"`
				SnapPipeline rawJSON     `json:",omitempty"`
			}
			out.SP, out.Sector = maddr.String(), in.Sector
			err = s.ro(ctx, func(tx *harmonydb.Tx) error {
				if err := tx.Select(&out.Locations, qSectorLocations, spID, in.Sector); err != nil {
					return err
				}
				var meta []sectorMeta
				if err := tx.Select(&meta, qSectorMeta, spID, in.Sector); err != nil {
					return err
				}
				if len(meta) > 0 {
					out.Meta = &meta[0]
				}
				var pj []string
				if err := tx.Select(&pj, qSectorSDRPipeline, spID, in.Sector); err != nil {
					return err
				}
				if len(pj) > 0 {
					out.SealPipeline = rawJSON(pj[0])
				}
				pj = nil
				if err := tx.Select(&pj, qSectorSnapPipeline, spID, in.Sector); err != nil {
					return err
				}
				if len(pj) > 0 {
					out.SnapPipeline = rawJSON(pj[0])
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
			for i := range out.Locations {
				l := &out.Locations[i]
				l.FileTypeName = fileTypeName(l.FileType)
				if !in.Check || l.PathMissing || s.d.Stor == nil {
					continue
				}
				st, err := s.d.Stor.StatSectorFile(ctx, abi.SectorID{Miner: abi.ActorID(spID), Number: abi.SectorNumber(in.Sector)}, storiface.SectorFileType(l.FileType), storiface.ID(l.StorageID))
				if err != nil {
					l.StatError = err.Error()
					continue
				}
				l.Stat = st
			}
			return out, nil
		})

	addTool(s, "storage_path_sectors",
		"Sector files indexed on one storage path: counts per file type and a sample of (miner, sector, file type) rows. Useful to see what goes missing if the path drops.",
		func(ctx context.Context, in pathSectorsArgs) (any, error) {
			if in.PathID == "" {
				return nil, xerrors.New("path_id is required")
			}
			ft, err := parseFileType(in.FileType)
			if err != nil {
				return nil, err
			}
			var out struct {
				Counts []struct {
					FileType int   `db:"sector_filetype"`
					Files    int64 `db:"files"`
					Miners   int64 `db:"miners"`
				}
				Sample []struct {
					MinerID   int64 `db:"miner_id"`
					SectorNum int64 `db:"sector_num"`
					FileType  int   `db:"sector_filetype"`
					IsPrimary *bool `db:"is_primary"`
				}
			}
			err = s.ro(ctx, func(tx *harmonydb.Tx) error {
				if err := tx.Select(&out.Counts, qPathSectorCounts, in.PathID); err != nil {
					return err
				}
				return tx.Select(&out.Sample, qPathSectorSample, in.PathID, ft, clamp(in.Limit, 200, 5000))
			})
			return out, err
		})

	addTool(s, "missing_refs",
		"Missing storage diagnosis: live sectors (sectors_meta) with no sealed/update or no cache/update-cache file in the sector index, grouped by deadline; sector_location rows pointing at storage IDs that no longer exist; and storage paths with stale heartbeats (their sectors go missing once storage_endpoint_gc removes the path).",
		func(ctx context.Context, in missingRefsArgs) (any, error) {
			spFilter := int64(-1)
			if in.SP != "" {
				id, _, err := s.spID(ctx, in.SP)
				if err != nil {
					return nil, err
				}
				spFilter = int64(id)
			}
			limit := clamp(in.Limit, 500, 10000)
			var missing []struct {
				SpID         int64  `db:"sp_id"`
				SectorNum    int64  `db:"sector_num"`
				Deadline     *int64 `db:"deadline"`
				Partition    *int64 `db:"partition"`
				IsFaulty     bool   `db:"is_faulty"`
				HasSectorKey bool   `db:"has_sector_key"`
				HasSealed    bool   `db:"has_sealed"`
				HasCache     bool   `db:"has_cache"`
			}
			var dangling []struct {
				StorageID string `db:"storage_id"`
				Files     int64  `db:"files"`
				Sectors   int64  `db:"sectors"`
			}
			var paths []storagePathRow
			err := s.ro(ctx, func(tx *harmonydb.Tx) error {
				if err := tx.Select(&missing, qMissingSectorFiles, spFilter, limit); err != nil {
					return err
				}
				if err := tx.Select(&dangling, qDanglingLocations); err != nil {
					return err
				}
				return tx.Select(&paths, qStoragePaths)
			})
			if err != nil {
				return nil, err
			}

			type dlKey struct {
				SpID     int64
				Deadline int64
			}
			byDl := map[dlKey]int{}
			for _, m := range missing {
				k := dlKey{m.SpID, -1}
				if m.Deadline != nil {
					k.Deadline = *m.Deadline
				}
				byDl[k]++
			}
			type dlCount struct {
				SpID     int64
				Deadline int64
				Sectors  int
			}
			var dls []dlCount
			for k, n := range byDl {
				dls = append(dls, dlCount{k.SpID, k.Deadline, n})
			}
			sort.Slice(dls, func(i, j int) bool {
				if dls[i].SpID != dls[j].SpID {
					return dls[i].SpID < dls[j].SpID
				}
				return dls[i].Deadline < dls[j].Deadline
			})

			var stale []storagePathRow
			for _, p := range paths {
				if p.HeartbeatAgeS == nil || time.Duration(*p.HeartbeatAgeS)*time.Second > staleHeartbeat {
					stale = append(stale, p)
				}
			}
			return map[string]any{
				"MissingSectors":          missing,
				"MissingSectorsTruncated": len(missing) >= limit,
				"MissingByDeadline":       dls,
				"LocationsOnUnknownPaths": dangling,
				"StalePaths":              stale,
			}, nil
		})

	addTool(s, "storage_diag",
		"Deep check of one storage path on the machine serving it: sectorstore.json, statfs, entry counts per file type directory, the mount and the block devices behind it (partitions, md/dm/LVM members), SMART summaries of the backing disks, df, findmnt, lsblk, /proc/mdstat and storage-related kernel log lines. For many paths use storage_overview; for full smartctl output use storage_disk_detail.",
		func(ctx context.Context, in storageDiagArgs) (any, error) {
			if in.PathID == "" {
				return nil, xerrors.New("path_id is required")
			}
			var targets []machine
			if in.Machine != "" {
				m, err := s.selectMachine(ctx, in.Machine)
				if err != nil {
					return nil, err
				}
				targets = []machine{m}
			} else {
				var paths []storagePathRow
				if err := s.ro(ctx, func(tx *harmonydb.Tx) error { return tx.Select(&paths, qStoragePaths) }); err != nil {
					return nil, err
				}
				ms, err := s.machines(ctx)
				if err != nil {
					return nil, err
				}
				for _, p := range paths {
					if p.StorageID == in.PathID {
						targets = pathMachines(p.URLs, ms)
					}
				}
				if len(targets) == 0 {
					return nil, xerrors.Errorf("no machine found serving storage path %s; pass machine explicitly", in.PathID)
				}
			}
			return callNodes[nodediag.StorageDiagResponse](ctx, s, targets, nodediag.OpStorage, nodediag.StorageDiagRequest{PathID: in.PathID}), nil
		})
}
