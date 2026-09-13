package gc

import (
	"maps"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/curio/lib/proofpaths"
	"github.com/filecoin-project/curio/lib/storiface"
)

func TestStoreCopyComplete(t *testing.T) {
	spt := abi.RegisteredSealProof_StackedDrg32GiBV1_1
	ssize, err := spt.SectorSize()
	require.NoError(t, err)

	fullCache := map[string]int64{proofpaths.PAuxName: proofpaths.PAuxSize}
	names, err := proofpaths.TreeRLastFileNames(spt)
	require.NoError(t, err)
	for _, n := range names {
		fullCache[n] = 1
	}

	// sealed / update: exact sector size, must be a file
	require.NoError(t, storeCopyComplete(storiface.FTSealed, spt, ssize, storiface.SectorFileStat{Size: int64(ssize)}))
	require.Error(t, storeCopyComplete(storiface.FTSealed, spt, ssize, storiface.SectorFileStat{Size: int64(ssize) - 1}))
	require.Error(t, storeCopyComplete(storiface.FTUpdate, spt, ssize, storiface.SectorFileStat{IsDir: true}))

	// unsealed: at least sector size
	require.NoError(t, storeCopyComplete(storiface.FTUnsealed, spt, ssize, storiface.SectorFileStat{Size: int64(ssize) + 4096}))
	require.Error(t, storeCopyComplete(storiface.FTUnsealed, spt, ssize, storiface.SectorFileStat{Size: 0}))

	// cache: directory with p_aux and all tree-r-last partitions
	require.NoError(t, storeCopyComplete(storiface.FTCache, spt, ssize, storiface.SectorFileStat{IsDir: true, Files: fullCache}))
	partial := maps.Clone(fullCache)
	delete(partial, proofpaths.PAuxName)
	require.Error(t, storeCopyComplete(storiface.FTCache, spt, ssize, storiface.SectorFileStat{IsDir: true, Files: partial}))
	require.Error(t, storeCopyComplete(storiface.FTUpdateCache, spt, ssize, storiface.SectorFileStat{Size: 100}))
	// unknown layout can't be verified
	require.Error(t, storeCopyComplete(storiface.FTUpdateCache, abi.RegisteredSealProof(-1), ssize, storiface.SectorFileStat{IsDir: true, Files: fullCache}))

	require.Error(t, storeCopyComplete(storiface.FTPiece, spt, ssize, storiface.SectorFileStat{Size: 1}))
}
