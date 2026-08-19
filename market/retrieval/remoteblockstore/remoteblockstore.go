package remoteblockstore

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/hashicorp/go-multierror"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	format "github.com/ipfs/go-ipld-format"
	logging "github.com/ipfs/go-log/v2"
	"github.com/ipld/go-car/util"
	"github.com/multiformats/go-multihash"
	"go.opencensus.io/stats"

	"github.com/filecoin-project/curio/harmony/harmonydb"
	"github.com/filecoin-project/curio/lib/cachedreader"
	"github.com/filecoin-project/curio/lib/commcidv2"
	"github.com/filecoin-project/curio/lib/storiface"
	"github.com/filecoin-project/curio/market/indexstore"
)

const MaxCachedReaders = 128

const MaxCarBlockPrefixSize = 128 // car entry len varint + cid len

var log = logging.Logger("remote-blockstore")

type idxAPI interface {
	PiecesContainingMultihash(ctx context.Context, m multihash.Multihash) ([]indexstore.PieceInfo, error)
	GetOffset(ctx context.Context, pieceCidv2 cid.Cid, hash multihash.Multihash) (uint64, error)
	GetPieceBlockOffsets(ctx context.Context, pieceCid cid.Cid) ([]indexstore.BlockOffset, error)
}

// RemoteBlockstore is a read-only blockstore over all cids across all pieces on a provider.
type RemoteBlockstore struct {
	idxApi       idxAPI
	blockMetrics *BlockMetrics
	db           *harmonydb.DB
	cpr          *cachedreader.CachedPieceReader

	// offsets is non-nil when Indexing.RetrievalOffsetCachePieces > 0; it
	// serves multihash -> (piece, offset) resolution for hot pieces from
	// memory, skipping both index lookups per block.
	offsets *offsetCache
}

type BlockMetrics struct {
	GetRequestCount             *stats.Int64Measure
	GetFailResponseCount        *stats.Int64Measure
	GetSuccessResponseCount     *stats.Int64Measure
	BytesSentCount              *stats.Int64Measure
	HasRequestCount             *stats.Int64Measure
	HasFailResponseCount        *stats.Int64Measure
	HasSuccessResponseCount     *stats.Int64Measure
	GetSizeRequestCount         *stats.Int64Measure
	GetSizeFailResponseCount    *stats.Int64Measure
	GetSizeSuccessResponseCount *stats.Int64Measure
}

func NewRemoteBlockstore(api idxAPI, db *harmonydb.DB, cpr *cachedreader.CachedPieceReader, offsetCachePieces int) *RemoteBlockstore {
	httpBlockMetrics := &BlockMetrics{
		GetRequestCount:             HttpRblsGetRequestCount,
		GetFailResponseCount:        HttpRblsGetFailResponseCount,
		GetSuccessResponseCount:     HttpRblsGetSuccessResponseCount,
		BytesSentCount:              HttpRblsBytesSentCount,
		HasRequestCount:             HttpRblsHasRequestCount,
		HasFailResponseCount:        HttpRblsHasFailResponseCount,
		HasSuccessResponseCount:     HttpRblsHasSuccessResponseCount,
		GetSizeRequestCount:         HttpRblsGetSizeRequestCount,
		GetSizeFailResponseCount:    HttpRblsGetSizeFailResponseCount,
		GetSizeSuccessResponseCount: HttpRblsGetSizeSuccessResponseCount,
	}

	rbs := &RemoteBlockstore{
		idxApi:       api,
		blockMetrics: httpBlockMetrics,
		db:           db,
		cpr:          cpr,
	}
	if offsetCachePieces > 0 {
		rbs.offsets = newOffsetCache(api, offsetCachePieces)
	}
	return rbs
}

