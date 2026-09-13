//go:build !skiff

package paths

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/go-multierror"
	"github.com/ipfs/go-cid"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/curio/lib/proofpaths"
	"github.com/filecoin-project/curio/lib/storiface"
	"github.com/filecoin-project/curio/lib/tarutil"
)

// ReadMinCacheInto writes a tar with the finalized-like (few MiB) cache files
// of the sector (p_aux, t_aux, tree-r-last partitions) into w. See ReadMinCache.
func (r *Remote) ReadMinCacheInto(ctx context.Context, s storiface.SectorRef, ft storiface.SectorFileType, w io.Writer) error {
	_, err := r.ReadMinCache(ctx, s, ft, w)
	return err
}

// ReadMinCache writes a tar with the finalized-like (few MiB) cache files of
// the sector (p_aux, t_aux, tree-r-last partitions) into w and returns the
// storage path the copy was read from.
//
// Copies are tried most-trustworthy first (see sortCopiesByTrust): long-term
// storage before sealing scratch, primary copies first, local before remote,
// then by weight. A copy which lacks the essential finalized-cache files is
// skipped: sealing paths can hold partial caches left behind by crashed
// pipeline runs, and using one of those would otherwise only fail much later,
// e.g. at the end of a snap encode.
func (r *Remote) ReadMinCache(ctx context.Context, s storiface.SectorRef, ft storiface.SectorFileType, w io.Writer) (storiface.ID, error) {
	si, err := r.index.StorageFindSector(ctx, s.ID, ft, 0, false)
	if err != nil {
		log.Debugf("Reader, did not find file on any of the workers %s (%s)", s.ID, ft.String())
		return "", err
	}

	if len(si) == 0 {
		return "", xerrors.Errorf("failed to read sector %v from remote(%d): %w", s, ft, storiface.ErrSectorNotFound)
	}

	localPaths := r.localPathsByID(ctx)
	sortCopiesByTrust(si, "", localPaths)

	for _, info := range si {
		if local, ok := localPaths[info.ID]; ok {
			dir := filepath.Join(local, ft.String(), storiface.SectorName(s.ID))

			fst, err := statSectorFile(dir, nil)
			if err != nil {
				log.Warnw("stat local cache copy", "sector", s.ID, "type", ft, "storage", info.ID, "path", dir, "error", err)
				continue
			}
			if missing := proofpaths.FinCacheMissing(s.ProofType, fst.Files); !fst.IsDir || len(missing) > 0 {
				log.Warnw("skipping incomplete local cache copy", "sector", s.ID, "type", ft, "storage", info.ID, "path", dir, "missing", missing)
				continue
			}

			return info.ID, readMinCache(dir, w)
		}

		for _, u := range info.URLs {
			purl, err := url.Parse(u)
			if err != nil {
				log.Warnw("parsing url", "url", u, "error", err)
				continue
			}

			purl.RawQuery = "mincache=true"
			u = purl.String()

			data, err := r.readRemoteAll(ctx, u)
			if err != nil {
				log.Warnw("reading from remote", "url", u, "error", err)
				continue
			}

			files, err := tarutil.TarFileSizes(bytes.NewReader(data))
			if err != nil {
				log.Warnw("listing remote cache tar", "url", u, "error", err)
				continue
			}
			if missing := proofpaths.FinCacheMissing(s.ProofType, files); len(missing) > 0 {
				log.Warnw("skipping incomplete remote cache copy", "sector", s.ID, "type", ft, "storage", info.ID, "url", u, "missing", missing)
				continue
			}

			if _, err := w.Write(data); err != nil {
				return "", err
			}
			return info.ID, nil
		}
	}

	return "", xerrors.Errorf("failed to read a complete minimal sector cache %v from any of %d copies (%d): %w", s, len(si), ft, storiface.ErrSectorNotFound)
}

// sortCopiesByTrust orders sector copies most-trustworthy first: long-term
// storage before sealing scratch (which may hold leftovers of crashed or
// superseded pipeline runs), primary before secondary copies, the preferred
// storage path (so related files of one sector come from the same copy),
// local before remote, then by weight.
func sortCopiesByTrust(si []storiface.SectorStorageInfo, prefer storiface.ID, localPaths map[storiface.ID]string) {
	sort.SliceStable(si, func(i, j int) bool {
		a, b := si[i], si[j]
		if a.CanStore != b.CanStore {
			return a.CanStore
		}
		if a.Primary != b.Primary {
			return a.Primary
		}
		if prefer != "" && (a.ID == prefer) != (b.ID == prefer) {
			return a.ID == prefer
		}
		_, aLocal := localPaths[a.ID]
		_, bLocal := localPaths[b.ID]
		if aLocal != bLocal {
			return aLocal
		}
		return a.Weight > b.Weight
	})
}

