package mcp

import (
	"context"

	"golang.org/x/xerrors"

	"github.com/filecoin-project/curio/harmony/harmonydb"
)

// ro runs catalog queries in a READ ONLY transaction which is always rolled
// back. Queries come only from queries.go; the read-only transaction is a
// second line of defence should a reviewed query (or a function it calls)
// ever try to write.
func (s *Server) ro(ctx context.Context, f func(tx *harmonydb.Tx) error) error {
	_, err := s.d.DB.BeginTransaction(ctx, func(tx *harmonydb.Tx) (bool, error) {
		if _, err := tx.Exec(`SET TRANSACTION READ ONLY`); err != nil {
			return false, xerrors.Errorf("setting read-only transaction: %w", err)
		}
		if _, err := tx.Exec(`SET LOCAL statement_timeout = '120s'`); err != nil {
			return false, xerrors.Errorf("setting statement timeout: %w", err)
		}
		return false, f(tx)
	})
	return err
}
