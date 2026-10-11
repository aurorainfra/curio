// trimhistory deletes old rows from harmony_task_history in small, parallel,
// keyset-paginated batches.
//
// It talks to the database directly (pgx, no harmonydb), so running it does not
// apply Curio schema migrations - the point is to shrink the table before an
// upgrade. It is safe to run while Curio is running: rows are only selected
// below a fixed cutoff, and new history rows are always written after it.
//
// Pagination follows the (work_end DESC, ...) history indexes, never the hash
// sharded id primary key, and each slice walks down from its upper bound with a
// keyset cursor so already-deleted rows (tombstones until compaction) are not
// rescanned. Rows are deleted by primary key point lookups.
//
// Connection settings default to the same CURIO_DB_* environment variables
// Curio uses (see /etc/curio.env).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/yugabyte/pgx/v5"
	"github.com/yugabyte/pgx/v5/pgconn"
	"github.com/yugabyte/pgx/v5/pgxpool"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

type slice struct {
	lo, hi time.Time // delete work_end in [lo, hi)
}

func main() {
	var (
		hosts    = flag.String("db-host", envOr("CURIO_DB_HOST", "127.0.0.1"), "database host(s), comma separated (CURIO_DB_HOST)")
		port     = flag.String("db-port", envOr("CURIO_DB_PORT", "5433"), "database port (CURIO_DB_PORT)")
		dbName   = flag.String("db-name", envOr("CURIO_DB_NAME", "yugabyte"), "database name (CURIO_DB_NAME)")
		user     = flag.String("db-user", envOr("CURIO_DB_USER", "yugabyte"), "database user (CURIO_DB_USER)")
		password = flag.String("db-password", envOr("CURIO_DB_PASSWORD", "yugabyte"), "database password (CURIO_DB_PASSWORD)")
		schema   = flag.String("schema", "curio", "schema holding the Curio tables")
		keep     = flag.Duration("keep", 24*time.Hour, "keep history rows whose work_end is within this duration of now")
		batch    = flag.Int("batch", 2000, "rows deleted per statement")
		workers  = flag.Int("workers", 8, "parallel delete workers")
		slices   = flag.Int("slices", 0, "time slices to split the work into (default workers*4)")
		stmtTO   = flag.Duration("statement-timeout", 2*time.Minute, "per-statement timeout")
		reallyDo = flag.Bool("really-do-it", false, "actually delete; without it only the plan (and optionally a count) is printed")
		doCount  = flag.Bool("count", false, "in dry-run mode, count the rows that would be deleted (full index range scan)")
	)
	flag.Parse()

	if *batch <= 0 || *workers <= 0 || *keep <= 0 {
		fatalf("batch, workers and keep must be positive")
	}
	if *slices <= 0 {
		*slices = *workers * 4
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	connStr := fmt.Sprintf("postgresql://%s:%s@%s:%s/%s?search_path=%s&pool_max_conns=%d",
		url.QueryEscape(*user), url.QueryEscape(*password), strings.Split(*hosts, ",")[0], *port, *dbName,
		url.QueryEscape(*schema), *workers+1)
	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		fatalf("connecting: %s", err)
	}
	defer pool.Close()

	cutoff := time.Now().Add(-*keep).UTC()

	var oldest *time.Time
	if err := pool.QueryRow(ctx, `SELECT min(work_end) FROM harmony_task_history`).Scan(&oldest); err != nil {
		fatalf("finding oldest history row: %s", err)
	}
	if oldest == nil || !oldest.Before(cutoff) {
		fmt.Printf("nothing to trim: oldest work_end %v, cutoff %s\n", oldest, cutoff.Format(time.RFC3339))
		return
	}
	fmt.Printf("trimming harmony_task_history: work_end in [%s, %s), keeping last %s\n",
		oldest.UTC().Format(time.RFC3339), cutoff.Format(time.RFC3339), *keep)

	if !*reallyDo {
		var plan []string
		rows, err := pool.Query(ctx, `EXPLAIN `+selectBatchSQL, *oldest, cutoff, cutoff, *batch)
		if err != nil {
			fatalf("explain: %s", err)
		}
		for rows.Next() {
			var l string
			if err := rows.Scan(&l); err != nil {
				fatalf("explain scan: %s", err)
			}
			plan = append(plan, "  "+l)
		}
		if err := rows.Err(); err != nil {
			fatalf("explain: %s", err)
		}
		fmt.Printf("batch select plan (should be an index scan on work_end):\n%s\n", strings.Join(plan, "\n"))
		if *doCount {
			var n int64
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM harmony_task_history WHERE work_end < $1`, cutoff).Scan(&n); err != nil {
				fatalf("counting: %s", err)
			}
			fmt.Printf("rows to delete: %d\n", n)
		}
		fmt.Println("dry run; pass --really-do-it to delete")
		return
	}

	// Split [oldest, cutoff) into equal time slices. Old history tends to be
	// denser or sparser in places; more slices than workers evens that out.
	work := make(chan slice, *slices)
	span := cutoff.Sub(*oldest)
	step := span / time.Duration(*slices)
	if step <= 0 {
		step = span
	}
	for lo := *oldest; lo.Before(cutoff); lo = lo.Add(step) {
		hi := lo.Add(step)
		if hi.After(cutoff) || cutoff.Sub(hi) < step/2 {
			hi = cutoff
		}
		work <- slice{lo: lo, hi: hi}
		if hi.Equal(cutoff) {
			break
		}
	}
	close(work)
	totalSlices := len(work)

	var deleted, slicesDone, retries atomic.Int64
	start := time.Now()

	progressDone := make(chan struct{})
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-progressDone:
				return
			case <-t.C:
				d := deleted.Load()
				el := time.Since(start).Seconds()
				fmt.Printf("deleted %d rows (%.0f rows/s), slices %d/%d, retries %d\n",
					d, float64(d)/el, slicesDone.Load(), totalSlices, retries.Load())
			}
		}
	}()

	var wg sync.WaitGroup
	errCh := make(chan error, *workers)
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for s := range work {
				if err := trimSlice(ctx, pool, s, *batch, *stmtTO, &deleted, &retries); err != nil {
					errCh <- err
					cancel()
					return
				}
				slicesDone.Add(1)
			}
		}()
	}
	wg.Wait()
	close(progressDone)
	close(errCh)

	el := time.Since(start)
	fmt.Printf("done: deleted %d rows in %s (%.0f rows/s), %d retries\n",
		deleted.Load(), el.Round(time.Second), float64(deleted.Load())/el.Seconds(), retries.Load())
	if err := <-errCh; err != nil {
		fatalf("trim failed (already deleted rows stay deleted; rerun to continue): %s", err)
	}
	if ctx.Err() != nil {
		fatalf("interrupted; rerun to continue")
	}
}

// selectBatchSQL picks the next batch of a slice, newest first, at or below the
// keyset cursor. $1 = slice lo, $2 = cursor, $3 = slice hi, $4 = batch size.
const selectBatchSQL = `SELECT id FROM harmony_task_history
	WHERE work_end >= $1 AND work_end <= $2 AND work_end < $3
	ORDER BY work_end DESC
	LIMIT $4`

// deleteBatchSQL deletes one batch by primary key and returns the work_end of
// each deleted row so the caller can advance the cursor.
const deleteBatchSQL = `DELETE FROM harmony_task_history
	WHERE id = ANY(ARRAY(` + selectBatchSQL + `))
	RETURNING work_end`

func trimSlice(ctx context.Context, pool *pgxpool.Pool, s slice, batch int, stmtTO time.Duration, deleted, retries *atomic.Int64) error {
	// The cursor uses <= so rows sharing the boundary work_end with the last
	// batch are not skipped; only those few rows' tombstones are rescanned.
	cursor := s.hi
	backoff := 100 * time.Millisecond

	for {
		if ctx.Err() != nil {
			return nil
		}

		n, minEnd, err := deleteBatch(ctx, pool, s, cursor, batch, stmtTO)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if retryable(err) && backoff < time.Minute {
				retries.Add(1)
				time.Sleep(backoff)
				backoff *= 2
				continue
			}
			return fmt.Errorf("slice [%s, %s): %w", s.lo.Format(time.RFC3339), s.hi.Format(time.RFC3339), err)
		}
		backoff = 100 * time.Millisecond

		deleted.Add(int64(n))
		if n < batch {
			// The scan ran out of rows in this slice.
			return nil
		}
		cursor = minEnd
	}
}

func deleteBatch(ctx context.Context, pool *pgxpool.Pool, s slice, cursor time.Time, batch int, stmtTO time.Duration) (int, time.Time, error) {
	sctx, cancel := context.WithTimeout(ctx, stmtTO)
	defer cancel()

	rows, err := pool.Query(sctx, deleteBatchSQL, s.lo, cursor, s.hi, batch)
	if err != nil {
		return 0, time.Time{}, err
	}
	defer rows.Close()

	var n int
	minEnd := cursor
	for rows.Next() {
		var we time.Time
		if err := rows.Scan(&we); err != nil {
			return 0, time.Time{}, err
		}
		if we.Before(minEnd) {
			minEnd = we
		}
		n++
	}
	return n, minEnd, rows.Err()
}

// retryable reports transient errors: Yugabyte transaction conflicts/restarts,
// statement timeouts and dropped connections.
func retryable(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, pgx.ErrTxClosed) {
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "40001", "40P01", "57014", "08000", "08003", "08006", "XX000":
			return true
		}
		return false
	}
	return pgconn.SafeToRetry(err) || pgconn.Timeout(err)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "trimhistory: "+format+"\n", args...)
	os.Exit(1)
}
