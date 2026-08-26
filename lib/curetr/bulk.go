package curetr

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/ipfs/go-cid"
	format "github.com/ipfs/go-ipld-format"
	"github.com/multiformats/go-multihash"
	"go.opencensus.io/stats"
	"golang.org/x/sync/errgroup"

	"github.com/filecoin-project/curio/deps/config"
	"github.com/filecoin-project/curio/lib/storiface"
	"github.com/filecoin-project/curio/market/retrieval/remoteblockstore"
)

// Bulk serving turns an ordered batch of block cids into a few large
// sequential piece reads instead of one random read per block:
//
//  1. the request is processed in consecutive slices of `window` indices,
//     which satisfies the frame-window rule by construction (everything
//     before the current slice is already emitted, and nothing past it is);
//  2. within a slice, blocks resolve to (piece, offset, length-bound) in
//     parallel, then bucket by piece and sort by offset;
//  3. members closer than bulkMergeGap merge into one range, read with a
//     single big ReadAt (one ranged request to the storage node, one
//     sequential disk span), and blocks are parsed out of the span.
//
// Since clients send blocks in consumption order and a contiguous write
// run is stored in offset order, real traffic degenerates to one merged
// range per slice.
//
// Everything here is tunable at runtime through
// config.BulkRetrievalConfig (HTTP.BulkRetrieval); the constants are the
// defaults, used when the handler has no config (tests) or a value is out
// of range.
const (
	// bulkMergeGap: adjacent block reads merge into one range if the gap
	// between them is at most this many bytes (over-read is cheaper than a
	// separate ranged request / disk seek).
	bulkMergeGap = 1 << 20
	// bulkMaxRange caps a single merged range, and thereby the read buffer
	// and one backing ReadAt (one ranged storage-node request).
	bulkMaxRange = 32 << 20
	// bulkUnknownTailBound bounds the last entry of a bucket when neither
	// the exact entry length nor the block size is known (hinted resolve);
	// every non-last member is bounded by its successor's offset.
	bulkUnknownTailBound = 4 << 20
	// bulkResolveParallelism bounds concurrent per-block index resolves
	// within a slice.
	bulkResolveParallelism = 64
)

// bulkLimits is one request's consistent snapshot of the dynamic config
// (values must not change mid-request).
type bulkLimits struct {
	maxBlocks     int
	maxWindow     uint64
	defaultWindow uint64
	advStreams    int
	mergeGap      uint64
	maxRange      uint64
	resolvePar    int
}

// dynVal reads a dynamic config value, falling back to def when the field
// is unset or below minV.
func dynVal(d *config.Dynamic[int], def, minV int) int {
	if d == nil {
		return def
	}
	if v := d.Get(); v >= minV {
		return v
	}
	return def
}

func (h *BulkHandler) limits() bulkLimits {
	l := bulkLimits{
		maxBlocks:     BulkMaxBlocks,
		maxWindow:     BulkMaxWindow,
		defaultWindow: BulkDefaultWindow,
		advStreams:    BulkAdvertisedStreams,
		mergeGap:      bulkMergeGap,
		maxRange:      bulkMaxRange,
		resolvePar:    bulkResolveParallelism,
	}
	if h.cfg != nil {
		l.maxBlocks = dynVal(h.cfg.MaxBlocks, BulkMaxBlocks, 1)
		l.maxWindow = uint64(dynVal(h.cfg.MaxWindow, BulkMaxWindow, 1))
		l.defaultWindow = uint64(dynVal(h.cfg.DefaultWindow, BulkDefaultWindow, 1))
		l.advStreams = dynVal(h.cfg.AdvertisedMaxStreams, BulkAdvertisedStreams, 1)
		l.mergeGap = uint64(dynVal(h.cfg.MergeGapKiB, bulkMergeGap>>10, 0)) << 10
		l.maxRange = uint64(dynVal(h.cfg.MaxRangeMiB, bulkMaxRange>>20, 1)) << 20
		l.resolvePar = dynVal(h.cfg.ResolveParallelism, bulkResolveParallelism, 1)
	}
	if l.defaultWindow > l.maxWindow {
		l.defaultWindow = l.maxWindow
	}
	return l
}

// maxRequestBody bounds the POST body: cids are ~40 bytes, hints double
// that, plus cbor framing slop.
func (l bulkLimits) maxRequestBody() int64 {
	return max(1<<20, int64(l.maxBlocks)*128)
}

