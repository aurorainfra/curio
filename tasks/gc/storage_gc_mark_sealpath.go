package gc

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"go.opencensus.io/stats"
	"go.opencensus.io/tag"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/curio/harmony/harmonydb"
	"github.com/filecoin-project/curio/lib/paths"
	"github.com/filecoin-project/curio/lib/proofpaths"
	"github.com/filecoin-project/curio/lib/storiface"
)

// sealPathLeftover is a sector file declared on a seal-only storage path.
type sealPathLeftover struct {
	sector    abi.SectorID
	fileType  storiface.SectorFileType
	storageID storiface.ID
}

// markSealPathLeftovers is STAGE 4 of the GC mark task.
//
// Seal-only paths are scratch space: once a sector has been moved to a store
// path, a copy left behind on a seal-only path is an artifact, typically of a
// pipeline run which crashed and was later retried elsewhere. Such copies can
// be partial (e.g. a cache directory with tree files but no p_aux), yet they
// are indexed like any other copy and readers may pick them over the good one.
//
// A seal-path copy is marked only when:
//   - the sector is known (has a sectors_meta row, i.e. was committed), is not
//     pinned, and has no sdr/snap pipeline entry before move-storage and no
//     unseal pipeline entry - those still legitimately use seal paths
//   - a copy of the same file type exists on a store path, and that copy is
//     verified complete: sealed/update files have the sector size, unsealed
//     files at least that, cache directories hold p_aux and all tree-r-last
//     partitions.
func (s *StorageGCMark) markSealPathLeftovers(ctx context.Context, storageSectors map[storiface.ID][]storiface.Decl) error {
	var pathRoles []struct {
		StorageID string `db:"storage_id"`
		CanSeal   bool   `db:"can_seal"`
		CanStore  bool   `db:"can_store"`
	}
	err := s.db.Select(ctx, &pathRoles, `SELECT storage_id, COALESCE(can_seal, FALSE) AS can_seal, COALESCE(can_store, FALSE) AS can_store FROM storage_path`)
	if err != nil {
		return xerrors.Errorf("select storage path roles: %w", err)
	}

	sealOnly := map[storiface.ID]bool{}
	store := map[storiface.ID]bool{}
	for _, p := range pathRoles {
		switch {
		case p.CanStore:
			store[storiface.ID(p.StorageID)] = true
		case p.CanSeal:
			sealOnly[storiface.ID(p.StorageID)] = true
		}
	}
	if len(sealOnly) == 0 || len(store) == 0 {
		return nil
	}

	// Candidate leftovers, and the store copies which could make them redundant
	storeCopies := map[abi.SectorID]map[storiface.SectorFileType][]storiface.ID{}
	var candidates []sealPathLeftover
	for storageID, decls := range storageSectors {
		for _, decl := range decls {
			for _, ft := range decl.SectorFileType.AllSet() {
				switch ft {
				case storiface.FTSealed, storiface.FTCache, storiface.FTUnsealed, storiface.FTUpdate, storiface.FTUpdateCache:
				default:
					continue // pieces and keys are not produced by the sealing pipelines
				}

				switch {
				case store[storageID]:
					if storeCopies[decl.SectorID] == nil {
						storeCopies[decl.SectorID] = map[storiface.SectorFileType][]storiface.ID{}
					}
					storeCopies[decl.SectorID][ft] = append(storeCopies[decl.SectorID][ft], storageID)
				case sealOnly[storageID]:
					candidates = append(candidates, sealPathLeftover{sector: decl.SectorID, fileType: ft, storageID: storageID})
				}
			}
		}
	}

	var leftovers []sealPathLeftover
	for _, c := range candidates {
		if len(storeCopies[c.sector][c.fileType]) > 0 {
			leftovers = append(leftovers, c)
		}
	}
	if len(leftovers) == 0 {
		return nil
	}

	// Exclusions: pinned sectors, sectors with pipeline stages still using seal paths, files already marked
	var pinned []struct {
		SpID      int64 `db:"sp_id"`
		SectorNum int64 `db:"sector_num"`
	}
	if err := s.db.Select(ctx, &pinned, `SELECT sp_id, sector_num FROM storage_gc_pins`); err != nil {
		return xerrors.Errorf("select gc pins: %w", err)
	}

	var active []struct {
		SpID      int64 `db:"sp_id"`
		SectorNum int64 `db:"sector_number"`
	}
	err = s.db.Select(ctx, &active, `
		SELECT sp_id, sector_number FROM sectors_sdr_pipeline WHERE after_move_storage = FALSE
		UNION SELECT sp_id, sector_number FROM sectors_snap_pipeline WHERE after_move_storage = FALSE
		UNION SELECT sp_id, sector_number FROM sectors_unseal_pipeline`)
	if err != nil {
		return xerrors.Errorf("select active pipeline sectors: %w", err)
	}

	skip := map[abi.SectorID]struct{}{}
	for _, p := range pinned {
		skip[abi.SectorID{Miner: abi.ActorID(p.SpID), Number: abi.SectorNumber(p.SectorNum)}] = struct{}{}
	}
	for _, a := range active {
		skip[abi.SectorID{Miner: abi.ActorID(a.SpID), Number: abi.SectorNumber(a.SectorNum)}] = struct{}{}
	}

	var marked []struct {
		SpID      int64  `db:"sp_id"`
		SectorNum int64  `db:"sector_num"`
		FileType  int64  `db:"sector_filetype"`
		StorageID string `db:"storage_id"`
	}
	if err := s.db.Select(ctx, &marked, `SELECT sp_id, sector_num, sector_filetype, storage_id FROM storage_removal_marks`); err != nil {
		return xerrors.Errorf("select existing marks: %w", err)
	}
	alreadyMarked := map[sealPathLeftover]struct{}{}
	for _, m := range marked {
		alreadyMarked[sealPathLeftover{
			sector:    abi.SectorID{Miner: abi.ActorID(m.SpID), Number: abi.SectorNumber(m.SectorNum)},
			fileType:  storiface.SectorFileType(m.FileType),
			storageID: storiface.ID(m.StorageID),
		}] = struct{}{}
	}

	var metas []struct {
		SpID         int64 `db:"sp_id"`
		SectorNum    int64 `db:"sector_num"`
		RegSealProof int64 `db:"reg_seal_proof"`
	}
	if err := s.db.Select(ctx, &metas, `SELECT sp_id, sector_num, reg_seal_proof FROM sectors_meta`); err != nil {
		return xerrors.Errorf("select sector proof types: %w", err)
	}
	proofTypes := map[abi.SectorID]abi.RegisteredSealProof{}
	for _, m := range metas {
		proofTypes[abi.SectorID{Miner: abi.ActorID(m.SpID), Number: abi.SectorNumber(m.SectorNum)}] = abi.RegisteredSealProof(m.RegSealProof)
	}

	sort.Slice(leftovers, func(i, j int) bool {
		a, b := leftovers[i], leftovers[j]
		if a.sector.Miner != b.sector.Miner {
			return a.sector.Miner < b.sector.Miner
		}
		if a.sector.Number != b.sector.Number {
			return a.sector.Number < b.sector.Number
		}
		if a.fileType != b.fileType {
			return a.fileType < b.fileType
		}
		return a.storageID < b.storageID
	})

	// Verify the store copies. Storage nodes which can't answer stat queries
	// (older version) are remembered so they're not asked once per file.
	unsupported := map[storiface.ID]bool{}
	var toMark []sealPathLeftover
	for _, l := range leftovers {
		if _, ok := skip[l.sector]; ok {
			continue
		}
		if _, ok := alreadyMarked[l]; ok {
			continue
		}
		spt, ok := proofTypes[l.sector]
		if !ok {
			continue // not a committed sector we know of; stage 1 handles files of unknown sectors
		}
		ssize, err := spt.SectorSize()
		if err != nil {
			log.Warnw("sector size for seal-path leftover", "miner", l.sector.Miner, "sector", l.sector.Number, "proof", spt, "error", err)
			continue
		}

		verified := false
		for _, storeID := range storeCopies[l.sector][l.fileType] {
			if unsupported[storeID] {
				continue
			}

			fst, err := s.remote.StatSectorFile(ctx, l.sector, l.fileType, storeID)
			if err != nil {
				if errors.Is(err, paths.ErrStatUnsupported) {
					log.Warnw("storage node does not support sector file stat, upgrade it to GC seal-path leftovers", "storage_id", storeID)
					unsupported[storeID] = true
				} else {
					log.Warnw("stat store copy", "miner", l.sector.Miner, "sector", l.sector.Number, "filetype", l.fileType, "storage_id", storeID, "error", err)
				}
				continue
			}

			if err := storeCopyComplete(l.fileType, spt, ssize, fst); err != nil {
				log.Warnw("store copy incomplete, keeping seal-path copy", "miner", l.sector.Miner, "sector", l.sector.Number, "filetype", l.fileType, "storage_id", storeID, "error", err)
				continue
			}

			verified = true
			break
		}
		if !verified {
			continue
		}

		toMark = append(toMark, l)
	}
	if len(toMark) == 0 {
		return nil
	}

	_, err = s.db.BeginTransaction(ctx, func(tx *harmonydb.Tx) (commit bool, err error) {
		for _, l := range toMark {
			n, err := tx.Exec(`INSERT INTO storage_removal_marks (sp_id, sector_num, sector_filetype, storage_id)
				VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`, int64(l.sector.Miner), int64(l.sector.Number), int64(l.fileType), string(l.storageID))
			if err != nil {
				return false, xerrors.Errorf("insert storage_removal_marks: %w", err)
			}
			if n == 0 {
				continue
			}

			log.Infow("file marked for GC", "miner", l.sector.Miner, "sector", l.sector.Number, "filetype", l.fileType, "storage_id", l.storageID, "reason", "seal-path-leftover")

			// Record GC metric
			if maddr, err := address.NewIDAddress(uint64(l.sector.Miner)); err == nil {
				if err = stats.RecordWithTags(ctx, []tag.Mutator{
					tag.Upsert(MinerTag, maddr.String()),
					tag.Upsert(FileTypeTag, fmt.Sprint(l.fileType)),
				}, GCMeasures.SectorsMarkedTotal.M(1)); err != nil {
					log.Errorf("recording metric: %v", err)
				}
			}
		}

		return true, nil
	}, harmonydb.OptionRetry())
	if err != nil {
		return xerrors.Errorf("mark transaction: %w", err)
	}

	return nil
}

