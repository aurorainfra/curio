package retrieval

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/go-chi/chi/v5"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	logging "github.com/ipfs/go-log/v2"
	"github.com/ipld/frisbii"
	"github.com/ipld/go-ipld-prime"
	"go.opencensus.io/stats"
	"go.opencensus.io/tag"

	"github.com/filecoin-project/curio/build"
	"github.com/filecoin-project/curio/deps/config"
	"github.com/filecoin-project/curio/harmony/harmonydb"
	"github.com/filecoin-project/curio/lib/cachedreader"
	"github.com/filecoin-project/curio/lib/curetr"
	"github.com/filecoin-project/curio/market/denylist"
	"github.com/filecoin-project/curio/market/indexstore"
	"github.com/filecoin-project/curio/market/retrieval/blockcache"
	"github.com/filecoin-project/curio/market/retrieval/remoteblockstore"

	"github.com/filecoin-project/lotus/blockstore"
)

var log = logging.Logger("retrievals")

// activeRequestCounters stores atomic counters for active requests per path+method
var activeRequestCounters sync.Map // map[string]*atomic.Int64

// Fallback limiter caps, used when no config is wired (tests) or a
// configured value is not positive. Live values come from
// HTTP.RetrievalMaxParallelRequests and HTTP.BulkRetrieval.MaxConcurrentStreams.
const (
	defaultMaxParallelRequests = 256
	// bulk streams each drive up to a maxRange read buffer and a full
	// sequential disk stream; a few dozen saturate any deployment's disks
	defaultBulkStreams = 32
)

// dynLimiter rejects requests over a live-readable concurrency limit with
// HTTP 429 (non-blocking, same semantics as the old channel semaphores,
// but the limit can change at runtime).
type dynLimiter struct {
	active atomic.Int64
	limit  func() int
}

func (l *dynLimiter) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if l.active.Add(1) > int64(l.limit()) {
			l.active.Add(-1)
			log.Warnw("Request limit reached", "method", r.Method, "path", r.URL.Path)
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("Service temporarily unavailable: too many concurrent requests"))
			return
		}
		defer l.active.Add(-1)
		next.ServeHTTP(w, r)
	})
}

type limiters struct {
	ipfs, ipfsHead, piece, bulk *dynLimiter
}

func newLimiters(httpCfg *config.HTTPConfig) limiters {
	retr := func() int { return defaultMaxParallelRequests }
	bulk := func() int { return defaultBulkStreams }
	if httpCfg != nil {
		if d := httpCfg.RetrievalMaxParallelRequests; d != nil {
			retr = func() int {
				if v := d.Get(); v > 0 {
					return v
				}
				return defaultMaxParallelRequests
			}
		}
		if d := httpCfg.BulkRetrieval.MaxConcurrentStreams; d != nil {
			bulk = func() int {
				if v := d.Get(); v > 0 {
					return v
				}
				return defaultBulkStreams
			}
		}
	}
	return limiters{
		ipfs:     &dynLimiter{limit: retr},
		ipfsHead: &dynLimiter{limit: func() int { return max(1, retr()/2) }},
		piece:    &dynLimiter{limit: retr},
		bulk:     &dynLimiter{limit: bulk},
	}
}

type Provider struct {
	db   *harmonydb.DB
	bs   *remoteblockstore.RemoteBlockstore
	fr   *frisbii.HttpIpfs
	raw  *curetr.Handler     // fast path for explicit raw single-block requests
	bulk *curetr.BulkHandler // batched sequential block streaming
	cpr  *cachedreader.CachedPieceReader
	lim  limiters
}

const (
	piecePrefix = "/piece/"
	ipfsPrefix  = "/ipfs/"
	infoPage    = "/info"
)