// BulkResolver resolves block cids to piece locations; implemented by
// remoteblockstore.RemoteBlockstore.
type BulkResolver interface {
	ResolveBlock(ctx context.Context, c cid.Cid, pieceHint cid.Cid) (remoteblockstore.BlockLoc, error)
	BulkServeNotify(piece cid.Cid, blocksServed int64)
	BulkInvalidate(piece cid.Cid)
}

// PieceReaderSource hands out shared piece readers; implemented by
// cachedreader.CachedPieceReader.
type PieceReaderSource interface {
	GetSharedPieceReader(ctx context.Context, pieceCid cid.Cid, retrieval bool) (storiface.Reader, uint64, error)
}

// BulkHandler serves POST /aurora/bulk/v0/blocks and GET /aurora/bulk/v0/info.
type BulkHandler struct {
	res BulkResolver
	prs PieceReaderSource
	// deny returns a non-nil error for blocks that must not be served
	// (denylisted, or the denylist is not loaded yet); nil disables checks.
	deny func(cid.Cid) error
	// cfg holds the live-tunable limits; nil uses the package defaults.
	cfg *config.BulkRetrievalConfig
}

func NewBulkHandler(res BulkResolver, prs PieceReaderSource, deny func(cid.Cid) error, cfg *config.BulkRetrievalConfig) *BulkHandler {
	return &BulkHandler{res: res, prs: prs, deny: deny, cfg: cfg}
}

// Enabled reports whether the endpoint should be served at all:
// MaxConcurrentStreams == 0 is the operator off switch (routes 404 so
// clients negative-cache the capability and fall back).
func (h *BulkHandler) Enabled() bool {
	return h.cfg == nil || h.cfg.MaxConcurrentStreams == nil || h.cfg.MaxConcurrentStreams.Get() != 0
}

// ServeInfo answers the capability probe.
func (h *BulkHandler) ServeInfo(w http.ResponseWriter, _ *http.Request) {
	lim := h.limits()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"version":    BulkProtoVersion,
		"addressing": []string{"cid"},
		"maxBlocks":  lim.maxBlocks,
		"maxWindow":  lim.maxWindow,
		"maxStreams": lim.advStreams,
	})
}

// ServeBlocks streams the requested blocks back as bulk frames.
func (h *BulkHandler) ServeBlocks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	start := time.Now()

	stats.Record(ctx, BulkInflight.M(bulkInflight.Add(1)))
	defer func() {
		stats.Record(context.Background(), BulkInflight.M(bulkInflight.Add(-1)),
			BulkRequestDurationMs.M(msSince(start)))
	}()
	stats.Record(ctx, BulkRequestCount.M(1))

	lim := h.limits()

	var req BulkRequest
	if err := req.UnmarshalCBOR(bufio.NewReader(http.MaxBytesReader(w, r.Body, lim.maxRequestBody()))); err != nil {
		stats.Record(ctx, Bulk400ResponseCount.M(1))
		http.Error(w, fmt.Sprintf("decoding request: %s", err), http.StatusBadRequest)
		return
	}
	if err := validateBulkRequest(&req, lim); err != nil {
		stats.Record(ctx, Bulk400ResponseCount.M(1))
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Window == 0 {
		req.Window = lim.defaultWindow
	}
	stats.Record(ctx, BulkBlocksRequested.M(int64(len(req.Blocks))))

	w.Header().Set("Content-Type", BulkContentType)
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	s := &bulkServe{
		h:       h,
		lim:     lim,
		req:     &req,
		w:       w,
		flusher: flusher,
		readers: map[cid.Cid]*pieceHandle{},
		served:  map[cid.Cid]int64{},
	}
	s.run(ctx)
}

func validateBulkRequest(req *BulkRequest, lim bulkLimits) error {
	if len(req.Blocks) == 0 || len(req.Blocks) > lim.maxBlocks {
		return fmt.Errorf("blocks count %d out of range [1, %d]", len(req.Blocks), lim.maxBlocks)
	}
	if req.Window > lim.maxWindow {
		return fmt.Errorf("window %d exceeds maximum %d", req.Window, lim.maxWindow)
	}
	if len(req.Pieces) != 0 && len(req.Pieces) != len(req.Blocks) {
		return fmt.Errorf("pieces count %d does not match blocks count %d", len(req.Pieces), len(req.Blocks))
	}
	return nil
}

// pieceHandle is one shared piece reader held for the whole request (the
// single-block path re-acquires and releases the refcounted reader per
// block; a bulk request does it once per piece).
type pieceHandle struct {
	reader storiface.Reader
	size   uint64 // raw (unpadded) piece size
	err    error  // open failure, cached so later slices fail fast
}

