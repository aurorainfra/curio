// Package curetr is the fast raw-block serving path from the curetr
// standalone retrieval server, integrated into curio: explicit raw
// single-block /ipfs/<cid> requests are served straight from the blockstore,
// skipping frisbii's IPLD LinkSystem machinery (per-request link system
// traversal, content negotiation and response framing). Everything else —
// CAR / dag-scope requests, subpaths, other Accept types — stays on frisbii.
//
// Response headers match frisbii's raw block responses, so clients cannot
// tell which path served them; the fast path additionally sets
// Content-Length (the block is fully in memory).
package curetr

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	format "github.com/ipfs/go-ipld-format"
	logging "github.com/ipfs/go-log/v2"
)

var log = logging.Logger("curetr")

// Blockstore is the read side served by the raw-block handler.
type Blockstore interface {
	Get(ctx context.Context, c cid.Cid) (blocks.Block, error)
	Has(ctx context.Context, c cid.Cid) (bool, error)
}

// Handler serves explicit raw single-block /ipfs requests.
type Handler struct {
	bs Blockstore
}

func NewHandler(bs Blockstore) *Handler {
	return &Handler{bs: bs}
}

// Handles reports whether the request is an explicit raw single-block
// request the fast path serves; anything else should go to the full
// trustless gateway implementation.
func Handles(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	path := strings.TrimPrefix(r.URL.Path, "/ipfs/")
	if path == "" || strings.Contains(path, "/") {
		return false
	}
	if r.URL.Query().Get("format") == "raw" {
		return true
	}
	return strings.Contains(r.Header.Get("Accept"), "application/vnd.ipld.raw")
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c, err := cid.Parse(strings.TrimPrefix(r.URL.Path, "/ipfs/"))
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid cid: %s", err), http.StatusBadRequest)
		return
	}

	if r.Method == http.MethodHead {
		has, err := h.bs.Has(r.Context(), c)
		if err != nil {
			log.Warnw("raw block head failed", "cid", c, "err", err)
			http.Error(w, "block check failed", http.StatusInternalServerError)
			return
		}
		if !has {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		setRawHeaders(w, c)
		w.WriteHeader(http.StatusOK)
		return
	}

	blk, err := h.bs.Get(r.Context(), c)
	if err != nil {
		if format.IsNotFound(err) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		log.Warnw("raw block get failed", "cid", c, "err", err)
		http.Error(w, "block retrieval failed", http.StatusInternalServerError)
		return
	}

	data := blk.RawData()
	setRawHeaders(w, c)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

// setRawHeaders mirrors frisbii's raw block response headers.
func setRawHeaders(w http.ResponseWriter, c cid.Cid) {
	h := w.Header()
	h.Set("Content-Type", "application/vnd.ipld.raw")
	h.Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.bin"`, c.String()))
	h.Set("Cache-Control", "public, max-age=29030400, immutable")
	h.Set("Etag", `"`+c.String()+`.raw"`)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Ipfs-Path", "/ipfs/"+c.String())
	h.Set("X-Ipfs-Roots", c.String())
	h.Set("Vary", "Accept, Accept-Encoding")
}
