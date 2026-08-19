package paths_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/curio/lib/paths"
	"github.com/filecoin-project/curio/lib/paths/mocks"
	"github.com/filecoin-project/curio/lib/storiface"

	"github.com/filecoin-project/lotus/storage/sealer/fsutil"
)

type stubLocalStorage struct {
	root string
}

func (s *stubLocalStorage) GetStorage() (storiface.StorageConfig, error) {
	return storiface.StorageConfig{StoragePaths: []storiface.LocalPath{{Path: s.root}}}, nil
}

func (s *stubLocalStorage) SetStorage(func(*storiface.StorageConfig)) error { return nil }

func (s *stubLocalStorage) Stat(path string) (fsutil.FsStat, error) {
	return fsutil.FsStat{Capacity: 1 << 40, Available: 1 << 40}, nil
}

func (s *stubLocalStorage) DiskUsage(path string) (int64, error) { return 1, nil }

// TestLocalPathIndex checks that files present in local storage paths are
// resolvable via LocalPath without any sector index round-trip, and that a
// removed file self-invalidates on the next lookup.
func TestLocalPathIndex(t *testing.T) {
	ctx := context.Background()
	mockCtrl := gomock.NewController(t)
	defer mockCtrl.Finish()

	root := t.TempDir()

	meta := storiface.LocalStorageMeta{
		ID:       storiface.ID(uuid.New().String()),
		Weight:   10,
		CanStore: true,
	}
	mb, err := json.MarshalIndent(&meta, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "sectorstore.json"), mb, 0644))

	sid := abi.SectorID{Miner: 1000, Number: 1}
	require.NoError(t, os.MkdirAll(filepath.Join(root, storiface.FTUnsealed.String()), 0755))
	sectorFile := filepath.Join(root, storiface.FTUnsealed.String(), storiface.SectorName(sid))
	require.NoError(t, os.WriteFile(sectorFile, []byte("data"), 0644))

	index := mocks.NewMockSectorIndex(mockCtrl)
	index.EXPECT().StorageAttach(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	index.EXPECT().StorageList(gomock.Any(), gomock.Any()).Return(nil, nil).AnyTimes()
	index.EXPECT().BatchStorageDeclareSectors(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	index.EXPECT().StorageReportHealth(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	l, err := paths.NewLocal(ctx, &stubLocalStorage{root: root}, index, "http://127.0.0.1:0/remote")
	require.NoError(t, err)

	// resolves from memory, no StorageFindSector expectation on the mock
	p, ok := l.LocalPath(sid, storiface.FTUnsealed)
	require.True(t, ok)
	require.Equal(t, sectorFile, p)

	// unknown sector misses
	_, ok = l.LocalPath(abi.SectorID{Miner: 1000, Number: 2}, storiface.FTUnsealed)
	require.False(t, ok)

	// removed file self-invalidates
	require.NoError(t, os.Remove(sectorFile))
	_, ok = l.LocalPath(sid, storiface.FTUnsealed)
	require.False(t, ok)
}
