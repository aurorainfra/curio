package curetr

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/ipfs/go-cid"
	format "github.com/ipfs/go-ipld-format"
	"github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/curio/lib/storiface"
	"github.com/filecoin-project/curio/market/retrieval/remoteblockstore"
)

// carPiece is an in-memory CAR-framed (or raw) piece with its block layout.
type carPiece struct {
	piece    cid.Cid
	data     []byte
	blocks   []cid.Cid
	payloads [][]byte
	offsets  []uint64
}

func pieceCidFor(t *testing.T, seed string) cid.Cid {
	h, err := multihash.Sum([]byte("piece-"+seed), multihash.SHA2_256, -1)
	require.NoError(t, err)
	return cid.NewCidV1(cid.FilCommitmentUnsealed, h)
}

func payloadFor(seed string, i, size int) []byte {
	p := make([]byte, size)
	x := byte(37*i + 11)
	for j := range p {
		x = x*31 + seed[j%len(seed)] + byte(j)
		p[j] = x
	}
	return p
}

// buildCarPiece assembles CAR entries (uvarint len | cid | payload).
func buildCarPiece(t *testing.T, seed string, sizes []int) *carPiece {
	cp := &carPiece{piece: pieceCidFor(t, seed)}
	var buf bytes.Buffer
	for i, size := range sizes {
		payload := payloadFor(seed, i, size)
		mh, err := multihash.Sum(payload, multihash.SHA2_256, -1)
		require.NoError(t, err)
		c := cid.NewCidV1(cid.Raw, mh)

		cp.blocks = append(cp.blocks, c)
		cp.payloads = append(cp.payloads, payload)
		cp.offsets = append(cp.offsets, uint64(buf.Len()))

		entry := append(c.Bytes(), payload...)
		buf.Write(binary.AppendUvarint(nil, uint64(len(entry))))
		buf.Write(entry)
	}
	cp.data = buf.Bytes()
	return cp
}

// buildRawPiece concatenates payloads without CAR framing (aggregate-style).
func buildRawPiece(t *testing.T, seed string, sizes []int) *carPiece {
	cp := &carPiece{piece: pieceCidFor(t, seed)}
	var buf bytes.Buffer
	for i, size := range sizes {
		payload := payloadFor(seed, i, size)
		mh, err := multihash.Sum(payload, multihash.SHA2_256, -1)
		require.NoError(t, err)

		cp.blocks = append(cp.blocks, cid.NewCidV1(cid.Raw, mh))
		cp.payloads = append(cp.payloads, payload)
		cp.offsets = append(cp.offsets, uint64(buf.Len()))
		buf.Write(payload)
	}
	cp.data = buf.Bytes()
	return cp
}

// fakeBulkResolver resolves against a set of carPieces, in order.
type fakeBulkResolver struct {
	mu          sync.Mutex
	pieces      []*carPiece
	exactLens   bool // report exact entry lengths (offset-cache style) instead of block sizes
	notified    map[cid.Cid]int64
	invalidated map[cid.Cid]int
	resolves    int
	hinted      int
}

func newFakeResolver(pieces ...*carPiece) *fakeBulkResolver {
	return &fakeBulkResolver{
		pieces:      pieces,
		notified:    map[cid.Cid]int64{},
		invalidated: map[cid.Cid]int{},
	}
}

func (f *fakeBulkResolver) find(p *carPiece, c cid.Cid) (int, bool) {
	for i, bc := range p.blocks {
		if bytes.Equal(bc.Hash(), c.Hash()) {
			return i, true
		}
	}
	return 0, false
}

func (f *fakeBulkResolver) locFor(p *carPiece, i int) remoteblockstore.BlockLoc {
	loc := remoteblockstore.BlockLoc{Piece: p.piece, Offset: p.offsets[i]}
	if f.exactLens {
		if i+1 < len(p.offsets) {
			loc.EntryLen = p.offsets[i+1] - p.offsets[i]
		}
	} else {
		loc.BlockSize = uint64(len(p.payloads[i]))
	}
	return loc
}