func NewRetrievalProvider(ctx context.Context, db *harmonydb.DB, idxStore *indexstore.IndexStore, cpr *cachedreader.CachedPieceReader, df *denylist.Filter, offsetCacheMemMiB int, bcCfg *blockcache.Config, httpCfg *config.HTTPConfig) *Provider {
	bs := remoteblockstore.NewRemoteBlockstore(idxStore, db, cpr, offsetCacheMemMiB)

	// Wrap the blockstore with denylist filtering so every block fetch
	// (including interior DAG nodes) is checked against the denylist.
	fbs := denylist.NewFilteredBlockstore(bs, df)

	// Block cache: size-partitioned, byte-budgeted, ghost-set admission
	// (see blockcache package doc); nil bcCfg disables caching entirely.
	var cbs blockstore.Blockstore = blockstore.Adapt(fbs)
	if bcCfg != nil {
		cbs = blockstore.NewReadCachedBlockstore(cbs, &BlockstoreCacheWrap[blockstore.MhString]{Sub: blockcache.New(*bcCfg)})
	}

	lsys := LinkSystemForBlockstore(cbs)
	fr := frisbii.NewHttpIpfs(ctx, lsys, frisbii.WithBlockHasCheck(cbs.Has))

	// The bulk handler checks the denylist per block itself: bulk requests
	// carry cids in the body, so the path-based denylist middleware never
	// sees them, and the blockstore wrapper is bypassed by design (bulk
	// reads merged ranges through the piece reader, not per-block Gets).
	denyCheck := func(c cid.Cid) error {
		denied, ready := df.IsDenied(c)
		if !ready {
			return fmt.Errorf("denylist not yet loaded")
		}
		if denied {
			return denylist.ErrBlockDenied
		}
		return nil
	}

	var bulkCfg *config.BulkRetrievalConfig
	if httpCfg != nil {
		bulkCfg = &httpCfg.BulkRetrieval
	}

	return &Provider{
		db:   db,
		bs:   bs,
		fr:   fr,
		raw:  curetr.NewHandler(cbs),
		bulk: curetr.NewBulkHandler(bs, cpr, denyCheck, bulkCfg),
		cpr:  cpr,
		lim:  newLimiters(httpCfg),
	}
}

// NewRetrievalProviderWithLinkSystem creates a Provider with a custom LinkSystem for testing
func NewRetrievalProviderWithLinkSystem(ctx context.Context, lsys ipld.LinkSystem, withBlockHasCheck func(context.Context, cid.Cid) (bool, error)) *Provider {
	fr := frisbii.NewHttpIpfs(ctx, lsys, frisbii.WithBlockHasCheck(withBlockHasCheck))

	return &Provider{
		fr:  fr,
		lim: newLimiters(nil),
	}
}

// responseWriterWrapper wraps http.ResponseWriter to capture status code and bytes written
type responseWriterWrapper struct {
	http.ResponseWriter
	statusCode   int
	bytesWritten int64
	isHead       bool
}

func (rw *responseWriterWrapper) WriteHeader(statusCode int) {
	rw.statusCode = statusCode
	rw.ResponseWriter.WriteHeader(statusCode)
}

func (rw *responseWriterWrapper) Write(b []byte) (int, error) {
	n, err := rw.ResponseWriter.Write(b)

	if !rw.isHead {
		rw.bytesWritten += int64(n)
	}

	return n, err
}

// getPathPrefix extracts the first path element to avoid cardinality explosion in metrics
// Examples: "/piece/abc123" -> "/piece/", "/ipfs/QmABC" -> "/ipfs/", "/info" -> "/info", "/" -> "/"
func getPathPrefix(urlPath string) string {
	// Clean the path to remove any .. or . elements
	cleaned := path.Clean(urlPath)

	if cleaned == "/" || cleaned == "" {
		return "/"
	}

	// Split the path and get the first element
	parts := strings.Split(strings.TrimPrefix(cleaned, "/"), "/")
	if len(parts) > 0 && parts[0] != "" {
		return "/" + parts[0] + "/"
	}

	return "/"
}