// bulkMember is one requested block placed in a piece.
type bulkMember struct {
	reqIdx    int
	c         cid.Cid
	offset    uint64
	entryLen  uint64    // exact CAR entry length, 0 = unknown
	blockSize uint64    // payload size from the index, 0 = unknown
	alts      []cid.Cid // untried alternate pieces
	bound     uint64    // computed upper bound on the entry length
}

type failedMember struct {
	m   bulkMember
	err error
}

type bulkServe struct {
	h       *BulkHandler
	lim     bulkLimits
	req     *BulkRequest
	w       http.ResponseWriter
	flusher http.Flusher

	readers map[cid.Cid]*pieceHandle
	served  map[cid.Cid]int64 // ok frames per piece, for BulkServeNotify

	writeErr error // sticky client-write failure: abort the request
}

func (s *bulkServe) run(ctx context.Context) {
	defer s.closeReaders()

	n := len(s.req.Blocks)
	window := int(s.req.Window)

	for base := 0; base < n && s.writeErr == nil && ctx.Err() == nil; base += window {
		s.serveSlice(ctx, base, min(base+window, n))
		s.flush()
	}

	if s.writeErr == nil && ctx.Err() == nil {
		s.emitFrame(uint64(n), BulkStatusOk, nil) // end frame
		s.flush()
	}

	for piece, cnt := range s.served {
		s.h.res.BulkServeNotify(piece, cnt)
	}
}

// serveSlice resolves and serves request indices [base, end). Everything
// emitted here is within the window rule: all lower indices were emitted by
// previous slices and no higher index is touched before this one returns.
func (s *bulkServe) serveSlice(ctx context.Context, base, end int) {
	type slot struct {
		identity []byte // identity-cid payload, served without any lookup
		loc      remoteblockstore.BlockLoc
		resolved bool
		err      error
	}
	slots := make([]slot, end-base)

	resolveStart := time.Now()
	var eg errgroup.Group
	eg.SetLimit(s.lim.resolvePar)
	for i := base; i < end; i++ {
		c := s.req.Blocks[i]
		sl := &slots[i-base]

		if s.h.deny != nil {
			if err := s.h.deny(c); err != nil {
				sl.err = err
				continue
			}
		}
		if dmh, err := multihash.Decode(c.Hash()); err == nil && dmh.Code == multihash.IDENTITY {
			sl.identity = dmh.Digest
			continue
		}

		var hint cid.Cid
		if len(s.req.Pieces) > 0 {
			hint = s.req.Pieces[i]
		}
		eg.Go(func() error {
			loc, err := s.h.res.ResolveBlock(ctx, c, hint)
			if err != nil {
				sl.err = err
			} else {
				sl.loc = loc
				sl.resolved = true
			}
			return nil
		})
	}
	_ = eg.Wait()
	stats.Record(ctx, BulkResolveDurationMs.M(msSince(resolveStart)))

	buckets := map[cid.Cid][]bulkMember{}
	for i := range slots {
		reqIdx := base + i
		switch {
		case s.writeErr != nil:
			return
		case slots[i].identity != nil:
			s.emitOk(reqIdx, slots[i].identity)
		case slots[i].err != nil:
			s.emitFail(reqIdx, slots[i].err)
		case slots[i].resolved:
			loc := slots[i].loc
			buckets[loc.Piece] = append(buckets[loc.Piece], bulkMember{
				reqIdx:    reqIdx,
				c:         s.req.Blocks[reqIdx],
				offset:    loc.Offset,
				entryLen:  loc.EntryLen,
				blockSize: loc.BlockSize,
				alts:      loc.Alts,
			})
		}
	}

	s.processBuckets(ctx, buckets, true)
}

