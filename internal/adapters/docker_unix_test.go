//go:build unix

package adapters

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestDockerContextDiscoveryIsSharedAcrossExecutions(t *testing.T) {
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$KNOTRA_TEST_DOCKER_CALLS\"\nprintf '%s\\n' 'unix:///knotra-test/docker.sock'\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("KNOTRA_TEST_DOCKER_CALLS", calls)
	runner := &Runner{}
	t.Cleanup(func() { _ = runner.Close() })
	runners := []*Runner{runner, runner.WithHooks(nil), runner.WithHooks(nil)}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := runner.docker(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled discovery: %v", err)
	}
	if _, err := runner.docker(t.Context()); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for _, current := range runners {
		workers.Go(func() {
			if _, err := current.docker(t.Context()); err != nil {
				t.Errorf("shared Docker discovery: %v", err)
			}
		})
	}
	workers.Wait()
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "\n"); got != 1 {
		t.Fatalf("Docker context inspected %d times for the same engine", got)
	}
}
