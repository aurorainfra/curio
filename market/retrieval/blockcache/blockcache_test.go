package blockcache

import (
	"fmt"
	"testing"

	blocks "github.com/ipfs/go-block-format"
	"github.com/stretchr/testify/require"

	"github.com/filecoin-project/lotus/blockstore"
)

func testBlock(t *testing.T, seed string, size int) (blocks.Block, blockstore.MhString) {
	data := make([]byte, size)
	copy(data, seed)
	blk := blocks.NewBlock(data)
	return blk, blockstore.MhString(blk.Cid().Hash())
}

func TestBlockCache(t *testing.T) {
	c := New(Config{
		SizeClasses:    []int{32 << 10, 128 << 10, 2 << 20},
		PartitionBytes: 64 << 10, // tiny resident budgets for eviction testing
		GhostBytes:     1 << 20,
		AdmitAfter:     1,
	})

	blk, mh := testBlock(t, "a", 16<<10)

	// first access: tracked in the ghost set only, not cached
	c.Add(mh, blk)
	_, ok := c.Get(mh)
	require.False(t, ok, "one-shot access must not be cached")
	require.False(t, c.Contains(mh))

	// second access: admitted
	c.Add(mh, blk)
	got, ok := c.Get(mh)
	require.True(t, ok, "second access should admit")
	require.Equal(t, blk.RawData(), got.RawData())

	// oversized blocks are never cached
	big, bigMh := testBlock(t, "big", 3<<20)
	c.Add(bigMh, big)
	c.Add(bigMh, big)
	require.False(t, c.Contains(bigMh))

	// partitioning: a large-class block does not evict the small-class one
	mid, midMh := testBlock(t, "mid", 60<<10)
	c.Add(midMh, mid)
	c.Add(midMh, mid)
	require.True(t, c.Contains(midMh))
	require.True(t, c.Contains(mh), "small-class block must survive large-class inserts")

	// a block larger than its whole partition budget is never admitted
	over, overMh := testBlock(t, "over", 100<<10)
	c.Add(overMh, over)
	c.Add(overMh, over)
	require.False(t, c.Contains(overMh))

	// byte-budget eviction within a class: 64KiB budget, 16KiB blocks ->
	// four resident max; admitting more evicts the least-recently-used
	var mhs []blockstore.MhString
	for i := 0; i < 6; i++ {
		b, m := testBlock(t, fmt.Sprintf("fill-%d", i), 16<<10)
		c.Add(m, b)
		c.Add(m, b) // admit
		mhs = append(mhs, m)
	}
	require.True(t, c.Contains(mhs[5]), "most recent block resident")
	require.False(t, c.Contains(mh), "oldest block evicted")

	// ghost readmission: the evicted block kept its frequency, one access
	// readmits it immediately
	c.Add(mh, blk)
	require.True(t, c.Contains(mh), "evictee readmitted from ghost on next access")

	// Remove drops resident and ghost state
	c.Remove(mh)
	require.False(t, c.Contains(mh))
	c.Add(mh, blk)
	require.False(t, c.Contains(mh), "after Remove the first access only tracks again")
}

func TestBlockCacheAdmitAfterZero(t *testing.T) {
	c := New(Config{AdmitAfter: 0, PartitionBytes: 1 << 20, GhostBytes: 1 << 20,
		SizeClasses: []int{32 << 10}})
	blk, mh := testBlock(t, "x", 1<<10)
	c.Add(mh, blk)
	require.True(t, c.Contains(mh), "AdmitAfter=0 admits on first access")
}
