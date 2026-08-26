package retrieval

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDynLimiter(t *testing.T) {
	var limit atomic.Int64
	limit.Store(1)
	l := &dynLimiter{limit: func() int { return int(limit.Load()) }}

	block := make(chan struct{})
	started := make(chan struct{}, 4)
	h := l.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-block
	}))

	serve := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
		return rec
	}
	waitStarted := func() {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("request did not start")
		}
	}

	// first request holds the only slot
	go serve()
	waitStarted()

	// second is rejected while the limit is 1
	require.Equal(t, http.StatusTooManyRequests, serve().Code)

	// raising the limit takes effect immediately, no restart or new limiter
	limit.Store(2)
	go serve()
	waitStarted()

	// both slots busy again
	require.Equal(t, http.StatusTooManyRequests, serve().Code)

	close(block)
}