func (f *fakeBulkResolver) ResolveBlock(ctx context.Context, c cid.Cid, pieceHint cid.Cid) (remoteblockstore.BlockLoc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolves++

	if pieceHint.Defined() {
		f.hinted++
		for _, p := range f.pieces {
			if p.piece != pieceHint {
				continue
			}
			if i, ok := f.find(p, c); ok {
				return f.locFor(p, i), nil
			}
		}
		return remoteblockstore.BlockLoc{}, format.ErrNotFound{Cid: c}
	}

	for pi, p := range f.pieces {
		if i, ok := f.find(p, c); ok {
			loc := f.locFor(p, i)
			for _, alt := range f.pieces[pi+1:] {
				if _, ok := f.find(alt, c); ok {
					loc.Alts = append(loc.Alts, alt.piece)
				}
			}
			return loc, nil
		}
	}
	return remoteblockstore.BlockLoc{}, format.ErrNotFound{Cid: c}
}

func (f *fakeBulkResolver) BulkServeNotify(piece cid.Cid, blocksServed int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notified[piece] += blocksServed
}

func (f *fakeBulkResolver) BulkInvalidate(piece cid.Cid) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalidated[piece]++
}

// fakeReaderSource serves carPiece bytes and records reads.
type fakeReaderSource struct {
	mu       sync.Mutex
	pieces   map[cid.Cid][]byte
	failOpen map[cid.Cid]bool
	opens    []cid.Cid
	readAts  []int // sizes of backing ReadAt calls
}

func newFakeReaderSource(pieces ...*carPiece) *fakeReaderSource {
	f := &fakeReaderSource{pieces: map[cid.Cid][]byte{}, failOpen: map[cid.Cid]bool{}}
	for _, p := range pieces {
		f.pieces[p.piece] = p.data
	}
	return f
}

func (f *fakeReaderSource) GetSharedPieceReader(ctx context.Context, pieceCid cid.Cid, retrieval bool) (storiface.Reader, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens = append(f.opens, pieceCid)
	if f.failOpen[pieceCid] {
		return nil, 0, fmt.Errorf("no unsealed copy of %s", pieceCid)
	}
	data, ok := f.pieces[pieceCid]
	if !ok {
		return nil, 0, fmt.Errorf("unknown piece %s", pieceCid)
	}
	return &fakeReader{Reader: bytes.NewReader(data), src: f}, uint64(len(data)), nil
}

type fakeReader struct {
	*bytes.Reader
	src *fakeReaderSource
}

func (r *fakeReader) ReadAt(p []byte, off int64) (int, error) {
	r.src.mu.Lock()
	r.src.readAts = append(r.src.readAts, len(p))
	r.src.mu.Unlock()
	return r.Reader.ReadAt(p, off)
}

func (r *fakeReader) Close() error { return nil }

type bulkFrame struct {
	idx    uint64
	status uint64
	data   []byte
}

// doBulk posts the request and decodes the whole frame stream.
func doBulk(t *testing.T, h *BulkHandler, req BulkRequest) (int, []bulkFrame) {
	var body bytes.Buffer
	require.NoError(t, req.MarshalCBOR(&body))

	r := httptest.NewRequest("POST", BulkBlocksPath, &body)
	w := httptest.NewRecorder()
	h.ServeBlocks(w, r)

	if w.Code != 200 {
		return w.Code, nil
	}
	require.Equal(t, BulkContentType, w.Header().Get("Content-Type"))

	var frames []bulkFrame
	br := bufio.NewReader(w.Body)
	for {
		idx, status, data, err := ReadBulkFrame(br)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		frames = append(frames, bulkFrame{idx, status, data})
	}
	return w.Code, frames
}