// readRemoteAll fetches url into memory.
func (r *Remote) readRemoteAll(ctx context.Context, url string) ([]byte, error) {
	rd, err := r.ReadRemote(ctx, url, 0, 0)
	if err != nil {
		return nil, err
	}

	data, err := io.ReadAll(rd)
	if cerr := rd.Close(); err == nil && cerr != nil {
		err = xerrors.Errorf("closing remote reader: %w", cerr)
	}
	if err != nil {
		return nil, err
	}

	return data, nil
}

// localPathsByID maps the IDs of storage paths local to this node to their root directories.
func (r *Remote) localPathsByID(ctx context.Context) map[storiface.ID]string {
	ll, ok := r.local.(interface {
		Local(ctx context.Context) ([]storiface.StoragePath, error)
	})
	if !ok {
		return nil
	}

	lps, err := ll.Local(ctx)
	if err != nil {
		log.Warnw("listing local storage paths", "error", err)
		return nil
	}

	out := make(map[storiface.ID]string, len(lps))
	for _, lp := range lps {
		out[lp.ID] = lp.LocalPath
	}
	return out
}

func readMinCache(dir string, writer io.Writer) error {
	return tarutil.TarDirectory(tarutil.FinCacheFileConstraints, dir, writer, make([]byte, 1<<20))
}

func (r *Remote) GenerateSingleVanillaProof(ctx context.Context, minerID abi.ActorID, sinfo storiface.PostSectorChallenge, ppt abi.RegisteredPoStProof) ([]byte, error) {
	p, err := r.local.GenerateSingleVanillaProof(ctx, minerID, sinfo, ppt)
	if err != errPathNotFound {
		return p, err
	}

	sid := abi.SectorID{
		Miner:  minerID,
		Number: sinfo.SectorNumber,
	}

	ft := storiface.FTSealed | storiface.FTCache
	if sinfo.Update {
		ft = storiface.FTUpdate | storiface.FTUpdateCache
	}

	si, err := r.index.StorageFindSector(ctx, sid, ft, 0, false)
	if err != nil {
		return nil, xerrors.Errorf("finding sector %d failed: %w", sid, err)
	}

	requestParams := SingleVanillaParams{
		Miner:     minerID,
		Sector:    sinfo,
		ProofType: ppt,
	}
	jreq, err := json.Marshal(requestParams)
	if err != nil {
		return nil, err
	}

	merr := xerrors.Errorf("sector not found")

	for _, info := range si {
		for _, u := range info.BaseURLs {
			url := fmt.Sprintf("%s/vanilla/single", u)

			req, err := http.NewRequest("POST", url, strings.NewReader(string(jreq)))
			if err != nil {
				merr = multierror.Append(merr, xerrors.Errorf("request: %w", err))
				log.Warnw("GenerateSingleVanillaProof request failed", "url", url, "error", err)
				continue
			}

			if r.auth != nil {
				req.Header = r.auth.Clone()
			}
			req = req.WithContext(ctx)

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				merr = multierror.Append(merr, xerrors.Errorf("do request: %w", err))
				log.Warnw("GenerateSingleVanillaProof do request failed", "url", url, "error", err)
				continue
			}

			if resp.StatusCode != http.StatusOK {
				if resp.StatusCode == http.StatusNotFound {
					log.Debugw("reading vanilla proof from remote not-found response", "url", url, "store", info.ID)
					continue
				}
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					merr = multierror.Append(merr, xerrors.Errorf("resp.Body ReadAll: %w", err))
					log.Warnw("GenerateSingleVanillaProof read response body failed", "url", url, "error", err)
					continue
				}

				if err := resp.Body.Close(); err != nil {
					log.Error("response close: ", err)
				}

				merr = multierror.Append(merr, xerrors.Errorf("non-200 code from %s: '%s'", url, strings.TrimSpace(string(body))))
				log.Warnw("GenerateSingleVanillaProof non-200 code from remote", "code", resp.StatusCode, "url", url, "body", string(body))
				continue
			}

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				if err := resp.Body.Close(); err != nil {
					log.Error("response close: ", err)
				}

				merr = multierror.Append(merr, xerrors.Errorf("resp.Body ReadAll: %w", err))
				log.Warnw("GenerateSingleVanillaProof read response body failed", "url", url, "error", err)
				continue
			}

			_ = resp.Body.Close()

			return body, nil
		}
	}

	return nil, merr
}

