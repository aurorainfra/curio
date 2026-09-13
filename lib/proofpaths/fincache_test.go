package proofpaths

import (
	"fmt"
	"maps"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/go-state-types/abi"
)

func TestTreeRLastFileNames(t *testing.T) {
	names, err := TreeRLastFileNames(abi.RegisteredSealProof_StackedDrg2KiBV1_1)
	require.NoError(t, err)
	require.Equal(t, []string{"sc-02-data-tree-r-last.dat"}, names)

	names, err = TreeRLastFileNames(abi.RegisteredSealProof_StackedDrg32GiBV1_1)
	require.NoError(t, err)
	require.Len(t, names, 8)
	require.Equal(t, "sc-02-data-tree-r-last-0.dat", names[0])
	require.Equal(t, "sc-02-data-tree-r-last-7.dat", names[7])
	for _, n := range names {
		require.True(t, IsFileTreeRLast(n), n)
	}

	names, err = TreeRLastFileNames(abi.RegisteredSealProof_StackedDrg64GiBV1_1_Feat_SyntheticPoRep)
	require.NoError(t, err)
	require.Len(t, names, 16)

	_, err = TreeRLastFileNames(abi.RegisteredSealProof(-1))
	require.Error(t, err)
}

func TestFinCacheMissing(t *testing.T) {
	spt := abi.RegisteredSealProof_StackedDrg32GiBV1_1

	full := map[string]int64{PAuxName: PAuxSize, TAuxName: 1234}
	for i := 0; i < 8; i++ {
		full[fmt.Sprintf("%s%d.dat", TreeRLastPrefix, i)] = 9_586_976
	}
	require.Empty(t, FinCacheMissing(spt, full))

	// partial cache left behind by a crashed TreeRC: trees but no p_aux
	noPAux := maps.Clone(full)
	delete(noPAux, PAuxName)
	require.Equal(t, []string{PAuxName}, FinCacheMissing(spt, noPAux))

	// truncated p_aux
	badPAux := maps.Clone(full)
	badPAux[PAuxName] = 32
	require.Len(t, FinCacheMissing(spt, badPAux), 1)

	// missing tree-r-last partition
	noTree := maps.Clone(full)
	delete(noTree, TreeRLastPrefix+"3.dat")
	require.Equal(t, []string{TreeRLastPrefix + "3.dat"}, FinCacheMissing(spt, noTree))

	// t_aux is regenerable, not required
	noTAux := maps.Clone(full)
	delete(noTAux, TAuxName)
	require.Empty(t, FinCacheMissing(spt, noTAux))

	// unknown proof type: only p_aux is checked
	require.Empty(t, FinCacheMissing(abi.RegisteredSealProof(-1), map[string]int64{PAuxName: PAuxSize}))
	require.Equal(t, []string{PAuxName}, FinCacheMissing(abi.RegisteredSealProof(-1), map[string]int64{}))
}
