package app

import (
	"testing"

	"github.com/knotra-dev/engine/internal/blob"
	"github.com/knotra-dev/engine/internal/execution"
)

func TestServiceRoleValidation(t *testing.T) {
	for _, test := range []struct {
		name string
		o    Options
		ok   bool
	}{
		{"local", Options{Listen: "127.0.0.1:8787"}, true},
		{"unknown", Options{Role: "worker"}, false},
		{"private-local-dirs", Options{Role: execution.RoleExecutor}, false},
		{"executor-no-listener", Options{Role: execution.RoleExecutor, S3: blob.S3Config{Bucket: "work"}}, true},
		{"scheduler-no-listener", Options{Role: execution.RoleScheduler, SharedDataDir: "/shared"}, true},
		{"api-requires-tls", Options{Role: execution.RoleAPI, Listen: "0.0.0.0:8787", S3: blob.S3Config{Bucket: "work"}}, false},
		{"api-tls", Options{Role: execution.RoleAPI, Listen: "0.0.0.0:8787", Token: "token", TLSCert: "cert", TLSKey: "key", S3: blob.S3Config{Bucket: "work"}}, true},
		{"conflicting-stores", Options{Role: execution.RoleExecutor, SharedDataDir: "/shared", S3: blob.S3Config{Bucket: "work"}}, false},
		{"incomplete-s3", Options{Listen: "127.0.0.1:8787", S3: blob.S3Config{Endpoint: "https://storage.example"}}, false},
		{"region-without-bucket", Options{Listen: "127.0.0.1:8787", S3: blob.S3Config{Region: "auto"}}, false},
		{"managed-local", Options{Listen: "127.0.0.1:8787", ManagedToken: "control", ManagedHoldAdmission: true}, true},
		{"managed-remote", Options{Listen: "0.0.0.0:8787", ManagedToken: "control", Token: "token", TLSCert: "cert", TLSKey: "key"}, false},
		{"managed-split-api", Options{Role: execution.RoleAPI, Listen: "127.0.0.1:8787", ManagedToken: "control", SharedDataDir: "/shared"}, false},
		{"managed-shared-storage", Options{Listen: "127.0.0.1:8787", ManagedToken: "control", SharedDataDir: "/shared"}, false},
		{"hold-without-owner", Options{Listen: "127.0.0.1:8787", ManagedHoldAdmission: true}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.o.DatabaseURL = "postgres://test"
			test.o.Profiles = []string{"profile.yaml"}
			err := test.o.validate()
			if (err == nil) != test.ok {
				t.Fatalf("validate=%v, want valid=%v", err, test.ok)
			}
		})
	}
}