// storeCopyComplete checks that the store copy described by fst can serve every
// use of a file of type ft, which is what makes a seal-path copy redundant.
func storeCopyComplete(ft storiface.SectorFileType, spt abi.RegisteredSealProof, ssize abi.SectorSize, fst storiface.SectorFileStat) error {
	switch ft {
	case storiface.FTSealed, storiface.FTUpdate:
		if fst.IsDir {
			return xerrors.Errorf("expected a file, found a directory")
		}
		if fst.Size != int64(ssize) {
			return xerrors.Errorf("size %d, want %d", fst.Size, ssize)
		}
	case storiface.FTUnsealed:
		if fst.IsDir {
			return xerrors.Errorf("expected a file, found a directory")
		}
		if fst.Size < int64(ssize) {
			return xerrors.Errorf("size %d, want at least %d", fst.Size, ssize)
		}
	case storiface.FTCache, storiface.FTUpdateCache:
		if !fst.IsDir {
			return xerrors.Errorf("expected a directory, found a file")
		}
		if _, err := proofpaths.TreeRLastPartitions(spt); err != nil {
			return xerrors.Errorf("can't determine expected cache layout: %w", err)
		}
		if missing := proofpaths.FinCacheMissing(spt, fst.Files); len(missing) > 0 {
			return xerrors.Errorf("missing %v", missing)
		}
	default:
		return xerrors.Errorf("unexpected file type %s", ft)
	}

	return nil
}