// requireServed asserts every requested block came back ok with the right
// payload, followed by the end frame.
func requireServed(t *testing.T, frames []bulkFrame, want map[uint64][]byte, nBlocks int) {
	got := map[uint64][]byte{}
	for _, f := range frames[:len(frames)-1] {
		require.Equal(t, uint64(BulkStatusOk), f.status, "frame %d", f.idx)
		_, dup := got[f.idx]
		require.False(t, dup, "duplicate frame %d", f.idx)
		got[f.idx] = f.data
	}
	require.Len(t, got, len(want))
	for idx, data := range want {
		require.Equal(t, data, got[idx], "block %d", idx)
	}

	end := frames[len(frames)-1]
	require.Equal(t, bulkFrame{uint64(nBlocks), BulkStatusOk, nil}, end)
}

// requireWindow asserts no frame was emitted more than window ahead of the
// lowest not-yet-emitted index.
func requireWindow(t *testing.T, frames []bulkFrame, nBlocks int, window uint64) {
	emitted := make([]bool, nBlocks)
	low := 0
	for _, f := range frames {
		if f.idx == uint64(nBlocks) {
			continue // end frame
		}
		require.Less(t, f.idx, uint64(low)+window,
			"frame %d emitted more than %d ahead of lowest un-emitted %d", f.idx, window, low)
		emitted[f.idx] = true
		for low < nBlocks && emitted[low] {
			low++
		}
	}
}

func mkBulkHandler(res BulkResolver, prs PieceReaderSource) *BulkHandler {
	return NewBulkHandler(res, prs, nil)
}

func TestBulkOrderedRun(t *testing.T) {
	sizes := make([]int, 20)
	for i := range sizes {
		sizes[i] = 950 + i*7
	}
	cp := buildCarPiece(t, "ordered", sizes)
	res := newFakeResolver(cp)
	src := newFakeReaderSource(cp)

	req := BulkRequest{Blocks: cp.blocks}
	want := map[uint64][]byte{}
	for i, p := range cp.payloads {
		want[uint64(i)] = p
	}

	code, frames := doBulk(t, mkBulkHandler(res, src), req)
	require.Equal(t, 200, code)
	requireServed(t, frames, want, len(cp.blocks))

	// in-order request over one piece: a single merged range read, one
	// reader open, one serve notification carrying the block count
	require.Len(t, src.readAts, 1)
	require.Len(t, src.opens, 1)
	require.Equal(t, int64(20), res.notified[cp.piece])
}

func TestBulkExactLens(t *testing.T) {
	// offset-cache style resolution: exact entry lengths, no block sizes
	cp := buildCarPiece(t, "exact", []int{1000, 1000, 1000, 1000})
	res := newFakeResolver(cp)
	res.exactLens = true
	src := newFakeReaderSource(cp)

	want := map[uint64][]byte{}
	for i, p := range cp.payloads {
		want[uint64(i)] = p
	}
	code, frames := doBulk(t, mkBulkHandler(res, src), BulkRequest{Blocks: cp.blocks})
	require.Equal(t, 200, code)
	requireServed(t, frames, want, len(cp.blocks))
	require.Len(t, src.readAts, 1)
}

func TestBulkWindowSlicing(t *testing.T) {
	cp := buildCarPiece(t, "windowed", []int{800, 800, 800, 800, 800, 800, 800, 800, 800, 800})
	res := newFakeResolver(cp)
	src := newFakeReaderSource(cp)

	req := BulkRequest{Blocks: cp.blocks, Window: 4}
	code, frames := doBulk(t, mkBulkHandler(res, src), req)
	require.Equal(t, 200, code)

	want := map[uint64][]byte{}
	for i, p := range cp.payloads {
		want[uint64(i)] = p
	}
	requireServed(t, frames, want, len(cp.blocks))
	requireWindow(t, frames, len(cp.blocks), 4)

	// 10 blocks / window 4 = 3 slices = 3 merged reads, but still 1 open
	require.Len(t, src.readAts, 3)
	require.Len(t, src.opens, 1)
}

