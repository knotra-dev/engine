package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/knotra-dev/engine/internal/store/db"
)

// BindStorage prevents services from mixing storage backends or S3 namespaces.
// Existing filesystem data must be explicitly migrated before selecting object storage.
func (s *Store) BindStorage(ctx context.Context, identity string, remote bool) error {
	if identity == "" {
		return errors.New("storage identity is required")
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		if _, err := q.LockTransactionKey(ctx, "knotra.storage."+s.EngineID); err != nil {
			return err
		}
		_, err := q.ReadStorageBinding(ctx)
		if errors.Is(err, pgx.ErrNoRows) && remote {
			hasData, err := q.HasDurableData(ctx)
			if err != nil {
				return err
			}
			if hasData {
				return errors.New("database has existing filesystem data; migrate durable files before enabling S3 storage")
			}
		} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		_, err = q.BindStorage(ctx, identity)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("durable storage configuration differs from this database's bound store")
		}
		return err
	})
}
