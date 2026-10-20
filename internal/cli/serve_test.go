package cli

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestServeRejectsInvalidRoleAndS3Boolean(t *testing.T) {
	for _, test := range []struct {
		args []string
		env  map[string]string
		want string
	}{
		{[]string{"serve", "--role", "wrong"}, nil, "unknown execution process role"},
		{[]string{"serve"}, map[string]string{"KNOTRA_ROLE": "wrong"}, "unknown execution process role"},
		{[]string{"serve"}, map[string]string{"KNOTRA_S3_PATH_STYLE": "wrong"}, "KNOTRA_S3_PATH_STYLE must be a boolean"},
		{[]string{"serve", "--s3-path-style=false"}, map[string]string{"KNOTRA_S3_PATH_STYLE": "wrong"}, "database-url"},
	} {
		err := Execute(context.Background(), test.args, Options{Out: io.Discard, Err: io.Discard, Getenv: func(key string) string { return test.env[key] }})
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("args=%v error=%v, want %q", test.args, err, test.want)
		}
	}
}