// getActiveRequestCounter gets or creates an atomic counter for a specific path+method combination
func getActiveRequestCounter(pathPrefix, method string) *atomic.Int64 {
	key := pathPrefix + ":" + method

	// Try to load existing counter
	if counter, ok := activeRequestCounters.Load(key); ok {
		return counter.(*atomic.Int64)
	}

	// Create new counter
	counter := &atomic.Int64{}
	actual, loaded := activeRequestCounters.LoadOrStore(key, counter)
	if loaded {
		return actual.(*atomic.Int64)
	}
	return counter
}

// incrementActiveRequests atomically increments the active request counter and records the metric
func incrementActiveRequests(ctx context.Context, pathPrefix, method string) *atomic.Int64 {
	counter := getActiveRequestCounter(pathPrefix, method)
	newValue := counter.Add(1)

	// Record the new active request count
	_ = stats.RecordWithTags(ctx, []tag.Mutator{
		tag.Upsert(remoteblockstore.HttpPathKey, pathPrefix),
		tag.Upsert(remoteblockstore.HttpMethodKey, method),
	}, remoteblockstore.HttpActiveRequests.M(newValue))

	return counter
}

// decrementActiveRequests atomically decrements the active request counter and records the metric
func decrementActiveRequests(ctx context.Context, counter *atomic.Int64, pathPrefix, method string) {
	newValue := counter.Add(-1)

	// Record the new active request count
	_ = stats.RecordWithTags(ctx, []tag.Mutator{
		tag.Upsert(remoteblockstore.HttpPathKey, pathPrefix),
		tag.Upsert(remoteblockstore.HttpMethodKey, method),
	}, remoteblockstore.HttpActiveRequests.M(newValue))
}

// metricsMiddleware records HTTP metrics for requests
func metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Extract path prefix to avoid unbounded cardinality in Prometheus
		pathPrefix := getPathPrefix(r.URL.Path)

		// Record request count and increment active requests
		ctx, _ := tag.New(r.Context(), tag.Upsert(remoteblockstore.HttpPathKey, pathPrefix), tag.Upsert(remoteblockstore.HttpMethodKey, r.Method))
		stats.Record(ctx, remoteblockstore.HttpRequestCount.M(1))

		// Increment active requests counter
		counter := incrementActiveRequests(ctx, pathPrefix, r.Method)

		// Ensure we decrement when done
		defer decrementActiveRequests(ctx, counter, pathPrefix, r.Method)

		// Wrap response writer to capture status and bytes
		wrapper := &responseWriterWrapper{
			ResponseWriter: w,
			statusCode:     http.StatusOK, // default if WriteHeader is not called
			isHead:         r.Method == http.MethodHead,
		}

		// Serve the request
		next.ServeHTTP(wrapper, r.WithContext(ctx))

		// Record response metrics
		statusCodeStr := strconv.Itoa(wrapper.statusCode)
		_ = stats.RecordWithTags(ctx, []tag.Mutator{
			tag.Upsert(remoteblockstore.HttpStatusCodeKey, statusCodeStr),
			tag.Upsert(remoteblockstore.HttpPathKey, pathPrefix),
			tag.Upsert(remoteblockstore.HttpMethodKey, r.Method),
		},
			remoteblockstore.HttpResponseStatusCount.M(1),
			remoteblockstore.HttpResponseBytesCount.M(wrapper.bytesWritten),
		)

		log.Debugw("HTTP request", "method", r.Method, "path", r.URL.Path, "pathPrefix", pathPrefix, "status", wrapper.statusCode, "bytes", wrapper.bytesWritten)
	})
}

