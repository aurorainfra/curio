package mcp

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/filecoin-project/go-address"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/curio/build"

	"github.com/filecoin-project/lotus/chain/types"
)

const maxWindow = 30 * 24 * time.Hour

// parseTime accepts RFC3339 timestamps, "2006-01-02 15:04:05" (UTC) and
// durations relative to now ("2h", "90m", "-2h" all mean two hours ago).
// An empty string returns nil.
func parseTime(s string) (*time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if s == "now" {
		t := time.Now()
		return &t, nil
	}
	for _, l := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.Parse(l, s); err == nil {
			return &t, nil
		}
	}
	if d, err := time.ParseDuration(strings.TrimPrefix(s, "-")); err == nil {
		t := time.Now().Add(-d)
		return &t, nil
	}
	return nil, xerrors.Errorf("invalid time %q: use RFC3339 (2026-10-18T12:00:00Z) or a duration ago (2h)", s)
}

// timeWindow resolves since/until arguments with defaults, bounded to
// maxWindow.
func timeWindow(since, until string, defSince time.Duration) (time.Time, time.Time, error) {
	end := time.Now()
	if t, err := parseTime(until); err != nil {
		return time.Time{}, time.Time{}, err
	} else if t != nil {
		end = *t
	}
	start := end.Add(-defSince)
	if t, err := parseTime(since); err != nil {
		return time.Time{}, time.Time{}, err
	} else if t != nil {
		start = *t
	}
	if !start.Before(end) {
		return time.Time{}, time.Time{}, xerrors.New("since must be before until")
	}
	if end.Sub(start) > maxWindow {
		start = end.Add(-maxWindow)
	}
	return start, end, nil
}

// spID resolves a storage provider given as f0123 / t0123 / 123, or as a
// robust address looked up on chain.
func (s *Server) spID(ctx context.Context, sp string) (uint64, address.Address, error) {
	sp = strings.TrimSpace(sp)
	if sp == "" {
		return 0, address.Undef, xerrors.New("sp is required (e.g. f01234)")
	}
	if n, err := strconv.ParseUint(sp, 10, 64); err == nil {
		a, err := address.NewIDAddress(n)
		return n, a, err
	}
	a, err := address.NewFromString(sp)
	if err != nil {
		return 0, address.Undef, xerrors.Errorf("invalid sp address %q: %w", sp, err)
	}
	if a.Protocol() != address.ID {
		if s.d.Chain == nil {
			return 0, address.Undef, xerrors.New("no chain connection to resolve a non-ID address; use the f0 ID address")
		}
		a, err = s.d.Chain.StateLookupID(ctx, a, types.EmptyTSK)
		if err != nil {
			return 0, address.Undef, xerrors.Errorf("looking up %s: %w", sp, err)
		}
	}
	id, err := address.IDFromAddress(a)
	return id, a, err
}

func clamp(v, def, maxV int) int {
	if v <= 0 {
		return def
	}
	return min(v, maxV)
}

// epochTime estimates the wall-clock time of an epoch from a reference
// tipset.
func epochTime(refEpoch int64, refTime time.Time, epoch int64) time.Time {
	return refTime.Add(time.Duration(epoch-refEpoch) * time.Duration(build.BlockDelaySecs) * time.Second)
}