// processBuckets reads every bucket; members that fail with untried
// alternate pieces are re-resolved and re-bucketed once (allowAlts guards
// against loops), the rest get failure frames.
func (s *bulkServe) processBuckets(ctx context.Context, buckets map[cid.Cid][]bulkMember, allowAlts bool) {
	for piece, members := range buckets {
		failed := s.readBucket(ctx, piece, members)
		if len(failed) == 0 {
			continue
		}

		retry := map[cid.Cid][]bulkMember{}
		for _, fm := range failed {
			if s.writeErr != nil {
				return
			}

			var requeued bool
			for allowAlts && len(fm.m.alts) > 0 {
				alt := fm.m.alts[0]
				fm.m.alts = fm.m.alts[1:]
				loc, err := s.h.res.ResolveBlock(ctx, fm.m.c, alt)
				if err != nil {
					continue
				}
				nm := fm.m
				nm.offset = loc.Offset
				nm.entryLen = loc.EntryLen
				// blockSize is a property of the block, not the piece; keep
				// the original when the hinted resolve doesn't know it
				if loc.BlockSize > 0 {
					nm.blockSize = loc.BlockSize
				}
				retry[loc.Piece] = append(retry[loc.Piece], nm)
				requeued = true
				break
			}
			if !requeued {
				s.emitFail(fm.m.reqIdx, fm.err)
			}
		}

		if len(retry) > 0 {
			s.processBuckets(ctx, retry, false)
		}
	}
}

// readBucket serves one piece's members via merged sequential reads. Ok
// frames are emitted inline; everything that could not be served comes back
// with its error.
func (s *bulkServe) readBucket(ctx context.Context, piece cid.Cid, members []bulkMember) []failedMember {
	ph := s.getReader(ctx, piece)
	if ph.err != nil {
		failed := make([]failedMember, 0, len(members))
		for _, m := range members {
			failed = append(failed, failedMember{m: m, err: fmt.Errorf("getting piece reader for %s: %w", piece, ph.err)})
		}
		return failed
	}

	sort.Slice(members, func(i, j int) bool { return members[i].offset < members[j].offset })

	var failed []failedMember

	// upper-bound each member's CAR entry: exact length when known, else
	// the next member's offset (entries never overlap), else the indexed
	// payload size plus framing slop, else the protocol tail bound; always
	// clamped to the piece end.
	ok := members[:0]
	for i, m := range members {
		bound := m.entryLen
		if bound == 0 && m.blockSize > 0 {
			bound = m.blockSize + remoteblockstore.MaxCarBlockPrefixSize
		}
		if i+1 < len(members) {
			if d := members[i+1].offset - m.offset; d > 0 && (bound == 0 || d < bound) {
				bound = d
			}
		}
		if bound == 0 || bound > s.lim.maxRange {
			// also a hard cap: a corrupt indexed size must not translate
			// into an unbounded read buffer
			bound = min(bulkUnknownTailBound, s.lim.maxRange)
		}
		if m.offset >= ph.size {
			failed = append(failed, failedMember{m: m, err: fmt.Errorf("offset %d beyond piece size %d", m.offset, ph.size)})
			s.h.res.BulkInvalidate(piece)
			continue
		}
		if m.offset+bound > ph.size {
			bound = ph.size - m.offset
		}
		m.bound = bound
		ok = append(ok, m)
	}
	members = ok

	for start := 0; start < len(members); {
		// grow the merged range while gaps stay small and the buffer bounded
		rangeStart := members[start].offset
		rangeEnd := rangeStart + members[start].bound
		next := start + 1
		for next < len(members) &&
			members[next].offset <= rangeEnd+s.lim.mergeGap &&
			members[next].offset+members[next].bound-rangeStart <= s.lim.maxRange {
			if e := members[next].offset + members[next].bound; e > rangeEnd {
				rangeEnd = e
			}
			next++
		}

		failed = append(failed, s.readRange(ctx, piece, ph, members[start:next], rangeStart, rangeEnd)...)
		start = next

		if s.writeErr != nil || ctx.Err() != nil {
			return failed
		}
	}

	return failed
}

// readRange does one big ReadAt covering members and emits their blocks.
func (s *bulkServe) readRange(ctx context.Context, piece cid.Cid, ph *pieceHandle, members []bulkMember, rangeStart, rangeEnd uint64) []failedMember {
	readStart := time.Now()
	buf := make([]byte, rangeEnd-rangeStart)
	n, err := ph.reader.ReadAt(buf, int64(rangeStart))
	stats.Record(ctx,
		BulkReadDurationMs.M(msSince(readStart)),
		BulkRangeBytes.M(int64(n)),
		BulkRangeBlocks.M(int64(len(members))))
	if n == 0 && err != nil {
		failed := make([]failedMember, 0, len(members))
		for _, m := range members {
			failed = append(failed, failedMember{m: m, err: fmt.Errorf("reading range [%d, %d) of piece %s: %w", rangeStart, rangeEnd, piece, err)})
		}
		return failed
	}
	buf = buf[:n]

	var failed []failedMember
	for _, m := range members {
		if s.writeErr != nil {
			return failed
		}

		rel := m.offset - rangeStart
		if rel >= uint64(len(buf)) {
			failed = append(failed, failedMember{m: m, err: fmt.Errorf("short range read of piece %s: %w", piece, err)})
			continue
		}

		data, perr := parseBulkEntry(buf[rel:], m)
		if perr != nil {
			// a block that does not parse/verify at its resolved offset
			// means the index (or cached offsets) are stale for this piece
			s.h.res.BulkInvalidate(piece)
			failed = append(failed, failedMember{m: m, err: perr})
			continue
		}
		s.emitOk(m.reqIdx, data)
		s.served[piece]++
	}
	return failed
}

