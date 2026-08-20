package cuhttp

import (
	"context"

	"github.com/go-chi/chi/v5"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/curio/deps"
	"github.com/filecoin-project/curio/market/denylist"
	ipni_provider "github.com/filecoin-project/curio/market/ipni/ipni-provider"
	"github.com/filecoin-project/curio/market/retrieval"
	"github.com/filecoin-project/curio/market/retrieval/blockcache"
)

// MountRetrievalPublicRoutes mounts piece/IPFS retrieval with bad-bits denylist filtering.
// Skiff and full Curio both use this; tests can mount it without standing up IPNI.
func MountRetrievalPublicRoutes(ctx context.Context, r *chi.Mux, d *deps.Deps) *denylist.Filter {
	df := denylist.NewFilter(ctx, d.Cfg.HTTP.DenylistServers)
	var bcCfg *blockcache.Config
	if bc := d.Cfg.HTTP.RetrievalBlockCache; !bc.Disable {
		classes := make([]int, len(bc.SizeClassKiB))
		for i, kib := range bc.SizeClassKiB {
			classes[i] = kib << 10
		}
		bcCfg = &blockcache.Config{
			SizeClasses:    classes,
			PartitionBytes: int64(bc.PartitionMiB) << 20,
			GhostBytes:     int64(bc.GhostMiB) << 20,
			AdmitAfter:     bc.AdmitAfter,
		}
	}
	rp := retrieval.NewRetrievalProvider(ctx, d.DB, d.IndexStore, d.CachedPieceReader, df, d.Cfg.Market.StorageMarketConfig.Indexing.RetrievalOffsetCacheMemMiB, bcCfg)
	retrieval.Router(r, rp, df)
	return df
}

// MountCommonPublicRoutes mounts public endpoints shared by curio and skiff:
// piece/IPFS retrieval with bad-bits denylist filtering, and IPNI provider routes.
// The returned IPNI provider is started for publishing and can be passed to PDP route mounting.
func MountCommonPublicRoutes(ctx context.Context, r *chi.Mux, d *deps.Deps) (*ipni_provider.Provider, error) {
	_ = MountRetrievalPublicRoutes(ctx, r, d)

	ipp, err := ipni_provider.NewProvider(d)
	if err != nil {
		return nil, xerrors.Errorf("failed to create new ipni provider: %w", err)
	}
	ipni_provider.Routes(r, ipp)
	go ipp.StartPublishing(ctx)

	return ipp, nil
}