func (ro *RemoteBlockstore) Get(ctx context.Context, c cid.Cid) (b blocks.Block, err error) {
	if ro.blockMetrics != nil {
		stats.Record(ctx, ro.blockMetrics.GetRequestCount.M(1))
	}

	defer func() {
		var nb int
		if b != nil {
			nb = len(b.RawData())
		}
		log.Debugw("Get", "cid", c, "err", err, "bytes", nb, "bnil", b == nil)
	}()

	// Fast path: blocks of recently served pieces resolve piece and offset
	// from the in-memory offset cache, skipping both index lookups. Any
	// failure falls through to the full path below (and drops the cached
	// piece, so a stale index self-heals).
	if ro.offsets != nil {
		if data, ok := ro.getViaOffsetCache(ctx, c); ok {
			return blocks.NewBlockWithCid(data, c)
		}
	}

	// Get the pieces that contain the cid
	pieces, err := ro.idxApi.PiecesContainingMultihash(ctx, c.Hash())

	// Check if it's an identity cid, if it is, return its digest
	if err != nil {
		digest, ok, iderr := isIdentity(c)
		if iderr == nil && ok {
			if ro.blockMetrics != nil {
				stats.Record(ctx, ro.blockMetrics.GetSuccessResponseCount.M(1))
			}
			return blocks.NewBlockWithCid(digest, c)
		}
		if ro.blockMetrics != nil {
			stats.Record(ctx, ro.blockMetrics.GetFailResponseCount.M(1))
		}
		return nil, fmt.Errorf("getting pieces containing cid %s: %w", c, err)
	}
	if len(pieces) == 0 {
		return nil, format.ErrNotFound{Cid: c}
	}

	// Get a reader over one of the pieces and extract the block data
	var merr error
	for _, piece := range pieces {
		log.Debugw("trying piece for block retrieval",
			"contentCid", c, "pieceCid", piece.PieceCid, "blockSize", piece.BlockSize,
			"pieceCidIsV2", commcidv2.IsPieceCidV2(piece.PieceCid),
			"pieceCidIsV1", commcidv2.IsCidV1PieceCid(piece.PieceCid))

		data, err := func() ([]byte, error) {
			reader, _, err := ro.cpr.GetSharedPieceReader(ctx, piece.PieceCid, true)
			if err != nil {
				return nil, fmt.Errorf("getting piece reader for piece %s (isV2=%v): %w",
					piece.PieceCid, commcidv2.IsPieceCidV2(piece.PieceCid), err)
			}
			defer func(reader storiface.Reader) {
				_ = reader.Close()
			}(reader)

			// Get the offset of the block within the piece (CAR file).
			// GetOffset now has V2↔V1 fallback for legacy/mixed Cassandra indexes.
			offset, err := ro.idxApi.GetOffset(ctx, piece.PieceCid, c.Hash())
			if err != nil {
				return nil, fmt.Errorf("getting offset/size for cid %s in piece %s (isV2=%v): %w",
					c, piece.PieceCid, commcidv2.IsPieceCidV2(piece.PieceCid), err)
			}

			// Try CAR block layout first (varint length + CID + payload).
			readerAt := io.NewSectionReader(reader, int64(offset), int64(piece.BlockSize+MaxCarBlockPrefixSize))
			readCid, data, err := util.ReadNode(bufio.NewReader(readerAt))
			if err == nil {
				if !bytes.Equal(readCid.Hash(), c.Hash()) {
					return nil, fmt.Errorf("read block %s from reader for piece %s, but expected block %s", readCid, piece.PieceCid, c)
				}
				return data, nil
			}

			// CAR parse failed. Data may be a raw blob (e.g. datasegmentv2 aggregates
			// store pieces as raw bytes at offset, without CAR framing).
			// Fall back to reading exactly BlockSize bytes and verifying the hash.
			log.Debugw("CAR block parse failed, trying raw block fallback", "cid", c, "piece", piece.PieceCid, "err", err)
			if piece.BlockSize == 0 {
				return nil, fmt.Errorf("reading data for block %s from reader for piece %s: %w", c, piece.PieceCid, err)
			}

			rawAt := io.NewSectionReader(reader, int64(offset), int64(piece.BlockSize))
			data, readErr := io.ReadAll(rawAt)
			if readErr != nil {
				return nil, fmt.Errorf("reading raw block for %s from piece %s: %w", c, piece.PieceCid, readErr)
			}
			if uint64(len(data)) != piece.BlockSize {
				return nil, fmt.Errorf("short read for block %s in piece %s: got %d, want %d", c, piece.PieceCid, len(data), piece.BlockSize)
			}

			// Verify the content hash matches the requested CID
			mh, mhErr := multihash.Sum(data, c.Prefix().MhType, -1)
			if mhErr != nil {
				return nil, fmt.Errorf("hashing raw block for %s: %w", c, mhErr)
			}
			if !bytes.Equal(mh, c.Hash()) {
				return nil, fmt.Errorf("raw block hash mismatch for %s in piece %s", c, piece.PieceCid)
			}

			return data, nil
		}()
		if err != nil {
			merr = multierror.Append(merr, err)
			continue
		}

		// Serving this piece worked: make its whole block index resolvable
		// from memory for subsequent blocks (async, deduplicated).
		if ro.offsets != nil {
			ro.offsets.maybeLoad(piece.PieceCid)
		}

		return blocks.NewBlockWithCid(data, c)
	}

	if merr == nil {
		merr = format.ErrNotFound{Cid: c}
	} else {
		// All pieces failed — this will result in an HTTP 500. Log details to help diagnose
		// whether the issue is V1/V2 CID mismatch, missing sectors_meta, or sector read failure.
		pieceCids := make([]string, len(pieces))
		for i, p := range pieces {
			pieceCids[i] = fmt.Sprintf("%s(v2=%v)", p.PieceCid, commcidv2.IsPieceCidV2(p.PieceCid))
		}
		log.Warnw("all pieces failed for block retrieval (will return 500)",
			"contentCid", c, "numPieces", len(pieces), "pieces", pieceCids, "err", merr)
	}

	if ro.blockMetrics != nil {
		stats.Record(ctx, ro.blockMetrics.GetFailResponseCount.M(1))
	}

	return nil, merr
}

