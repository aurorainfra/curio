package paths

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/curio/lib/storiface"
)

func TestSortCopiesByTrust(t *testing.T) {
	copies := func() []storiface.SectorStorageInfo {
		return []storiface.SectorStorageInfo{
			{ID: "seal-local", CanSeal: true, Weight: 100},
			{ID: "store-remote-b", CanStore: true, Weight: 10},
			{ID: "store-remote-a", CanStore: true, Weight: 10},
			{ID: "store-local", CanStore: true, Weight: 1},
			{ID: "store-primary", CanStore: true, Primary: true, Weight: 1},
		}
	}
	local := map[storiface.ID]string{"seal-local": "/seal", "store-local": "/store"}
	ids := func(si []storiface.SectorStorageInfo) []storiface.ID {
		out := make([]storiface.ID, 0, len(si))
		for _, s := range si {
			out = append(out, s.ID)
		}
		return out
	}

	// store before seal even when the seal copy is local and heavier; primary
	// first; local store copy before remote ones; equal weights keep their order
	si := copies()
	sortCopiesByTrust(si, "", local)
	require.Equal(t, []storiface.ID{"store-primary", "store-local", "store-remote-b", "store-remote-a", "seal-local"}, ids(si))

	// the preferred path beats locality and weight, but not the store/primary ranking
	si = copies()
	sortCopiesByTrust(si, "store-remote-a", local)
	require.Equal(t, []storiface.ID{"store-primary", "store-remote-a", "store-local", "store-remote-b", "seal-local"}, ids(si))

	// preferring a seal path never promotes it above store copies
	si = copies()
	sortCopiesByTrust(si, "seal-local", local)
	require.Equal(t, []storiface.ID{"store-primary", "store-local", "store-remote-b", "store-remote-a", "seal-local"}, ids(si))
}
