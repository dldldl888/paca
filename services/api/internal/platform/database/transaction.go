package database

import (
	"context"
	"github.com/jmoiron/sqlx"
)

type transactionKey struct{}

func WithTransaction(ctx context.Context, tx *sqlx.Tx) context.Context {
	return context.WithValue(ctx, transactionKey{}, tx)
}
func Transaction(ctx context.Context) *sqlx.Tx {
	tx, _ := ctx.Value(transactionKey{}).(*sqlx.Tx)
	return tx
}

// CopyTransaction preserves only database ordering state when replacing actor credentials.
func CopyTransaction(dst, src context.Context) context.Context {
	if tx := Transaction(src); tx != nil {
		return WithTransaction(dst, tx)
	}
	return dst
}
