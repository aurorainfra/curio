package paths

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/xerrors"

	"github.com/filecoin-project/curio/lib/storiface"
)

// ReaderSeqTrusted opens a sequential reader for sector file ft, choosing the
// copy the way ReadMinCache does (see sortCopiesByTrust) instead of taking
// whatever copy happens to be local: long-term storage before sealing scratch,
// which may hold leftovers of crashed or superseded sealing runs. prefer names
// the storage path a related file (e.g. the cache of a sealed file) was read
// from, so that both come from the same copy where possible. Sealed and update
// files whose size isn't the sector size are skipped as partial. Returns the
// storage path the reader is backed by.
func (r *Remote) ReaderSeqTrusted(ctx context.Context, s storiface.SectorRef, ft storiface.SectorFileType, prefer storiface.ID) (io.ReadCloser, storiface.ID, error) {
	ctx = context.WithValue(ctx, FindSectorCacheKey, r.findSectorCache)

	si, err := r.index.StorageFindSector(ctx, s.ID, ft, 0, false)
	if err != nil {
		return nil, "", xerrors.Errorf("finding sector %v (%s): %w", s.ID, ft, err)
	}
	if len(si) == 0 {
		return nil, "", xerrors.Errorf("failed to read sector %v from remote(%d): %w", s, ft, storiface.ErrSectorNotFound)
	}

	var expectSize int64
	if ft == storiface.FTSealed || ft == storiface.FTUpdate {
		if ssize, err := s.ProofType.SectorSize(); err == nil {
			expectSize = int64(ssize)
		}
	}

	localPaths := r.localPathsByID(ctx)
	sortCopiesByTrust(si, prefer, localPaths)

	for _, info := range si {
		if local, ok := localPaths[info.ID]; ok {
			p := filepath.Join(local, ft.String(), storiface.SectorName(s.ID))

			if expectSize > 0 {
				st, err := os.Stat(p)
				if err != nil {
					log.Warnw("stat local sector file", "sector", s.ID, "type", ft, "storage", info.ID, "path", p, "error", err)
					continue
				}
				if st.IsDir() || st.Size() != expectSize {
					log.Warnw("skipping local sector file with unexpected size", "sector", s.ID, "type", ft, "storage", info.ID, "path", p, "size", st.Size(), "want", expectSize)
					continue
				}
			}

			f, err := os.Open(p)
			if err != nil {
				log.Warnw("open local sector file", "sector", s.ID, "type", ft, "storage", info.ID, "path", p, "error", err)
				continue
			}
			return f, info.ID, nil
		}

		if expectSize > 0 {
			fst, err := r.StatSectorFile(ctx, s.ID, ft, info.ID)
			switch {
			case errors.Is(err, ErrStatUnsupported):
				// older node, the size can't be checked up front
			case err != nil:
				log.Warnw("stat remote sector file", "sector", s.ID, "type", ft, "storage", info.ID, "error", err)
				continue
			case fst.IsDir || fst.Size != expectSize:
				log.Warnw("skipping remote sector file with unexpected size", "sector", s.ID, "type", ft, "storage", info.ID, "size", fst.Size, "want", expectSize)
				continue
			}
		}

		for _, u := range info.URLs {
			rd, err := r.ReadRemote(ctx, u, 0, 0)
			if err != nil {
				log.Warnw("reading from remote", "url", u, "error", err)
				continue
			}
			return rd, info.ID, nil
		}
	}

	return nil, "", xerrors.Errorf("failed to read sector %v from any of %d copies (%d): %w", s, len(si), ft, storiface.ErrSectorNotFound)
}