func TestBulkReversedOrder(t *testing.T) {
	cp := buildCarPiece(t, "reversed", []int{700, 700, 700, 700, 700, 700, 700, 700})
	res := newFakeResolver(cp)
	src := newFakeReaderSource(cp)

	n := len(cp.blocks)
	req := BulkRequest{Window: 3}
	want := map[uint64][]byte{}
	for i := 0; i < n; i++ {
		req.Blocks = append(req.Blocks, cp.blocks[n-1-i])
		want[uint64(i)] = cp.payloads[n-1-i]
	}

	code, frames := doBulk(t, mkBulkHandler(res, src), req)
	require.Equal(t, 200, code)
	requireServed(t, frames, want, n)
	requireWindow(t, frames, n, 3)
}

func TestBulkMixedPieces(t *testing.T) {
	p1 := buildCarPiece(t, "mixed-a", []int{600, 600, 600})
	p2 := buildCarPiece(t, "mixed-b", []int{600, 600, 600})
	res := newFakeResolver(p1, p2)
	src := newFakeReaderSource(p1, p2)

	req := BulkRequest{}
	want := map[uint64][]byte{}
	for i := 0; i < 3; i++ { // interleave pieces
		req.Blocks = append(req.Blocks, p1.blocks[i], p2.blocks[i])
		want[uint64(2*i)] = p1.payloads[i]
		want[uint64(2*i+1)] = p2.payloads[i]
	}

	code, frames := doBulk(t, mkBulkHandler(res, src), req)
	require.Equal(t, 200, code)
	requireServed(t, frames, want, len(req.Blocks))

	// one merged range per piece, single slice
	require.Len(t, src.readAts, 2)
	require.Len(t, src.opens, 2)
}

func TestBulkNotFoundAndDenied(t *testing.T) {
	cp := buildCarPiece(t, "partial", []int{500, 500})
	res := newFakeResolver(cp)
	src := newFakeReaderSource(cp)

	unknown := cid.NewCidV1(cid.Raw, mustMh(t, []byte("nope")))
	denied := cp.blocks[1]

	h := NewBulkHandler(res, src, func(c cid.Cid) error {
		if c == denied {
			return fmt.Errorf("denied")
		}
		return nil
	})

	req := BulkRequest{Blocks: []cid.Cid{cp.blocks[0], unknown, denied}}
	code, frames := doBulk(t, h, req)
	require.Equal(t, 200, code)
	require.Len(t, frames, 4)

	byIdx := map[uint64]bulkFrame{}
	for _, f := range frames {
		byIdx[f.idx] = f
	}
	require.Equal(t, uint64(BulkStatusOk), byIdx[0].status)
	require.Equal(t, cp.payloads[0], byIdx[0].data)
	require.Equal(t, uint64(BulkStatusNotFound), byIdx[1].status)
	require.Equal(t, uint64(BulkStatusError), byIdx[2].status)
	require.Equal(t, uint64(BulkStatusOk), byIdx[3].status) // end frame
	require.Empty(t, byIdx[3].data)
}

func TestBulkAltPieceRetry(t *testing.T) {
	// same blocks in two pieces; the preferred piece has no readable copy
	sizes := []int{900, 900, 900}
	p1 := buildCarPiece(t, "altsrc", sizes)
	p2 := &carPiece{ // same content under a different piece cid
		piece:    pieceCidFor(t, "altdst"),
		data:     p1.data,
		blocks:   p1.blocks,
		payloads: p1.payloads,
		offsets:  p1.offsets,
	}
	res := newFakeResolver(p1, p2)
	src := newFakeReaderSource(p1, p2)
	src.failOpen[p1.piece] = true

	want := map[uint64][]byte{}
	for i, p := range p1.payloads {
		want[uint64(i)] = p
	}
	code, frames := doBulk(t, mkBulkHandler(res, src), BulkRequest{Blocks: p1.blocks})
	require.Equal(t, 200, code)
	requireServed(t, frames, want, len(p1.blocks))

	require.Equal(t, []cid.Cid{p1.piece, p2.piece}, src.opens)
	require.Equal(t, int64(3), res.notified[p2.piece])
}

