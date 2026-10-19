package zeropiecelocator

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/go-state-types/abi"
)

func TestPieceCIDs(t *testing.T) {
	// Verify the exported CIDs are valid and distinct.
	require.True(t, PieceCIDV1.Defined(), "V1 CID should be defined")
	require.True(t, PieceCIDV2.Defined(), "V2 CID should be defined")
	require.NotEqual(t, PieceCIDV1, PieceCIDV2, "V1 and V2 CIDs should differ")

	t.Logf("PieceCIDV1: %s", PieceCIDV1)
	t.Logf("PieceCIDV2: %s", PieceCIDV2)
	t.Logf("RawSize: %d", RawSize)
	t.Logf("PaddedSize: %d", PaddedSize)
}

func TestSizeConsistency(t *testing.T) {
	require.Equal(t, abi.PaddedPieceSize(32<<30), PaddedSize)
	require.Equal(t, PaddedSize.Unpadded(), UnpaddedSize)
	require.Equal(t, int64(UnpaddedSize), RawSize)
}

func TestHandler_HEAD_V1(t *testing.T) {
	ts := NewServer(t)

	url := fmt.Sprintf("%s?id=%s", ts.URL, PieceCIDV1.String())
	resp, err := http.Head(url)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, strconv.FormatInt(RawSize, 10), resp.Header.Get("Content-Length"))
	require.Equal(t, "bytes", resp.Header.Get("Accept-Ranges"))
}

func TestHandler_HEAD_V2(t *testing.T) {
	ts := NewServer(t)

	url := fmt.Sprintf("%s?id=%s", ts.URL, PieceCIDV2.String())
	resp, err := http.Head(url)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, strconv.FormatInt(RawSize, 10), resp.Header.Get("Content-Length"))
	require.Equal(t, "bytes", resp.Header.Get("Accept-Ranges"))
}

func TestHandler_HEAD_Unknown(t *testing.T) {
	ts := NewServer(t)

	url := fmt.Sprintf("%s?id=%s", ts.URL, "baga6ea4seaqao7s73y24kcutaosvacpdjgfe5pw76ooze3glnc7bs3u6tgmqrq")
	resp, err := http.Head(url)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestHandler_GET_StreamsZeros(t *testing.T) {
	ts := NewServer(t)

	url := fmt.Sprintf("%s?id=%s", ts.URL, PieceCIDV1.String())
	resp, err := http.Get(url)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, strconv.FormatInt(RawSize, 10), resp.Header.Get("Content-Length"))

	// Read a small chunk and verify it's all zeros. We don't read the full
	// 32GiB in a test -- just enough to confirm the stream content.
	buf := make([]byte, 64*1024)
	for i := range buf {
		buf[i] = 0xFF
	}

	n, err := resp.Body.Read(buf)
	require.NoError(t, err)
	require.Greater(t, n, 0)

	for i := 0; i < n; i++ {
		if buf[i] != 0 {
			t.Fatalf("expected zero byte at offset %d, got %d", i, buf[i])
		}
	}
}

func TestHandler_RangeRequest(t *testing.T) {
	ts := NewServer(t)

	url := fmt.Sprintf("%s?id=%s", ts.URL, PieceCIDV1.String())

	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	req.Header.Set("Range", "bytes=1000-1099")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusPartialContent, resp.StatusCode)
	require.Equal(t, "100", resp.Header.Get("Content-Length"))
	require.Contains(t, resp.Header.Get("Content-Range"), "bytes 1000-1099/")

	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Len(t, data, 100)

	for i, b := range data {
		require.Equal(t, byte(0), b, "non-zero byte at offset %d", i)
	}
}

func TestHandler_RangeRequest_LargeOffset(t *testing.T) {
	ts := NewServer(t)

	url := fmt.Sprintf("%s?id=%s", ts.URL, PieceCIDV2.String())

	// Request a range near the end of the piece.
	start := RawSize - 512
	end := RawSize - 1
	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusPartialContent, resp.StatusCode)
	require.Equal(t, "512", resp.Header.Get("Content-Length"))

	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Len(t, data, 512)
}

func TestZeroReaderAt(t *testing.T) {
	var zr zeroReaderAt
	buf := make([]byte, 1024)
	for i := range buf {
		buf[i] = 0xFF
	}

	n, err := zr.ReadAt(buf, 999999)
	require.NoError(t, err)
	require.Equal(t, 1024, n)

	for i, b := range buf {
		require.Equal(t, byte(0), b, "non-zero byte at offset %d", i)
	}
}
