package proofpaths

import (
	"fmt"
	"os"

	"github.com/filecoin-project/go-state-types/abi"
)

const PAuxName = "p_aux"
const TAuxName = "t_aux"

// PAuxSize is the size of p_aux: comm_c followed by comm_r_last, 32 bytes each.
const PAuxSize = 64

// TreeRLastPartitions returns the number of tree-r-last partition files
// (sub-trees) a sealed sector of the given proof type has. This follows the
// sector shapes in rust-fil-proofs: sectors up to 512MiB use a single base
// tree, 32GiB sectors are split into 8 sub-trees and 64GiB sectors into 16.
func TreeRLastPartitions(spt abi.RegisteredSealProof) (int, error) {
	switch spt {
	case abi.RegisteredSealProof_StackedDrg2KiBV1, abi.RegisteredSealProof_StackedDrg2KiBV1_1, abi.RegisteredSealProof_StackedDrg2KiBV1_1_Feat_SyntheticPoRep, abi.RegisteredSealProof_StackedDrg2KiBV1_2_Feat_NiPoRep:
		return 1, nil
	case abi.RegisteredSealProof_StackedDrg8MiBV1, abi.RegisteredSealProof_StackedDrg8MiBV1_1, abi.RegisteredSealProof_StackedDrg8MiBV1_1_Feat_SyntheticPoRep, abi.RegisteredSealProof_StackedDrg8MiBV1_2_Feat_NiPoRep:
		return 1, nil
	case abi.RegisteredSealProof_StackedDrg512MiBV1, abi.RegisteredSealProof_StackedDrg512MiBV1_1, abi.RegisteredSealProof_StackedDrg512MiBV1_1_Feat_SyntheticPoRep, abi.RegisteredSealProof_StackedDrg512MiBV1_2_Feat_NiPoRep:
		return 1, nil
	case abi.RegisteredSealProof_StackedDrg32GiBV1, abi.RegisteredSealProof_StackedDrg32GiBV1_1, abi.RegisteredSealProof_StackedDrg32GiBV1_1_Feat_SyntheticPoRep, abi.RegisteredSealProof_StackedDrg32GiBV1_2_Feat_NiPoRep:
		return 8, nil
	case abi.RegisteredSealProof_StackedDrg64GiBV1, abi.RegisteredSealProof_StackedDrg64GiBV1_1, abi.RegisteredSealProof_StackedDrg64GiBV1_1_Feat_SyntheticPoRep, abi.RegisteredSealProof_StackedDrg64GiBV1_2_Feat_NiPoRep:
		return 16, nil

	default:
		return 0, fmt.Errorf("unsupported proof type: %v", spt)
	}
}

// TreeRLastFileNames returns the tree-r-last file names of a sector of the
// given proof type. With a single partition rust-fil-proofs names the file
// "sc-02-data-tree-r-last.dat", otherwise partitions are numbered
// "sc-02-data-tree-r-last-N.dat".
func TreeRLastFileNames(spt abi.RegisteredSealProof) ([]string, error) {
	n, err := TreeRLastPartitions(spt)
	if err != nil {
		return nil, err
	}

	if n == 1 {
		return []string{dataFilePrefix + "tree-r-last.dat"}, nil
	}

	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("%s%d.dat", TreeRLastPrefix, i))
	}
	return out, nil
}

// FinCacheMissing reports which files a finalized cache directory (post-PoRep
// or post-snap) must contain for proving and snap-encoding but which are
// absent or malformed in files (base name -> size). p_aux is always required;
// tree-r-last partitions are checked when the proof type is known. t_aux is
// not required: it only holds store configuration and can be regenerated.
func FinCacheMissing(spt abi.RegisteredSealProof, files map[string]int64) []string {
	var missing []string

	if sz, ok := files[PAuxName]; !ok {
		missing = append(missing, PAuxName)
	} else if sz != PAuxSize {
		missing = append(missing, fmt.Sprintf("%s (size %d, want %d)", PAuxName, sz, PAuxSize))
	}

	if names, err := TreeRLastFileNames(spt); err == nil {
		for _, name := range names {
			if sz, ok := files[name]; !ok || sz == 0 {
				missing = append(missing, name)
			}
		}
	}

	return missing
}

// FinCacheDirMissing is FinCacheMissing applied to the regular files in dir.
func FinCacheDirMissing(spt abi.RegisteredSealProof, dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	files := make(map[string]int64, len(ents))
	for _, ent := range ents {
		if ent.IsDir() {
			continue
		}
		info, err := ent.Info()
		if err != nil {
			if os.IsNotExist(err) {
				continue // removed while listing
			}
			return nil, err
		}
		files[ent.Name()] = info.Size()
	}

	return FinCacheMissing(spt, files), nil
}
