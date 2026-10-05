package db

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

type txBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Transact runs fn inside a database transaction, committing on success.
func (q *Queries) Transact(ctx context.Context, fn func(q *Queries) error) error {
	b, ok := q.db.(txBeginner)
	if !ok {
		return errors.New("db does not support transactions")
	}
	tx, err := b.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(New(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