// parseBulkEntry extracts and verifies member m's block from b (which
// starts at the member's offset). CAR entry framing first, then the raw
// fallback for aggregate pieces that store payload bytes unframed.
func parseBulkEntry(b []byte, m bulkMember) ([]byte, error) {
	data, carErr := func() ([]byte, error) {
		entryLen, vn := binary.Uvarint(b)
		if vn <= 0 {
			return nil, fmt.Errorf("invalid CAR entry varint for %s", m.c)
		}
		if entryLen == 0 || entryLen > uint64(len(b)-vn) {
			return nil, fmt.Errorf("CAR entry for %s out of bounds: len %d, have %d", m.c, entryLen, len(b)-vn)
		}
		entry := b[vn : vn+int(entryLen)]
		cidLen, readCid, err := cid.CidFromBytes(entry)
		if err != nil {
			return nil, fmt.Errorf("parsing CAR entry cid for %s: %w", m.c, err)
		}
		if !bytes.Equal(readCid.Hash(), m.c.Hash()) {
			return nil, fmt.Errorf("read block %s, expected %s", readCid, m.c)
		}
		return entry[cidLen:], nil
	}()
	if carErr == nil {
		return data, nil
	}

	// raw fallback (mirrors the single-block path): datasegment aggregates
	// store payload bytes at offset without CAR framing
	if m.blockSize == 0 || uint64(len(b)) < m.blockSize {
		return nil, carErr
	}
	raw := b[:m.blockSize]
	mh, err := multihash.Sum(raw, m.c.Prefix().MhType, -1)
	if err != nil {
		return nil, fmt.Errorf("hashing raw block for %s: %w", m.c, err)
	}
	if !bytes.Equal(mh, m.c.Hash()) {
		return nil, fmt.Errorf("raw block hash mismatch for %s (CAR parse: %w)", m.c, carErr)
	}
	return raw, nil
}

func (s *bulkServe) getReader(ctx context.Context, piece cid.Cid) *pieceHandle {
	if ph, ok := s.readers[piece]; ok {
		return ph
	}
	reader, size, err := s.h.prs.GetSharedPieceReader(ctx, piece, true)
	ph := &pieceHandle{reader: reader, size: size, err: err}
	s.readers[piece] = ph
	return ph
}

func (s *bulkServe) closeReaders() {
	for _, ph := range s.readers {
		if ph.err == nil {
			_ = ph.reader.Close()
		}
	}
}

func (s *bulkServe) emitOk(reqIdx int, data []byte) {
	s.emitFrame(uint64(reqIdx), BulkStatusOk, data)
	if s.writeErr == nil {
		stats.Record(context.Background(),
			BulkFramesOk.M(1),
			BulkBytesServedCount.M(int64(len(data))))
	}
}

func (s *bulkServe) emitFail(reqIdx int, err error) {
	if format.IsNotFound(err) {
		s.emitFrame(uint64(reqIdx), BulkStatusNotFound, nil)
		if s.writeErr == nil {
			stats.Record(context.Background(), BulkFramesNotFound.M(1))
		}
		return
	}
	log.Debugw("bulk block failed", "idx", reqIdx, "cid", s.req.Blocks[reqIdx], "err", err)
	s.emitFrame(uint64(reqIdx), BulkStatusError, nil)
	if s.writeErr == nil {
		stats.Record(context.Background(), BulkFramesError.M(1))
	}
}

func (s *bulkServe) emitFrame(idx uint64, status uint64, data []byte) {
	if s.writeErr != nil {
		return
	}
	s.writeErr = WriteBulkFrame(s.w, idx, status, data)
}

func (s *bulkServe) flush() {
	if s.flusher != nil && s.writeErr == nil {
		s.flusher.Flush()
	}
}

func msSince(t time.Time) float64 {
	return float64(time.Since(t).Microseconds()) / 1000
}