// getViaOffsetCache serves a block using only in-memory metadata: the offset
// cache resolves multihash -> (piece, offset, exact CAR entry length), and
// the shared piece reader does the (cached) data access. Returns ok=false on
// any failure — the caller then takes the full lookup path — and drops the
// cached piece so stale offsets self-heal.
func (ro *RemoteBlockstore) getViaOffsetCache(ctx context.Context, c cid.Cid) ([]byte, bool) {
	piece, offset, entryLen, ok := ro.offsets.lookup(string(c.Hash()))
	if !ok {
		return nil, false
	}

	data, err := func() ([]byte, error) {
		reader, size, err := ro.cpr.GetSharedPieceReader(ctx, piece, true)
		if err != nil {
			return nil, fmt.Errorf("getting piece reader for piece %s: %w", piece, err)
		}
		defer func(reader storiface.Reader) {
			_ = reader.Close()
		}(reader)

		length := int64(entryLen)
		if length == 0 {
			// last block of the piece: bound the read by the piece size
			if size <= offset {
				return nil, fmt.Errorf("cached offset %d beyond piece size %d for piece %s", offset, size, piece)
			}
			length = int64(size - offset)
		}

		readerAt := io.NewSectionReader(reader, int64(offset), length)
		readCid, data, err := util.ReadNode(bufio.NewReader(readerAt))
		if err != nil {
			return nil, fmt.Errorf("reading data for block %s from reader for piece %s: %w", c, piece, err)
		}
		if !bytes.Equal(readCid.Hash(), c.Hash()) {
			return nil, fmt.Errorf("read block %s from reader for piece %s, but expected block %s", readCid, piece, c)
		}
		return data, nil
	}()
	if err != nil {
		log.Debugw("offset cache read failed, falling back to index lookup", "cid", c, "piece", piece, "err", err)
		ro.offsets.invalidate(piece)
		return nil, false
	}

	return data, true
}

func (ro *RemoteBlockstore) Has(ctx context.Context, c cid.Cid) (bool, error) {
	if ro.blockMetrics != nil {
		stats.Record(ctx, ro.blockMetrics.HasRequestCount.M(1))
	}

	log.Debugw("Has", "cid", c)

	pieces, err := ro.idxApi.PiecesContainingMultihash(ctx, c.Hash())
	if err != nil {
		if ro.blockMetrics != nil {
			stats.Record(ctx, ro.blockMetrics.HasFailResponseCount.M(1))
		}
		return false, fmt.Errorf("getting pieces containing cid %s: %w", c, err)
	}
	has := len(pieces) > 0

	log.Debugw("Has response", "cid", c, "has", has, "error", err)
	if ro.blockMetrics != nil {
		stats.Record(ctx, ro.blockMetrics.HasSuccessResponseCount.M(1))
	}
	return has, nil
}

func (ro *RemoteBlockstore) GetSize(ctx context.Context, c cid.Cid) (int, error) {
	if ro.blockMetrics != nil {
		stats.Record(ctx, ro.blockMetrics.GetSizeRequestCount.M(1))
	}

	log.Debugw("GetSize", "cid", c)
	size, err := ro.blockstoreGetSize(ctx, c)
	log.Debugw("GetSize response", "cid", c, "size", size, "error", err)
	if err != nil && ro.blockMetrics != nil {
		stats.Record(ctx, ro.blockMetrics.GetSizeFailResponseCount.M(1))
	} else if ro.blockMetrics != nil {
		stats.Record(ctx, ro.blockMetrics.GetSizeSuccessResponseCount.M(1))
	}
	return size, err
}

// --- UNSUPPORTED BLOCKSTORE METHODS -------
func (ro *RemoteBlockstore) DeleteBlock(context.Context, cid.Cid) error {
	return errors.New("unsupported operation DeleteBlock")
}
func (ro *RemoteBlockstore) HashOnRead(_ bool) {}
func (ro *RemoteBlockstore) Put(context.Context, blocks.Block) error {
	return errors.New("unsupported operation Put")
}
func (ro *RemoteBlockstore) PutMany(context.Context, []blocks.Block) error {
	return errors.New("unsupported operation PutMany")
}
func (ro *RemoteBlockstore) AllKeysChan(ctx context.Context) (<-chan cid.Cid, error) {
	return nil, errors.New("unsupported operation AllKeysChan")
}

func (ro *RemoteBlockstore) blockstoreGetSize(ctx context.Context, c cid.Cid) (int, error) {
	// Get the pieces that contain the cid
	pieces, err := ro.idxApi.PiecesContainingMultihash(ctx, c.Hash())
	if err != nil {
		return 0, fmt.Errorf("getting pieces containing cid %s: %w", c, err)
	}
	if len(pieces) == 0 {
		// We must return ipld ErrNotFound here because that's the only type
		// that bitswap interprets as a not found error. All other error types
		// are treated as general errors.
		return 0, format.ErrNotFound{Cid: c}
	}

	var merr error

	// Iterate over all pieces in case the sector containing the first piece with the Block
	// is not unsealed
	for _, p := range pieces {
		return int(p.BlockSize), nil
	}

	return 0, merr
}

func isIdentity(c cid.Cid) (digest []byte, ok bool, err error) {
	dmh, err := multihash.Decode(c.Hash())
	if err != nil {
		return nil, false, err
	}
	ok = dmh.Code == multihash.IDENTITY
	digest = dmh.Digest
	return digest, ok, nil
}