func (r *Remote) GeneratePoRepVanillaProof(ctx context.Context, sr storiface.SectorRef, sealed, unsealed cid.Cid, ticket abi.SealRandomness, seed abi.InteractiveSealRandomness) ([]byte, error) {
	// Attempt to generate the proof locally first
	p, err := r.local.GeneratePoRepVanillaProof(ctx, sr, sealed, unsealed, ticket, seed)
	if err != errPathNotFound {
		return p, err
	}

	// Define the file types to look for based on the sector's state
	ft := storiface.FTSealed | storiface.FTCache

	// Find sector information
	si, err := r.index.StorageFindSector(ctx, sr.ID, ft, 0, false)
	if err != nil {
		return nil, xerrors.Errorf("finding sector %d failed: %w", sr.ID, err)
	}

	// Prepare request parameters
	requestParams := PoRepVanillaParams{
		Sector:   sr,
		Sealed:   sealed,
		Unsealed: unsealed,
		Ticket:   ticket,
		Seed:     seed,
	}
	jreq, err := json.Marshal(requestParams)
	if err != nil {
		return nil, err
	}

	merr := xerrors.Errorf("sector not found")

	// Iterate over all found sector locations
	for _, info := range si {
		for _, u := range info.BaseURLs {
			url := fmt.Sprintf("%s/vanilla/porep", u)

			// Create and send the request
			req, err := http.NewRequest("POST", url, strings.NewReader(string(jreq)))
			if err != nil {
				merr = multierror.Append(merr, xerrors.Errorf("request: %w", err))
				log.Warnw("GeneratePoRepVanillaProof request failed", "url", url, "error", err)
				continue
			}

			// Set auth headers if available
			if r.auth != nil {
				req.Header = r.auth.Clone()
			}
			req = req.WithContext(ctx)

			// Execute the request
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				merr = multierror.Append(merr, xerrors.Errorf("do request: %w", err))
				log.Warnw("GeneratePoRepVanillaProof do request failed", "url", url, "error", err)
				continue
			}

			// Handle non-OK status codes
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()

				if resp.StatusCode == http.StatusNotFound {
					log.Debugw("reading vanilla proof from remote not-found response", "url", url, "store", info.ID)
					continue
				}

				merr = multierror.Append(merr, xerrors.Errorf("non-200 code from %s: '%s'", url, strings.TrimSpace(string(body))))
				log.Warnw("GeneratePoRepVanillaProof non-200 code from remote", "code", resp.StatusCode, "url", url, "body", string(body))
				continue
			}

			// Read the response body
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				merr = multierror.Append(merr, xerrors.Errorf("resp.Body ReadAll: %w", err))
				log.Warnw("GeneratePoRepVanillaProof read response body failed", "url", url, "error", err)
				continue
			}

			// Return the proof if successful
			return body, nil
		}
	}

	// Return the accumulated error if the proof was not generated
	return nil, merr
}

func (r *Remote) ReadSnapVanillaProof(ctx context.Context, sr storiface.SectorRef) ([]byte, error) {
	// Attempt to generate the proof locally first
	p, err := r.local.ReadSnapVanillaProof(ctx, sr)
	if err != errPathNotFound {
		return p, err
	}

	// Define the file types to look for based on the sector's state
	ft := storiface.FTUpdateCache

	// Find sector information
	si, err := r.index.StorageFindSector(ctx, sr.ID, ft, 0, false)
	if err != nil {
		return nil, xerrors.Errorf("finding sector %d failed: %w", sr.ID, err)
	}

	// Prepare request parameters
	requestParams := SnapVanillaParams{
		Sector: sr,
	}
	jreq, err := json.Marshal(requestParams)
	if err != nil {
		return nil, err
	}

	merr := xerrors.Errorf("sector not found")

	// Iterate over all found sector locations
	for _, info := range si {
		for _, u := range info.BaseURLs {
			url := fmt.Sprintf("%s/vanilla/snap", u)

			// Create and send the request
			req, err := http.NewRequest("POST", url, strings.NewReader(string(jreq)))
			if err != nil {
				merr = multierror.Append(merr, xerrors.Errorf("request: %w", err))
				log.Warnw("ReadSnapVanillaProof request failed", "url", url, "error", err)
				continue
			}

			// Set auth headers if available
			if r.auth != nil {
				req.Header = r.auth.Clone()
			}
			req = req.WithContext(ctx)

			// Execute the request
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				merr = multierror.Append(merr, xerrors.Errorf("do request: %w", err))
				log.Warnw("ReadSnapVanillaProof do request failed", "url", url, "error", err)
				continue
			}

			// Handle non-OK status codes
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()

				if resp.StatusCode == http.StatusNotFound {
					log.Debugw("reading snap vanilla proof from remote not-found response", "url", url, "store", info.ID)
					continue
				}

				merr = multierror.Append(merr, xerrors.Errorf("non-200 code from %s: '%s'", url, strings.TrimSpace(string(body))))
				log.Warnw("ReadSnapVanillaProof non-200 code from remote", "code", resp.StatusCode, "url", url, "body", string(body))
				continue
			}

			// Read the response body
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				merr = multierror.Append(merr, xerrors.Errorf("resp.Body ReadAll: %w", err)) //nolint:ineffassign
				log.Warnw("ReadSnapVanillaProof read response body failed", "url", url, "error", err)
			}
			_ = resp.Body.Close()

			// Return the proof if successful
			return body, nil
		}
	}

	// Return the accumulated error if the proof was not generated
	return nil, merr
}
