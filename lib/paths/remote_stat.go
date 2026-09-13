package paths

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	gopath "path"
	"path/filepath"
	"strings"

	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/curio/lib/storiface"
)

// ErrStatUnsupported is returned by StatSectorFile when the node serving the
// storage path runs a version which does not implement the stat query.
var ErrStatUnsupported = errors.New("remote storage node does not support sector file stat")

// statSectorFile describes the sector file or cache directory at path. stat may
// be nil, in which case the path is stat-ed here.
func statSectorFile(path string, stat os.FileInfo) (storiface.SectorFileStat, error) {
	if stat == nil {
		var err error
		stat, err = os.Stat(path)
		if err != nil {
			return storiface.SectorFileStat{}, err
		}
	}

	if !stat.IsDir() {
		return storiface.SectorFileStat{Size: stat.Size()}, nil
	}

	ents, err := os.ReadDir(path)
	if err != nil {
		return storiface.SectorFileStat{}, xerrors.Errorf("listing %s: %w", path, err)
	}

	files := make(map[string]int64, len(ents))
	for _, ent := range ents {
		if ent.IsDir() {
			continue
		}
		info, err := ent.Info()
		if err != nil {
			if os.IsNotExist(err) {
				continue // removed while listing
			}
			return storiface.SectorFileStat{}, xerrors.Errorf("stat %s: %w", ent.Name(), err)
		}
		files[ent.Name()] = info.Size()
	}

	return storiface.SectorFileStat{IsDir: true, Files: files}, nil
}

// StatSectorFile describes the copy of sector file ft on storage path id
// without transferring it: the size of a regular file, or the names and sizes
// of the files inside a cache directory. Local paths are inspected directly,
// remote ones through the fetch handler's ?stat=true query.
func (r *Remote) StatSectorFile(ctx context.Context, s abi.SectorID, ft storiface.SectorFileType, id storiface.ID) (storiface.SectorFileStat, error) {
	if local, ok := r.localPathsByID(ctx)[id]; ok {
		return statSectorFile(filepath.Join(local, ft.String(), storiface.SectorName(s)), nil)
	}

	sinfo, err := r.index.StorageInfo(ctx, id)
	if err != nil {
		return storiface.SectorFileStat{}, xerrors.Errorf("getting storage info for %s: %w", id, err)
	}

	lastErr := xerrors.Errorf("storage path %s has no urls", id)
	for _, u := range sinfo.URLs {
		purl, err := url.Parse(u)
		if err != nil {
			lastErr = xerrors.Errorf("parsing url %s: %w", u, err)
			continue
		}
		purl.Path = gopath.Join(purl.Path, ft.String(), storiface.SectorName(s))
		purl.RawQuery = "stat=true"

		fst, err := r.statRemote(ctx, purl.String())
		if err != nil {
			if errors.Is(err, ErrStatUnsupported) {
				return storiface.SectorFileStat{}, err
			}
			log.Warnw("stat sector file on remote", "url", purl.String(), "error", err)
			lastErr = err
			continue
		}

		return fst, nil
	}

	return storiface.SectorFileStat{}, xerrors.Errorf("stat %s (%s) on %s: %w", s, ft, id, lastErr)
}

func (r *Remote) statRemote(ctx context.Context, url string) (storiface.SectorFileStat, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return storiface.SectorFileStat{}, xerrors.Errorf("request: %w", err)
	}
	if r.auth != nil {
		req.Header = r.auth.Clone()
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return storiface.SectorFileStat{}, xerrors.Errorf("do request: %w", err)
	}
	defer resp.Body.Close() // nolint

	if resp.StatusCode != http.StatusOK {
		return storiface.SectorFileStat{}, xerrors.Errorf("non-200 code: %d", resp.StatusCode)
	}

	// A node predating the stat query ignores it and streams the file/tar instead.
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return storiface.SectorFileStat{}, xerrors.Errorf("got content-type %q: %w", ct, ErrStatUnsupported)
	}

	var out storiface.SectorFileStat
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return storiface.SectorFileStat{}, xerrors.Errorf("decoding stat response: %w", err)
	}

	return out, nil
}
