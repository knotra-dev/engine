package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/knotra-dev/engine/internal/contract"
	"github.com/knotra-dev/engine/internal/protocol"
	"github.com/knotra-dev/engine/internal/store"
)

func TestEventEndpointsDoNotHydrateRuns(t *testing.T) {
	db := apiStore(t)
	plan := contract.Plan{Version: "knotra/v1", CompilerVersion: contract.CompilerVersion, CELVersion: contract.CELVersion, Root: "pipeline.yaml", Digest: "events",
		Pipelines: map[string]*contract.Pipeline{"pipeline.yaml": {}}, Profile: contract.Profile{Spec: contract.ProfileSpec{Limits: contract.Limits{Timeout: "1h"}}}}
	if err := pgx.BeginFunc(t.Context(), db.Pool, func(tx pgx.Tx) error {
		definition := protocol.Definition{ID: "events", PackageDigest: "events"}
		if _, err := store.PutDefinition(t.Context(), tx, definition); err != nil {
			return err
		}
		_, err := store.PutRiverRun(t.Context(), tx, protocol.Run{ID: "run", DefinitionID: definition.ID, Status: "pending"}, plan, contract.Values{},
			func(context.Context, pgx.Tx, string, int64) error { return nil })
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Observe(t.Context(), protocol.Event{RunID: "run", InstanceID: "node", Type: "model.delta", Data: map[string]any{"content": "hello"}}); err != nil {
		t.Fatal(err)
	}
	lock, err := db.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Rollback(context.Background()) }()
	// Run hydration must block on these tables; event delivery must remain independent.
	if _, err := lock.Exec(t.Context(), "LOCK TABLE knotra_definitions, knotra_instances IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	handler := (&Server{Store: db, Token: "secret"}).Handler()
	srv := httptest.NewServer(handler)
	defer srv.Close()
	for _, endpoint := range []string{"history", "events"} {
		t.Run(endpoint, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/runs/run/"+endpoint+"?instanceId=node", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer secret")
			response, err := srv.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode != http.StatusOK {
				t.Fatal(response.Status)
			}
			var event protocol.Event
			if endpoint == "history" {
				var page protocol.Page[protocol.Event]
				if err := json.NewDecoder(response.Body).Decode(&page); err != nil || len(page.Items) != 1 || page.NextCursor != nil {
					t.Fatalf("history: %+v, %v", page, err)
				}
				event = page.Items[0]
			} else {
				if response.Header.Get("Content-Type") != "text/event-stream" {
					t.Fatal("missing SSE content type")
				}
				scanner := bufio.NewScanner(response.Body)
				for scanner.Scan() {
					if data, ok := strings.CutPrefix(scanner.Text(), "data: "); ok {
						if err := json.Unmarshal([]byte(data), &event); err != nil {
							t.Fatal(err)
						}
						break
					}
				}
				if err := scanner.Err(); err != nil {
					t.Fatal(err)
				}
			}
			data, ok := event.Data.(map[string]any)
			if event.ID == "" || event.RunID != "run" || event.InstanceID != "node" || event.Type != "model.delta" || !ok || data["content"] != "hello" {
				t.Fatalf("event changed: %+v", event)
			}
		})
		for _, tc := range []struct {
			run, token string
			status     int
		}{
			{"missing", "Bearer secret", http.StatusNotFound},
			{"run", "", http.StatusUnauthorized},
		} {
			request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/runs/"+tc.run+"/"+endpoint, nil)
			request.Header.Set("Authorization", tc.token)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.status {
				t.Fatalf("%s: %d %s", request.URL, response.Code, response.Body.String())
			}
		}
	}
}
