package curetr

// Bulk retrieval wire protocol (v1). The gateway posts an ordered batch of
// block cids and the server streams the blocks back from mostly-sequential
// piece reads. Spec: ribs doc/curio-bulk-retrieval.md.
//
// Request: POST /aurora/bulk/v0/blocks, body is a dag-cbor map (BulkRequest).
// Response: 200 with a stream of frames
//
//	uvarint index | uvarint status | uvarint length | length bytes
//
// terminated by an end frame with index == len(blocks), status == ok,
// length == 0. Frames may be emitted out of request order, but never more
// than the request window ahead of the lowest not-yet-emitted index.

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/ipfs/go-cid"
)

// BulkRequest is the dag-cbor body of POST /aurora/bulk/v0/blocks. Blocks
// are in the order the client will consume them; within a contiguous write
// run that is also piece offset order, which is what makes merged
// sequential reads possible server-side. Pieces optionally carries a piece
// cid hint per block (parallel to Blocks), letting the server skip
// per-block piece resolution.
type BulkRequest struct {
	Blocks []cid.Cid `cborgen:"blocks"`
	Window uint64    `cborgen:"window"`
	Pieces []cid.Cid `cborgen:"pieces"`
}

// Frame status codes.
const (
	BulkStatusOk       = 0
	BulkStatusNotFound = 1 // not indexed on this provider
	BulkStatusError    = 2 // indexed but unreadable (no unsealed copy, read/verify failure, denied)
)

// Protocol limits, advertised by GET /aurora/bulk/v0/info.
const (
	BulkProtoVersion      = 1
	BulkMaxBlocks         = 4096
	BulkMaxWindow         = 256
	BulkDefaultWindow     = 64 // window == 0 in a request means this
	BulkAdvertisedStreams = 8  // advisory per-client concurrent stream cap

	// BulkMaxFrameData bounds a single ok frame's payload on the read side;
	// far above any real block size (curio blocks are ≤ a few MiB).
	BulkMaxFrameData = 64 << 20
)

// BulkContentType is the response Content-Type of the frame stream.
const BulkContentType = "application/vnd.aurora.bulk.v0"

// BulkBlocksPath and BulkInfoPath are the endpoint paths.
const (
	BulkBlocksPath = "/aurora/bulk/v0/blocks"
	BulkInfoPath   = "/aurora/bulk/v0/info"
)

// WriteBulkFrame writes one frame. data may be nil for status frames and
// the end frame.
func WriteBulkFrame(w io.Writer, idx uint64, status uint64, data []byte) error {
	hdr := make([]byte, 0, 3*binary.MaxVarintLen64)
	hdr = binary.AppendUvarint(hdr, idx)
	hdr = binary.AppendUvarint(hdr, status)
	hdr = binary.AppendUvarint(hdr, uint64(len(data)))
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	if len(data) > 0 {
		if _, err := w.Write(data); err != nil {
			return err
		}
	}
	return nil
}

// ReadBulkFrame reads one frame. On a clean end of stream (no bytes before
// the next frame) it returns io.EOF; a stream truncated mid-frame returns
// io.ErrUnexpectedEOF. The caller detects the end frame by idx ==
// len(blocks).
func ReadBulkFrame(r *bufio.Reader) (idx uint64, status uint64, data []byte, err error) {
	idx, err = binary.ReadUvarint(r)
	if err != nil {
		if err == io.EOF {
			return 0, 0, nil, io.EOF
		}
		return 0, 0, nil, fmt.Errorf("reading frame index: %w", err)
	}
	status, err = binary.ReadUvarint(r)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("reading frame status: %w", unexpectEOF(err))
	}
	length, err := binary.ReadUvarint(r)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("reading frame length: %w", unexpectEOF(err))
	}
	if length > BulkMaxFrameData {
		return 0, 0, nil, fmt.Errorf("frame data length %d exceeds limit %d", length, uint64(BulkMaxFrameData))
	}
	if length > 0 {
		data = make([]byte, length)
		if _, err := io.ReadFull(r, data); err != nil {
			return 0, 0, nil, fmt.Errorf("reading frame data: %w", err)
		}
	}
	return idx, status, data, nil
}

func unexpectEOF(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}
