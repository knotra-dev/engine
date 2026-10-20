package integration

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/knotra-dev/engine/internal/contract"
	"github.com/knotra-dev/engine/internal/protocol"
	"github.com/knotra-dev/engine/internal/store"
)

func TestRiverServiceStopsWithOpenEventStream(t *testing.T) {
	if os.Getenv("KNOTRA_TEST_DATABASE_URL") == "" {
		t.Skip("set KNOTRA_TEST_DATABASE_URL for HTTP shutdown acceptance")
	}
	for _, mode := range []string{"idle", "slow-reader"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			serviceCtx, stopService := context.WithCancel(ctx)
			defer stopService()
			profile := recoveryProfile()
			profile.Spec.Sandboxes = nil
			options, api, done := startRiverService(t, serviceCtx, profile)
			pkg := contract.Package{Entrypoint: "pipeline.yaml", Source: routingHumanPipeline,
				Files: []contract.File{{Path: "pipeline.yaml", Content: []byte(routingHumanPipeline)}}}
			var definition struct{ Definition protocol.Definition }
			if err := api.Command(ctx, "/definitions", map[string]any{"package": pkg}, uuid.NewString(), &definition); err != nil {
				t.Fatal(err)
			}
			var accepted struct{ Run protocol.Run }
			if err := api.Command(ctx, "/runs", map[string]any{"definitionId": definition.Definition.ID, "profile": profile.Metadata.Name}, uuid.NewString(), &accepted); err != nil {
				t.Fatal(err)
			}
			if mode == "slow-reader" {
				db, err := store.Open(ctx, options.DatabaseURL)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				for range 100 {
					if err := db.Observe(ctx, protocol.Event{RunID: accepted.Run.ID, InstanceID: "work", Type: "model.delta", Data: map[string]any{"content": strings.Repeat("x", 60<<10)}}); err != nil {
						t.Fatal(err)
					}
				}
			}
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, api.BaseURL+"/v1/runs/"+accepted.Run.ID+"/events", nil)
			if err != nil {
				t.Fatal(err)
			}
			connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", options.Listen)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = connection.Close() }()
			if err := connection.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if mode == "slow-reader" {
				tcp, ok := connection.(*net.TCPConn)
				if !ok {
					t.Fatal("expected a TCP connection")
				}
				if err := tcp.SetReadBuffer(1024); err != nil {
					t.Fatal(err)
				}
			}
			if err := request.Write(connection); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(connection)
			if status, err := reader.ReadString('\n'); err != nil || status != "HTTP/1.1 200 OK\r\n" {
				t.Fatalf("event stream response: %q %v", status, err)
			}
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					t.Fatal(err)
				}
				if line == "\r\n" {
					break
				}
			}
			if mode == "slow-reader" {
				// Fill the socket buffer while the client deliberately leaves the event body unread.
				select {
				case <-time.After(200 * time.Millisecond):
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			stopService()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("shutdown with an open event stream failed: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("event stream prevented HTTP shutdown")
			}
		})
	}
}
