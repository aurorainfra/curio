package cachedreader

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ipfs/go-cid"
	"golang.org/x/xerrors"

	commcid "github.com/filecoin-project/go-fil-commcid"
	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/curio/harmony/harmonydb"
	"github.com/filecoin-project/curio/lib/commcidv2"
)

// mpdDeal is one market_piece_deal row (joined with sectors_meta for the
// seal proof), as used by the retrieval piece-resolution path.
type mpdDeal struct {
	ID       string                  `db:"id"`
	SpID     int64                   `db:"sp_id"`
	Sector   int64                   `db:"sector_num"`
	Offset   sql.NullInt64           `db:"piece_offset"`
	Length   abi.PaddedPieceSize     `db:"piece_length"`
	RawSize  int64                   `db:"raw_size"`
	Proof    abi.RegisteredSealProof `db:"reg_seal_proof"`
	PieceRef sql.NullInt64           `db:"piece_ref"`
}

// Note the inner join on sectors_meta: it restricts the cache to deals whose
// sector finished sealing. Deals still being sealed have no sectors_meta row
// (and no readable unsealed copy in its final place); caching them would
// pin reg_seal_proof=0 rows that stay wrong after sealing completes. They
// are simply absent here and get backfilled on a post-seal cache miss.
const mpdDealCols = `mpd.id,
	mpd.sp_id,
	mpd.sector_num,
	mpd.piece_offset,
	mpd.piece_length,
	mpd.raw_size,
	mpd.piece_ref,
	sm.reg_seal_proof
FROM market_piece_deal mpd
JOIN sectors_meta sm
  ON sm.sp_id = mpd.sp_id
 AND sm.sector_num = mpd.sector_num`

// pieceDealCache keeps piece -> market deal metadata in memory
// (Indexing.PreloadRetrievalMetadata). All rows are bulk-loaded once at
// startup; pieces not found in the cache (e.g. deals indexed after startup)
// are looked up in the DB and backfilled. Entries are dropped when a read
// through them fails (Invalidate), so stale metadata self-heals.
type pieceDealCache struct {
	db *harmonydb.DB

	loaded atomic.Bool

	mu    sync.RWMutex
	deals map[string][]mpdDeal // piece cid (v1) string -> deals (all piece lengths)
}

func newPieceDealCache(db *harmonydb.DB) *pieceDealCache {
	return &pieceDealCache{
		db:    db,
		deals: map[string][]mpdDeal{},
	}
}

// start bulk-loads the cache in the background; until it finishes Get serves
// straight from the DB.
func (c *pieceDealCache) start(ctx context.Context) {
	astart := time.Now()

	var rows []struct {
		PieceCid string `db:"piece_cid"`
		mpdDeal
	}
	err := c.db.Select(ctx, &rows, `SELECT mpd.piece_cid, `+mpdDealCols)
	if err != nil {
		log.Errorw("preloading piece deal metadata failed; serving from DB", "err", err)
		return
	}

	deals := make(map[string][]mpdDeal, len(rows))
	for _, r := range rows {
		deals[r.PieceCid] = append(deals[r.PieceCid], r.mpdDeal)
	}

	c.mu.Lock()
	c.deals = deals
	c.mu.Unlock()
	c.loaded.Store(true)

	log.Infow("preloaded piece deal metadata", "pieces", len(deals), "deals", len(rows), "took", time.Since(astart).Truncate(time.Millisecond))
}

// Get returns all market deals for a v1 piece cid (any piece length). Before
// the bulk load finishes, and on cache miss, it queries the DB; miss results
// with deals are backfilled into the cache.
func (c *pieceDealCache) Get(ctx context.Context, pieceCidV1 cid.Cid) ([]mpdDeal, error) {
	key := pieceCidV1.String()

	if c.loaded.Load() {
		c.mu.RLock()
		ds, ok := c.deals[key]
		c.mu.RUnlock()
		if ok {
			return ds, nil
		}
	}

	var ds []mpdDeal
	err := c.db.Select(ctx, &ds, `SELECT `+mpdDealCols+` WHERE mpd.piece_cid = $1`, key)
	if err != nil {
		return nil, xerrors.Errorf("getting piece deals: %w", err)
	}

	// Only backfill positive results: an empty entry would hide deals indexed
	// later, and the piece error cache already rate-limits repeated misses.
	if c.loaded.Load() && len(ds) > 0 {
		c.mu.Lock()
		c.deals[key] = ds
		c.mu.Unlock()
	}

	return ds, nil
}

// Invalidate drops the cache entry for a piece (v1 or v2 cid), forcing the
// next resolution to re-query the DB.
func (c *pieceDealCache) Invalidate(piece cid.Cid) {
	pieceCid := piece
	if commcidv2.IsPieceCidV2(piece) {
		v1, _, err := commcid.PieceCidV1FromV2(piece)
		if err != nil {
			return
		}
		pieceCid = v1
	}

	c.mu.Lock()
	delete(c.deals, pieceCid.String())
	c.mu.Unlock()
}
