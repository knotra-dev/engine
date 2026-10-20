package store

import (
	"context"
	"testing"
)

func TestDistributedServiceLeasesFenceHostsAndExclusiveProcess(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	first, err := s.AcquireServiceLease(ctx, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close(ctx) }()
	for _, host := range []string{"worker-2", "", ""} {
		peer, err := s.AcquireServiceLease(ctx, host)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = peer.Close(ctx) }()
	}
	if duplicate, err := s.AcquireServiceLease(ctx, "worker-1"); err == nil {
		_ = duplicate.Close(ctx)
		t.Fatal("duplicate execution host acquired the same resource namespace")
	}
	if exclusive, err := s.AcquireLease(ctx); err == nil {
		_ = exclusive.Close(ctx)
		t.Fatal("exclusive service acquired a database with running distributed services")
	}
	if err := first.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(ctx); err != nil {
		t.Fatal(err)
	}
	replacement, err := s.AcquireServiceLease(ctx, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	_ = replacement.Close(ctx)
}

func TestExclusiveLeaseRejectsDistributedService(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	lease, err := s.AcquireLease(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Close(ctx) }()
	for _, host := range []string{"worker-1", ""} {
		if other, err := s.AcquireServiceLease(ctx, host); err == nil {
			_ = other.Close(ctx)
			t.Fatal("distributed service bypassed an exclusive service")
		}
	}
}

func TestStorageBindingRejectsOtherBackends(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, identity := range []string{"s3:one", "s3:one"} {
		if err := s.BindStorage(ctx, identity, true); err != nil {
			t.Fatal(err)
		}
	}
	for _, identity := range []string{"s3:two", "filesystem", ""} {
		if err := s.BindStorage(ctx, identity, identity != "filesystem"); err == nil {
			t.Fatalf("accepted mismatched storage %q", identity)
		}
	}
}

func TestExistingFilesystemDataCannotSwitchToS3(t *testing.T) {
	s := testStore(t)
	_, _ = setupRun(t, s)
	if err := s.BindStorage(context.Background(), "s3:new", true); err == nil {
		t.Fatal("existing durable data was silently stranded by switching to S3")
	}
	if err := s.BindStorage(context.Background(), "filesystem", false); err != nil {
		t.Fatal(err)
	}
}
