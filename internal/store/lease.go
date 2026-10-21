package store

import (
	"context"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/knotra-dev/engine/internal/execution"
	"github.com/knotra-dev/engine/internal/store/db"
)

// Lease fences service deployments. It uses a dedicated
// connection, so pool_max_conns=1 remains usable by commands and activities.
// PostgreSQL releases the advisory lock when the process/connection disappears.
type Lease struct {
	mu   sync.Mutex
	conn *pgx.Conn
}

func (s *Store) AcquireLease(ctx context.Context) (*Lease, error) {
	conn, err := pgx.ConnectConfig(ctx, s.Pool.Config().ConnConfig.Copy())
	if err != nil {
		return nil, err
	}
	var acquired bool
	acquired, err = db.New(conn).AcquireLease(ctx, "knotra.engine."+s.EngineID)
	if err != nil || !acquired {
		_ = conn.Close(ctx)
		if err == nil {
			err = fmt.Errorf("another engine process is already running against this database")
		}
		return nil, err
	}
	return &Lease{conn: conn}, nil
}

// AcquireServiceLease shares the engine guard with other distributed services.
// Executing services additionally fence their physical host's resource namespace.
func (s *Store) AcquireServiceLease(ctx context.Context, hostID string) (_ *Lease, err error) {
	if hostID != "" {
		if err := execution.ValidateHostID(hostID); err != nil {
			return nil, err
		}
	}
	conn, err := pgx.ConnectConfig(ctx, s.Pool.Config().ConnConfig.Copy())
	if err != nil {
		return nil, err
	}

	defer func() {
		if err != nil {
			_ = conn.Close(context.Background())
		}
	}()
	q := db.New(conn)
	shared, err := q.AcquireSharedServiceLease(ctx, "knotra.engine."+s.EngineID)
	if err != nil {
		return nil, err
	}
	if !shared {
		return nil, fmt.Errorf("database is owned by an exclusive engine process")
	}
	if hostID != "" {
		acquired, err := q.AcquireLease(ctx, "knotra.host."+s.EngineID+"."+hostID)
		if err != nil {
			return nil, err
		}
		if !acquired {
			return nil, fmt.Errorf("another executor is already running on host %q; use a distinct --host-id for each execution host", hostID)
		}
	}
	return &Lease{conn: conn}, nil
}

func (l *Lease) Check(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.conn.Ping(ctx)
}

func (l *Lease) Close(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.conn.Close(ctx)
}
