// Package zeropiecelocator provides a test PieceLocator HTTP server that serves
// an all-zero-bytes 32GiB piece. It implements the PieceLocator HTTP protocol
// (HEAD and GET with ?id=<pieceCID>) and responds with the appropriate zerocomm
// piece CID for a 32GiB sector. Supports HTTP Range requests.
//
// This is useful for testing the offline deal pipeline without needing real piece data.
package zeropiecelocator

import (
	"io"
	"math/bits"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ipfs/go-cid"

	"github.com/filecoin-project/go-commp-utils/zerocomm"
	commcid "github.com/filecoin-project/go-fil-commcid"
	"github.com/filecoin-project/go-state-types/abi"
)

var (
	// PaddedSize is the padded piece size (32 GiB).
	PaddedSize = abi.PaddedPieceSize(32 << 30)

	// UnpaddedSize is the unpadded piece size corresponding to PaddedSize.
	UnpaddedSize = PaddedSize.Unpadded()

	// RawSize is the raw data size in bytes (equal to UnpaddedSize).
	RawSize = int64(UnpaddedSize)

	// PieceCIDV1 is the V1 piece commitment CID for the 32GiB zero piece.
	PieceCIDV1 cid.Cid

	// PieceCIDV2 is the V2 piece commitment CID for the 32GiB zero piece.
	PieceCIDV2 cid.Cid
)

func init() {
	PieceCIDV1 = zerocomm.ZeroPieceCommitment(UnpaddedSize)

	level := bits.TrailingZeros64(uint64(PaddedSize)) - zerocomm.Skip - 5
	var err error
	PieceCIDV2, err = commcid.DataCommitmentToPieceCidv2(zerocomm.PieceComms[level][:], uint64(RawSize))
	if err != nil {
		panic("zeropiecelocator: failed to compute V2 CID: " + err.Error())
	}
}

// Handler creates an http.Handler that serves the 32GiB zero piece.
// Supports HEAD, GET, and Range requests.
//
// The handler responds to requests with ?id=<pieceCID> where the CID matches
// either the V1 or V2 zero piece CID:
//   - HEAD: responds 200 with Content-Length and Accept-Ranges: bytes
//   - GET: responds 200 with full zero-byte content
//   - GET with Range header: responds 206 Partial Content
//   - Non-matching CIDs receive 404
func Handler() http.Handler {
	accepted := map[string]bool{
		PieceCIDV1.String(): true,
		PieceCIDV2.String(): true,
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		if !accepted[id] {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, id, time.Time{}, io.NewSectionReader(&zeroReaderAt{}, 0, RawSize))
	})
}

// NewServer creates a test HTTP server that serves the 32GiB zero piece.
// The server is automatically closed when the test finishes.
func NewServer(t testing.TB) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(Handler())
	t.Cleanup(ts.Close)
	return ts
}

// zeroReaderAt implements io.ReaderAt returning zero bytes at any offset.
type zeroReaderAt struct{}

func (*zeroReaderAt) ReadAt(p []byte, _ int64) (int, error) {
	clear(p)
	return len(p), nil
}