func TestBulkStaleOffsetError(t *testing.T) {
	// resolver reports block 0 at block 1's offset: parse succeeds, cid
	// mismatch -> error frame + piece invalidated
	cp := buildCarPiece(t, "stale", []int{400, 400})
	res := newFakeResolver(cp)
	res.exactLens = true
	cp.offsets[0], cp.offsets[1] = cp.offsets[1], cp.offsets[0]
	src := newFakeReaderSource(cp)

	code, frames := doBulk(t, mkBulkHandler(res, src), BulkRequest{Blocks: cp.blocks[:1]})
	require.Equal(t, 200, code)
	require.Len(t, frames, 2)
	require.Equal(t, uint64(BulkStatusError), frames[0].status)
	require.Equal(t, 1, res.invalidated[cp.piece])
}

func TestBulkRawAggregate(t *testing.T) {
	cp := buildRawPiece(t, "raw", []int{512, 1024, 2048})
	res := newFakeResolver(cp)
	src := newFakeReaderSource(cp)

	want := map[uint64][]byte{}
	for i, p := range cp.payloads {
		want[uint64(i)] = p
	}
	code, frames := doBulk(t, mkBulkHandler(res, src), BulkRequest{Blocks: cp.blocks})
	require.Equal(t, 200, code)
	requireServed(t, frames, want, len(cp.blocks))
}

func TestBulkIdentityCid(t *testing.T) {
	res := newFakeResolver()
	src := newFakeReaderSource()

	digest := []byte("inline data")
	mh, err := multihash.Sum(digest, multihash.IDENTITY, -1)
	require.NoError(t, err)
	idCid := cid.NewCidV1(cid.Raw, mh)

	code, frames := doBulk(t, mkBulkHandler(res, src), BulkRequest{Blocks: []cid.Cid{idCid}})
	require.Equal(t, 200, code)
	require.Len(t, frames, 2)
	require.Equal(t, uint64(BulkStatusOk), frames[0].status)
	require.Equal(t, digest, frames[0].data)
	require.Zero(t, res.resolves)
}

func TestBulkPieceHints(t *testing.T) {
	cp := buildCarPiece(t, "hinted", []int{800, 800, 800})
	res := newFakeResolver(cp)
	src := newFakeReaderSource(cp)

	req := BulkRequest{Blocks: cp.blocks, Pieces: []cid.Cid{cp.piece, cp.piece, cp.piece}}
	want := map[uint64][]byte{}
	for i, p := range cp.payloads {
		want[uint64(i)] = p
	}

	code, frames := doBulk(t, mkBulkHandler(res, src), req)
	require.Equal(t, 200, code)
	requireServed(t, frames, want, len(cp.blocks))
	require.Equal(t, 3, res.hinted)
	require.Len(t, src.readAts, 1)
}

func TestBulkValidation(t *testing.T) {
	cp := buildCarPiece(t, "valid", []int{100})
	h := mkBulkHandler(newFakeResolver(cp), newFakeReaderSource(cp))

	tooMany := make([]cid.Cid, BulkMaxBlocks+1)
	for i := range tooMany {
		tooMany[i] = cp.blocks[0]
	}

	for name, req := range map[string]BulkRequest{
		"empty":           {},
		"too many blocks": {Blocks: tooMany},
		"window too big":  {Blocks: cp.blocks, Window: BulkMaxWindow + 1},
		"pieces mismatch": {Blocks: cp.blocks, Pieces: []cid.Cid{cp.piece, cp.piece}},
	} {
		code, _ := doBulk(t, h, req)
		require.Equal(t, 400, code, name)
	}
}

func mustMh(t *testing.T, data []byte) []byte {
	mh, err := multihash.Sum(data, multihash.SHA2_256, -1)
	require.NoError(t, err)
	return mh
}
