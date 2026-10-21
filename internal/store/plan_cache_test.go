package store

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/knotra-dev/engine/internal/contract"
	"github.com/knotra-dev/engine/internal/protocol"
)

type planQueryCounter struct{ reads atomic.Int32 }

func (c *planQueryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "-- name: ReadPlan :one") {
		c.reads.Add(1)
	}
	return ctx
}

func (*planQueryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func countPlanQueries(t *testing.T, s *Store) *planQueryCounter {
	t.Helper()
	counter := new(planQueryCounter)
	config := s.Pool.Config()
	config.ConnConfig.Tracer = counter
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	s.Pool.Close()
	s.Pool = pool
	return counter
}

func admitPlan(t testing.TB, s *Store, definition string, plan contract.Plan) string {
	t.Helper()
	id := uuid.NewString()
	err := pgx.BeginFunc(t.Context(), s.Pool, func(tx pgx.Tx) error {
		_, err := PutRiverRun(t.Context(), tx, protocol.Run{ID: id, DefinitionID: definition, Status: "pending"}, plan, contract.Values{}, noOpWake)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func BenchmarkStorePlan(b *testing.B) {
	for _, size := range []struct {
		name  string
		bytes int
	}{{"small", 0}, {"1MiB", 1 << 20}} {
		b.Run(size.name, func(b *testing.B) {
			s := testStore(b)
			definition, plan := setupDefinition(b, s)
			plan.Package.Source = strings.Repeat("x", size.bytes)
			id := admitPlan(b, s, definition, plan)
			for _, method := range []struct {
				name string
				read func(context.Context, string) (contract.Plan, error)
			}{
				{"ReadPlan", func(ctx context.Context, id string) (contract.Plan, error) { return ReadPlan(ctx, s.Pool, id) }},
				{"Store.Plan", s.Plan},
			} {
				b.Run(method.name, func(b *testing.B) {
					if _, err := method.read(b.Context(), id); err != nil {
						b.Fatal(err)
					}
					b.ReportAllocs()
					b.ResetTimer()
					for b.Loop() {
						if _, err := method.read(b.Context(), id); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}

func TestPlanCacheReusesAdmittedPlanAndIsolatesRuns(t *testing.T) {
	s := testStore(t)
	definition, first := setupDefinition(t, s)
	first.Profile.Metadata.Name = "first"
	first.ModelDigests = map[string]string{"model": "first-snapshot"}
	firstID := admitPlan(t, s, definition, first)
	second := first
	second.Profile.Metadata.Name = "second"
	second.ModelDigests = map[string]string{"model": "second-snapshot"}
	secondID := admitPlan(t, s, definition, second)
	counter := countPlanQueries(t, s)
	for range 3 {
		for _, run := range []struct{ id, profile, snapshot string }{
			{firstID, "first", "first-snapshot"},
			{secondID, "second", "second-snapshot"},
		} {
			plan, err := s.Plan(t.Context(), run.id)
			if err != nil {
				t.Fatal(err)
			}
			if plan.Digest != first.Digest || plan.Profile.Metadata.Name != run.profile || plan.ModelDigests["model"] != run.snapshot {
				t.Fatalf("run %s lost its admitted profile/snapshot: %+v", run.id, plan)
			}
		}
	}
	if got := counter.reads.Load(); got != 2 {
		t.Fatalf("repeated plan reads issued %d database queries, want 2", got)
	}
}

func TestPlanCacheDoesNotRetainErrorsOrCancelledLoads(t *testing.T) {
	s := testStore(t)
	id, original := setupRun(t, s)
	counter := countPlanQueries(t, s)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Plan(cancelled, id); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled load: %v", err)
	}
	if _, err := s.Pool.Exec(t.Context(), "UPDATE knotra_runs SET plan='[]' WHERE id=$1", id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Plan(t.Context(), id); err == nil {
		t.Fatal("invalid stored plan was accepted")
	}
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(t.Context(), "UPDATE knotra_runs SET plan=$2 WHERE id=$1", id, encoded); err != nil {
		t.Fatal(err)
	}
	plan, err := s.Plan(t.Context(), id)
	if err != nil || plan.Digest != original.Digest {
		t.Fatalf("retry after failed load: digest %q, error %v", plan.Digest, err)
	}
	before := counter.reads.Load()
	if _, err := s.Plan(cancelled, id); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled cache hit: %v", err)
	}
	if _, err := s.Plan(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if got := counter.reads.Load(); got != before {
		t.Fatalf("cached plan performed another query: before %d, after %d", before, got)
	}
}

func TestPlanCacheAdmissionReadsStayTransactional(t *testing.T) {
	s := testStore(t)
	definition, plan := setupDefinition(t, s)
	run := protocol.Run{ID: uuid.NewString(), DefinitionID: definition, Status: "pending"}
	tx, err := s.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if _, err := PutRiverRun(t.Context(), tx, run, plan, contract.Values{}, noOpWake); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPlan(t.Context(), tx, run.ID); err != nil {
		t.Fatalf("transaction cannot read its admitted plan: %v", err)
	}
	if _, err := s.Plan(t.Context(), run.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("uncommitted plan became visible: %v", err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	plan.Profile.Metadata.Name = "committed"
	if err := pgx.BeginFunc(t.Context(), s.Pool, func(tx pgx.Tx) error {
		_, err := PutRiverRun(t.Context(), tx, run, plan, contract.Values{}, noOpWake)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Plan(t.Context(), run.ID)
	if err != nil || loaded.Profile.Metadata.Name != "committed" {
		t.Fatalf("rolled-back or missing plan was cached: profile %q, error %v", loaded.Profile.Metadata.Name, err)
	}
}

func TestPlanCacheBounds(t *testing.T) {
	t.Run("entries", func(t *testing.T) {
		var cache admittedPlanCache
		for i := range 128 {
			cache.store(strconv.Itoa(i), contract.Plan{Digest: "old"}, 1)
		}
		cache.store("new", contract.Plan{Digest: "new"}, 1)
		if _, ok := cache.load("0"); ok {
			t.Fatal("entry limit retained old plans")
		}
		if plan, ok := cache.load("new"); !ok || plan.Digest != "new" {
			t.Fatal("entry limit dropped the incoming plan")
		}
	})
	t.Run("serialized bytes and duplicate loads", func(t *testing.T) {
		var cache admittedPlanCache
		cache.store("first", contract.Plan{Digest: "first"}, 40<<20)
		cache.store("first", contract.Plan{Digest: "first"}, 40<<20)
		cache.store("second", contract.Plan{Digest: "second"}, 24<<20)
		if _, ok := cache.load("first"); !ok {
			t.Fatal("duplicate cold loads consumed the byte budget twice")
		}
		cache.store("third", contract.Plan{Digest: "third"}, 1)
		if _, ok := cache.load("first"); ok {
			t.Fatal("byte limit retained old plans")
		}
		if plan, ok := cache.load("third"); !ok || plan.Digest != "third" {
			t.Fatal("byte limit dropped the incoming plan")
		}
	})
	t.Run("oversize bypass", func(t *testing.T) {
		var cache admittedPlanCache
		cache.store("small", contract.Plan{Digest: "small"}, 1)
		cache.store("large", contract.Plan{Digest: "large"}, 65<<20)
		if _, ok := cache.load("large"); ok {
			t.Fatal("oversize plan was retained")
		}
		if _, ok := cache.load("small"); !ok {
			t.Fatal("oversize bypass evicted the cached plan")
		}
	})
}

func TestPlanCacheConcurrentAccess(t *testing.T) {
	var cache admittedPlanCache
	var group sync.WaitGroup
	for range 16 {
		group.Go(func() {
			for range 100 {
				cache.store("run", contract.Plan{Digest: "frozen"}, 1)
				if plan, ok := cache.load("run"); !ok || plan.Digest != "frozen" {
					t.Error("concurrent reader lost the admitted plan")
				}
			}
		})
	}
	group.Wait()
}

func TestPlanCacheConcurrentColdReads(t *testing.T) {
	s := testStore(t)
	id, original := setupRun(t, s)
	counter := countPlanQueries(t, s)
	start := make(chan struct{})
	errors := make(chan error, 16)
	var group sync.WaitGroup
	for range 16 {
		group.Go(func() {
			<-start
			plan, err := s.Plan(t.Context(), id)
			if err != nil {
				errors <- err
				return
			}
			if plan.Digest != original.Digest || plan.Pipelines[plan.Root] == nil {
				t.Error("cold reader received an incomplete plan")
			}
		})
	}
	close(start)
	group.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	before := counter.reads.Load()
	if _, err := s.Plan(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if after := counter.reads.Load(); after != before {
		t.Fatalf("concurrent cold loads did not populate cache: before %d, after %d", before, after)
	}
}
