package queue

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/metric"

	"github.com/knotra-dev/engine/internal/execution"
	"github.com/knotra-dev/engine/internal/executor"
	"github.com/knotra-dev/engine/internal/store"
	"github.com/knotra-dev/engine/internal/store/db"
)

// Runtime composes the pure scheduler, PostgreSQL mutations and River delivery.
// Create Client with Handlers before Start. Stop it before closing the store.
type Runtime struct {
	Store    *store.Store
	Host     *executor.Host
	Outcomes *execution.OutcomeFiles
	Client   *Client
	WorkerID string
	HostID   string
	Failure  chan error

	workCtx context.Context
	cancel  context.CancelCauseFunc
	done    chan struct{}

	// ponytail: scan 64 expired outcome keys per tick; the cursor prevents a
	// missing envelope from starving later keys. Partition if scans dominate.
	outcomeCursor string

	timerCursor     string
	deliveryCursor  string
	resourceCursor  string
	containerCursor string
	sessionCursor   string
}

func (r *Runtime) Handlers() Handlers {
	return Handlers{Advance: r.advance, Execute: r.execute, Finalize: r.finalize, Cleanup: r.cleanup}
}

// Done closes when maintenance stops, including loss of the worker heartbeat.
func (r *Runtime) Done() <-chan struct{} { return r.done }

func (r *Runtime) Err() error {
	if r.workCtx == nil {
		return nil
	}
	return context.Cause(r.workCtx)
}

func (r *Runtime) Start(ctx context.Context) error {
	if r.Store == nil || r.Host == nil || r.Host.Runner == nil || r.Outcomes == nil || r.Client == nil {
		return errors.New("runtime requires store, host, outcomes and queue client")
	}
	if r.workCtx != nil {
		return errors.New("runtime process incarnation cannot be restarted")
	}
	if r.HostID != r.Client.hostID || r.Store.EngineID != r.Client.engineID {
		return errors.New("runtime queue does not match its engine and host identity")
	}
	if (r.Host.Runner.EngineID != "" && r.Host.Runner.EngineID != r.Store.EngineID) ||
		(r.Host.Runner.HostID != "" && r.Host.Runner.HostID != r.HostID) {
		return errors.New("runtime runner does not match its engine and host identity")
	}

	r.Host.Runner.EngineID, r.Host.Runner.HostID = r.Store.EngineID, r.HostID
	r.Host.Runner.ResourceEvidence = r.Outcomes
	if err := r.Store.RegisterWorker(ctx, r.WorkerID, r.HostID, r.Client.role); err != nil {
		return err
	}
	if err := r.Store.BindWorkerDeliveryClient(ctx, r.WorkerID, r.Client.River.ID()); err != nil {
		return err
	}

	r.Client.logger = r.Client.logger.With("actorId", r.WorkerID)
	if r.Host.Log == nil {
		r.Host.Log = r.Client.logger
	}
	r.workCtx, r.cancel = context.WithCancelCause(context.WithoutCancel(ctx))
	r.Failure = make(chan error, 1)
	r.done = make(chan struct{})
	registration, err := r.observeMetrics()
	if err != nil {
		r.cancel(err)
		close(r.done)
		return err
	}
	if err := r.Client.Start(r.workCtx); err != nil {
		_ = registration.Unregister()
		r.cancel(err)
		close(r.done)
		return err
	}

	go r.maintain(registration)
	return nil
}

func (r *Runtime) Stop(ctx context.Context) error {
	if r.cancel == nil {
		return nil
	}
	drainErr := r.Store.DrainWorker(ctx, r.WorkerID)
	if drainErr == nil && r.Client.executes() && r.workCtx.Err() == nil {
		drainErr = r.drainSessions(ctx)
	}
	err := r.Client.Stop(ctx)
	r.cancel(context.Canceled)
	select {
	case <-r.done:
	case <-ctx.Done():
		return errors.Join(drainErr, err, ctx.Err())
	}
	return errors.Join(drainErr, err)
}

func (r *Runtime) drainSessions(ctx context.Context) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		active, err := db.New(r.Store.Pool).HasLiveSessionRuns(ctx, new(r.WorkerID))
		if err != nil || !active {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.workCtx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (r *Runtime) maintain(registration metric.Registration) {
	pollsDone := make(chan struct{})
	go func() {
		defer close(pollsDone)
		var polls []func(context.Context) error
		if r.Client.schedules() {
			polls = append(polls, r.pumpTimers, r.reconcileOutcomes, r.reconcileDeliveries)
		}
		if r.Client.executes() {
			polls = append(polls, r.reconcileResources, r.reconcilePhysicalResources, r.reconcileSessions)
		}
		if len(polls) == 0 {
			return
		}

		timer := time.NewTicker(time.Second)
		defer timer.Stop()
		for {
			select {
			case <-r.workCtx.Done():
				return
			case <-timer.C:
			}
			// Each reconciliation has its own bound and retries independently.
			// A bad envelope or busy run must not suppress unrelated cleanup.
			for _, poll := range polls {
				ctx, cancel := context.WithTimeout(r.workCtx, execution.WorkerHeartbeat)
				err := poll(ctx)
				cancel()
				if err != nil && r.workCtx.Err() == nil {
					select {
					case r.Failure <- err:
					default:
					}
				}
			}
		}
	}()
	defer func() {
		<-pollsDone
		_ = registration.Unregister()
		close(r.done)
	}()
	heartbeat := time.NewTicker(execution.WorkerHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.workCtx.Done():
			return
		case <-heartbeat.C:
			ctx, cancel := context.WithTimeout(r.workCtx, execution.WorkerHeartbeat)
			err := r.Store.HeartbeatWorker(ctx, r.WorkerID)
			cancel()
			if err != nil {
				select {
				case r.Failure <- err:
				default:
				}
				r.cancel(err)
				return
			}
		}
	}
}
