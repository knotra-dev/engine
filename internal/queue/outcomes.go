package queue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/knotra-dev/engine/internal/contract"
	"github.com/knotra-dev/engine/internal/execution"
	"github.com/knotra-dev/engine/internal/store"
	"github.com/knotra-dev/engine/internal/telemetry"
)

func (r *Runtime) finalize(ctx context.Context, job *river.Job[FinalizeArgs]) error {
	outcome, err := r.Outcomes.GetContext(ctx, job.Args.OutcomeKey)
	if err != nil {
		return err
	}
	if outcome.Ownership.AttemptID != job.Args.Attempt {
		return river.JobCancel(store.ErrConflict)
	}
	trace.SpanFromContext(ctx).SetAttributes(telemetry.OwnershipAttributes(outcome.Ownership)...)
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("knotra.actor.id", r.WorkerID))
	return r.publish(ctx, outcome, true, func(tx pgx.Tx) error {
		_, err := river.JobCompleteTx[*riverpgxv5.Driver](ctx, tx, job)
		return err
	})
}

func (r *Runtime) publish(ctx context.Context, outcome execution.Outcome, recovery bool, complete func(pgx.Tx) error) (err error) {
	attrs := telemetry.OwnershipAttributes(outcome.Ownership)
	attrs = append(attrs, attribute.String("knotra.engine.id", r.Store.EngineID), attribute.String("knotra.host.id", r.HostID),
		attribute.String("knotra.actor.id", r.WorkerID), attribute.Bool("recovery", recovery))
	ctx, span := otel.Tracer("knotra/scheduler").Start(ctx, "outcome.publish", trace.WithAttributes(attrs...))
	defer span.End()
	defer func() {
		span.SetAttributes(attribute.Bool("failed", err != nil))
		telemetry.Event(ctx, telemetry.OwnershipLogger(r.Host.Log, outcome.Ownership), "outcome.publication", err)
		if err != nil {
			schedulerCount(ctx, "publication.failures")
		}
	}()
	if err := r.verifyArtifacts(ctx, outcome); err != nil {
		return err
	}
	plan, err := r.Store.Plan(ctx, outcome.Ownership.RunID)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, r.Store.Pool, func(tx pgx.Tx) error {
		var eligible bool
		var err error
		if recovery {
			eligible, err = store.RecoverOutcome(ctx, tx, outcome)
		} else {
			eligible, err = r.Store.StageOutcome(ctx, tx, outcome)
		}
		if err != nil {
			return err
		}
		if eligible {
			if err := r.advanceTx(ctx, tx, plan, outcome.Ownership.RunID, outcome.Ownership.InstanceID); err != nil {
				return err
			}
		}
		if _, err := store.ReleaseAttemptSlots(ctx, tx, outcome.Ownership, r.Wake); err != nil {
			return err
		}
		return complete(tx)
	})
}

func outcomeArtifacts(outputs contract.Values) []execution.ArtifactReference {
	var artifacts []execution.ArtifactReference
	seen := map[string]bool{}
	keys := make([]string, 0, len(outputs))
	for key := range outputs {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		for _, artifact := range outputs[key].Artifacts {
			if !seen[artifact.ID] {
				seen[artifact.ID] = true
				artifacts = append(artifacts, execution.ArtifactReference{ID: artifact.ID, SHA256: artifact.SHA256, Size: artifact.Size})
			}
		}
	}
	return artifacts
}

func (r *Runtime) verifyArtifacts(ctx context.Context, outcome execution.Outcome) error {
	if err := outcome.Validate(); err != nil {
		return err
	}
	refs := make(map[string]execution.ArtifactReference, len(outcome.Artifacts))
	for _, ref := range outcome.Artifacts {
		data, err := r.Host.Artifacts.Get(ctx, ref.ID)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		if ref.Size != int64(len(data)) || ref.SHA256 != hex.EncodeToString(sum[:]) {
			return fmt.Errorf("outcome artifact %s does not match immutable bytes", ref.ID)
		}
		refs[ref.ID] = ref
	}
	for _, value := range outcome.Outputs {
		for _, artifact := range value.Artifacts {
			ref, found := refs[artifact.ID]
			if !found || ref.SHA256 != artifact.SHA256 || ref.Size != artifact.Size {
				return errors.New("output artifact is not covered by verified outcome references")
			}
		}
	}
	return nil
}
