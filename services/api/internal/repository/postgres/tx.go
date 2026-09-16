package postgres

import (
	"context"
	"fmt"

	"github.com/Paca-AI/api/internal/platform/database"
	"github.com/jmoiron/sqlx"
)

// WithTx executes fn inside a database transaction.  The transaction is
// committed if fn returns nil, and rolled back on any error.
func WithTx(ctx context.Context, db *sqlx.DB, fn func(tx *sqlx.Tx) error) error {
	if tx := database.Transaction(ctx); tx != nil {
		return fn(tx)
	}
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("tx: begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err = fn(tx); err != nil {
		return fmt.Errorf("tx: %w", err)
	}
	return tx.Commit()
}
