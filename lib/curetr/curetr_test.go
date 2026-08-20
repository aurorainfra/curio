package curetr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	format "github.com/ipfs/go-ipld-format"
	"github.com/stretchr/testify/require"
)

type fakeBS struct {
	blk blocks.Block
}

func (f *fakeBS) Get(_ context.Context, c cid.Cid) (blocks.Block, error) {
	if f.blk != nil && f.blk.Cid() == c {
		return f.blk, nil
	}
	return nil, format.ErrNotFound{Cid: c}
}

func (f *fakeBS) Has(_ context.Context, c cid.Cid) (bool, error) {
	return f.blk != nil && f.blk.Cid() == c, nil
}

func TestHandles(t *testing.T) {
	req := func(method, path, accept, query string) *http.Request {
		r := httptest.NewRequest(method, path+query, nil)
		if accept != "" {
			r.Header.Set("Accept", accept)
		}
		return r
	}

	blk := blocks.NewBlock([]byte("hello world"))
	c := blk.Cid().String()

	// served by the fast path
	require.True(t, Handles(req("GET", "/ipfs/"+c, "application/vnd.ipld.raw", "")))
	require.True(t, Handles(req("GET", "/ipfs/"+c, "application/vnd.ipld.raw;", ""))) // ribs-style trailing semicolon
	require.True(t, Handles(req("GET", "/ipfs/"+c, "", "?format=raw")))
	require.True(t, Handles(req("HEAD", "/ipfs/"+c, "application/vnd.ipld.raw", "")))

	// left to frisbii
	require.False(t, Handles(req("GET", "/ipfs/"+c, "application/vnd.ipld.car", "")))
	require.False(t, Handles(req("GET", "/ipfs/"+c, "", "")))
	require.False(t, Handles(req("GET", "/ipfs/"+c+"/sub/path", "application/vnd.ipld.raw", "")))
	require.False(t, Handles(req("POST", "/ipfs/"+c, "application/vnd.ipld.raw", "")))
}

func TestServeRawBlock(t *testing.T) {
	blk := blocks.NewBlock([]byte("hello world"))
	h := NewHandler(&fakeBS{blk: blk})

	// GET hit
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/ipfs/"+blk.Cid().String(), nil)
	r.Header.Set("Accept", "application/vnd.ipld.raw")
	h.ServeHTTP(rec, r)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "application/vnd.ipld.raw", rec.Header().Get("Content-Type"))
	require.Equal(t, `"`+blk.Cid().String()+`.raw"`, rec.Header().Get("Etag"))
	require.Equal(t, "11", rec.Header().Get("Content-Length"))
	require.Equal(t, []byte("hello world"), rec.Body.Bytes())

	// GET miss -> 404
	other := blocks.NewBlock([]byte("other"))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/ipfs/"+other.Cid().String(), nil))
	require.Equal(t, http.StatusNotFound, rec.Code)

	// HEAD hit
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("HEAD", "/ipfs/"+blk.Cid().String(), nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, rec.Body.Bytes())

	// invalid cid -> 400
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/ipfs/notacid", nil))
	require.Equal(t, http.StatusBadRequest, rec.Code)
}
