package execution

import (
	"testing"

	"github.com/michael-bill/knotra/internal/contract"
)

func TestPermissionIntersectionCannotEscalate(t *testing.T) {
	parent := &contract.Permissions{
		Models:    []string{"allowed"},
		MCP:       map[string][]string{"server": {"read"}},
		Sandboxes: []string{"local"},
		Secrets:   []string{"safe"},
	}
	child := &contract.Permissions{
		Models:    []string{"allowed", "forbidden"},
		MCP:       map[string][]string{"server": {"read", "write"}, "extra": {"all"}},
		Sandboxes: []string{"local", "host"},
		Secrets:   []string{"safe", "root"},
	}
	actual := IntersectPermissions(parent, child)
	if len(actual.Models) != 1 || len(actual.MCP["server"]) != 1 || len(actual.MCP["extra"]) != 0 || len(actual.Sandboxes) != 1 || len(actual.Secrets) != 1 {
		t.Fatalf("escalated permissions: %+v", actual)
	}
}
