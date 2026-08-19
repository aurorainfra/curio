package paths_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/curio/lib/paths"
	"github.com/filecoin-project/curio/lib/paths/mocks"
	"github.com/filecoin-project/curio/lib/storiface"
)

// TestReaderRangeHeader asserts that remote unsealed reads request exactly
// the byte range they need. Regression test for the Range overshoot bug
// where Reader passed the absolute end offset as ReadRemote's length,
// producing ranges that overshot by (pieceOffset + start) bytes — for pieces
// deep in a sector the remote would stream gigabytes per ~1MB read.
func TestReaderRangeHeader(t *testing.T) {
	mockCtrl := gomock.NewController(t)
	defer mockCtrl.Finish()

	sectorRef := storiface.SectorRef{
		ID: abi.SectorID{
			Miner:  123,
			Number: 123,
		},
		ProofType: 1,
	}

	// piece deep in the sector: this is what made the overshoot huge
	pieceOffset := abi.PaddedPieceSize(2 << 30)
	pieceSize := abi.PaddedPieceSize(1 << 20)

	rangeCh := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/allocated/") {
			w.WriteHeader(http.StatusOK)
			return
		}
		rangeCh <- r.Header.Get("Range")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("data"))
	}))
	defer srv.Close()

	sectorURL := fmt.Sprintf("%s/remote/%s/%s", srv.URL, storiface.FTUnsealed.String(), storiface.SectorName(sectorRef.ID))

	lstore := mocks.NewMockStore(mockCtrl)
	lstore.EXPECT().AcquireSector(gomock.Any(), sectorRef, storiface.FTUnsealed, storiface.FTNone,
		storiface.PathStorage, storiface.AcquireMove).Return(storiface.SectorPaths{}, storiface.SectorPaths{}, nil)

	index := mocks.NewMockSectorIndex(mockCtrl)
	index.EXPECT().StorageFindSector(gomock.Any(), sectorRef.ID, storiface.FTUnsealed, gomock.Any(), false).
		Return([]storiface.SectorStorageInfo{{URLs: []string{sectorURL}}}, nil)

	remoteStore, err := paths.NewRemote(lstore, index, nil, 6000, mocks.NewMockPartialFileHandler(mockCtrl))
	require.NoError(t, err)

	rdg, err := remoteStore.Reader(context.Background(), sectorRef, pieceOffset, pieceSize)
	require.NoError(t, err)
	require.NotNil(t, rdg)

	// read the whole piece: start=0, end=pieceSize (piece-relative)
	rd, err := rdg(0, storiface.PaddedByteIndex(pieceSize))
	require.NoError(t, err)
	_, _ = io.ReadAll(rd)
	require.NoError(t, rd.Close())

	wantRange := fmt.Sprintf("bytes=%d-%d", pieceOffset, uint64(pieceOffset)+uint64(pieceSize)-1)
	require.Equal(t, wantRange, <-rangeCh)
}
