package curetr

import (
	"bufio"
	"bytes"
	"io"
	"testing"

	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/require"
)

func testCid(t *testing.T, seed byte) cid.Cid {
	mh, err := multihash.Sum(bytes.Repeat([]byte{seed}, 4), multihash.SHA2_256, -1)
	require.NoError(t, err)
	return cid.NewCidV1(cid.Raw, mh)
}

func TestBulkRequestCborRoundTrip(t *testing.T) {
	req := BulkRequest{
		Blocks: []cid.Cid{testCid(t, 1), testCid(t, 2), testCid(t, 3)},
		Window: 64,
		Pieces: []cid.Cid{testCid(t, 9), testCid(t, 9), testCid(t, 9)},
	}

	var buf bytes.Buffer
	require.NoError(t, req.MarshalCBOR(&buf))

	var got BulkRequest
	require.NoError(t, got.UnmarshalCBOR(&buf))
	require.Equal(t, req, got)
}

func TestBulkRequestCborNoPieces(t *testing.T) {
	req := BulkRequest{
		Blocks: []cid.Cid{testCid(t, 1)},
		Window: 1,
	}

	var buf bytes.Buffer
	require.NoError(t, req.MarshalCBOR(&buf))

	var got BulkRequest
	require.NoError(t, got.UnmarshalCBOR(&buf))
	require.Equal(t, uint64(1), got.Window)
	require.Len(t, got.Blocks, 1)
	require.Empty(t, got.Pieces)
}

func TestBulkFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer

	require.NoError(t, WriteBulkFrame(&buf, 0, BulkStatusOk, []byte("hello")))
	require.NoError(t, WriteBulkFrame(&buf, 300, BulkStatusNotFound, nil))
	require.NoError(t, WriteBulkFrame(&buf, 2, BulkStatusError, nil))
	require.NoError(t, WriteBulkFrame(&buf, 301, BulkStatusOk, nil)) // end frame

	br := bufio.NewReader(&buf)

	idx, status, data, err := ReadBulkFrame(br)
	require.NoError(t, err)
	require.Equal(t, uint64(0), idx)
	require.Equal(t, uint64(BulkStatusOk), status)
	require.Equal(t, []byte("hello"), data)

	idx, status, data, err = ReadBulkFrame(br)
	require.NoError(t, err)
	require.Equal(t, uint64(300), idx)
	require.Equal(t, uint64(BulkStatusNotFound), status)
	require.Nil(t, data)

	idx, status, _, err = ReadBulkFrame(br)
	require.NoError(t, err)
	require.Equal(t, uint64(2), idx)
	require.Equal(t, uint64(BulkStatusError), status)

	idx, status, data, err = ReadBulkFrame(br)
	require.NoError(t, err)
	require.Equal(t, uint64(301), idx)
	require.Equal(t, uint64(BulkStatusOk), status)
	require.Empty(t, data)

	_, _, _, err = ReadBulkFrame(br)
	require.ErrorIs(t, err, io.EOF)
}

func TestBulkFrameTruncated(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, WriteBulkFrame(&buf, 5, BulkStatusOk, []byte("hello world")))

	// truncate mid-payload
	trunc := buf.Bytes()[:buf.Len()-4]
	_, _, _, err := ReadBulkFrame(bufio.NewReader(bytes.NewReader(trunc)))
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)

	// truncate mid-header (after the index varint)
	_, _, _, err = ReadBulkFrame(bufio.NewReader(bytes.NewReader(buf.Bytes()[:1])))
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
}