func Router(mux *chi.Mux, rp *Provider, df *denylist.Filter) {
	// Group retrieval routes with metrics middleware
	mux.Group(func(r chi.Router) {
		r.Use(metricsMiddleware)

		// Piece endpoint with denylist and limiter
		r.Group(func(r chi.Router) {
			r.Use(denylist.Middleware(df))
			r.Use(rp.lim.piece.middleware)
			r.Get(piecePrefix+"{cid}", rp.handleByPieceCid)
			r.Head(piecePrefix+"{cid}", rp.handleByPieceCid)
		})

		// IPFS endpoints with denylist and limiter
		r.Group(func(r chi.Router) {
			r.Use(denylist.Middleware(df))
			r.Use(rp.lim.ipfsHead.middleware)
			r.Head(ipfsPrefix+"*", rp.serveIpfs)
		})

		r.Group(func(r chi.Router) {
			r.Use(denylist.Middleware(df))
			r.Use(rp.lim.ipfs.middleware)
			r.Get(ipfsPrefix+"*", rp.serveIpfs)
		})

		// Bulk block endpoints. The request carries cids in the body, so
		// the path-based denylist middleware does not apply — the handler
		// checks the denylist per block.
		r.Group(func(r chi.Router) {
			r.Use(rp.lim.bulk.middleware)
			r.Post(curetr.BulkBlocksPath, rp.serveBulkBlocks)
		})
		r.Get(curetr.BulkInfoPath, rp.serveBulkInfo)

		// Info endpoint without limiter or denylist
		r.Get(infoPage, handleInfo)
	})
}

func (rp *Provider) serveBulkBlocks(w http.ResponseWriter, r *http.Request) {
	if rp.bulk == nil || !rp.bulk.Enabled() {
		http.Error(w, "bulk retrieval not available", http.StatusNotFound)
		return
	}
	rp.bulk.ServeBlocks(w, r)
}

func (rp *Provider) serveBulkInfo(w http.ResponseWriter, r *http.Request) {
	if rp.bulk == nil || !rp.bulk.Enabled() {
		http.Error(w, "bulk retrieval not available", http.StatusNotFound)
		return
	}
	rp.bulk.ServeInfo(w, r)
}

// serveIpfs routes explicit raw single-block requests to the curetr fast
// path (no IPLD LinkSystem involved) and everything else to frisbii. The
// same denylist-filtered, read-cached blockstore backs both, so behavior
// only differs in serving overhead.
func (rp *Provider) serveIpfs(w http.ResponseWriter, r *http.Request) {
	if rp.raw != nil && curetr.Handles(r) {
		stats.Record(r.Context(), remoteblockstore.HttpIpfsRawFastpathCount.M(1))
		rp.raw.ServeHTTP(w, r)
		return
	}
	stats.Record(r.Context(), remoteblockstore.HttpIpfsFrisbiiCount.M(1))
	rp.fr.ServeHTTP(w, r)
}

func handleInfo(rw http.ResponseWriter, r *http.Request) {
	infoOut := fmt.Sprintf(`{"Version":"0.4.0", "Server": "Curio/%s"}`, build.BuildVersion)
	_, _ = rw.Write([]byte(infoOut))
}

type BlockstoreCacheWrap[T any] struct {
	Sub interface {
		Remove(mhString T)
		Contains(mhString T) bool
		Get(mhString T) (blocks.Block, bool)
		Add(mhString T, block blocks.Block)
	}
}

func (b *BlockstoreCacheWrap[T]) Contains(mhString T) bool {
	return b.Sub.Contains(mhString)
}

func (b *BlockstoreCacheWrap[T]) Get(mhString T) (blocks.Block, bool) {
	block, ok := b.Sub.Get(mhString)
	if ok {
		stats.Record(context.Background(), remoteblockstore.BlockstoreCacheHits.M(1))
	} else {
		stats.Record(context.Background(), remoteblockstore.BlockstoreCacheMisses.M(1))
	}
	return block, ok
}

func (b *BlockstoreCacheWrap[T]) Remove(mhString T) bool {
	b.Sub.Remove(mhString)
	return true
}

func (b *BlockstoreCacheWrap[T]) Add(mhString T, block blocks.Block) (evicted bool) {
	b.Sub.Add(mhString, block)
	return true
}

var _ blockstore.BlockstoreCache = (*BlockstoreCacheWrap[blockstore.MhString])(nil)
