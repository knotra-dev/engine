package queue

import (
	"testing"
	"testing/synctest"

	"github.com/google/uuid"
	"github.com/knotra-dev/engine/internal/execution"
)

func TestClientsCreatedAtSameTimeHaveIndependentIdentities(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		config := Config{Role: execution.RoleAPI, EngineID: uuid.NewString(), HostID: "same-host"}
		first, err := New(nil, config, Handlers{})
		if err != nil {
			t.Fatal(err)
		}
		second, err := New(nil, config, Handlers{})
		if err != nil {
			t.Fatal(err)
		}
		if first.River.ID() == second.River.ID() {
			t.Fatalf("independent clients share delivery identity %q", first.River.ID())
		}
	})
}
