package adapters

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/knotra-dev/engine/internal/contract"
)

type discoveryTransport func(*http.Request) (*http.Response, error)

func (f discoveryTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestAdmittedModelDiscoveryFailures(t *testing.T) {
	for _, provider := range []string{"ollama", "openai", "anthropic"} {
		for _, tc := range []struct {
			name      string
			status    int
			code      string
			retryable bool
			cancelled error
		}{
			{name: "bad request", status: 400, code: "MODEL_REQUEST_REJECTED"},
			{name: "unauthorized", status: 401, code: "MODEL_AUTH_FAILED"},
			{name: "forbidden", status: 403, code: "MODEL_AUTH_FAILED"},
			{name: "missing model", status: 404, code: "MODEL_REQUEST_REJECTED"},
			{name: "request timeout", status: 408, code: "MODEL_REQUEST_REJECTED", retryable: true},
			{name: "rate limited", status: 429, code: "MODEL_RATE_LIMITED", retryable: true},
			{name: "internal error", status: 500, code: "MODEL_REQUEST_REJECTED", retryable: true},
			{name: "unavailable", status: 503, code: "MODEL_REQUEST_REJECTED", retryable: true},
			{name: "transport failure", code: "EXTERNAL_CALL_FAILED", retryable: true},
			{name: "body read failure", code: "EXTERNAL_CALL_FAILED", retryable: true},
			{name: "invalid JSON", code: "OUTPUT_INVALID"},
			{name: "missing identity"},
			{name: "changed identity", code: "RESOURCE_CHANGED"},
			{name: "empty credential"},
			{name: "cancelled", cancelled: context.Canceled},
			{name: "deadline exceeded", cancelled: context.DeadlineExceeded},
		} {
			t.Run(provider+"/"+tc.name, func(t *testing.T) {
				admitted := false
				discoveries, generations := 0, 0
				transport := discoveryTransport(func(req *http.Request) (*http.Response, error) {
					if req.Context().Err() != nil {
						return nil, req.Context().Err()
					}
					status, body := http.StatusOK, ""
					switch req.URL.Path {
					case "/api/tags":
						body = `{"models":[{"name":"qwen3.5:9b","digest":"admitted"}]}`
					case "/api/show":
						body = `{"capabilities":["completion"]}`
					case "/models/fixture-model":
						body = `{"id":"fixture-model"}`
					default:
						generations++
						t.Errorf("unexpected generation request: %s", req.URL.Path)
					}
					if admitted {
						discoveries++
						if tc.status != 0 {
							status, body = tc.status, "fixture-key private prompt"
						}
						switch tc.name {
						case "transport failure":
							return nil, errors.New("fixture-key transport failure")
						case "body read failure":
							return &http.Response{StatusCode: status, Body: io.NopCloser(iotest.ErrReader(errors.New("fixture-key read failure")))}, nil
						case "invalid JSON":
							body = "{"
						case "missing identity":
							body = "{}"
						case "changed identity":
							body = strings.ReplaceAll(body, "admitted", "changed")
							body = strings.ReplaceAll(body, "fixture-model", "changed-model")
						}
					}
					return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
				})
				req := testRequest("http://model.test")
				if provider != "ollama" {
					req = cloudRequest(provider, "http://model.test")
				}
				pipeline(req).Spec.Sandboxes = nil
				hooks := newHooks()
				runner := &Runner{Hooks: hooks, HTTPClient: &http.Client{Transport: transport}}
				if err := runner.Prepare(t.Context(), req.Plan); err != nil {
					t.Fatal(err)
				}
				admitted = true
				if tc.name == "empty credential" {
					connection := req.Plan.Profile.Spec.Models["local"]
					empty := ""
					connection.Auth = map[string]contract.Credential{"key": {Value: &empty}}
					req.Plan.Profile.Spec.Models["local"] = connection
				}
				ctx := t.Context()
				if errors.Is(tc.cancelled, context.Canceled) {
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
				} else if errors.Is(tc.cancelled, context.DeadlineExceeded) {
					var cancel context.CancelFunc
					ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
					defer cancel()
				}
				_, err := runner.Execute(ctx, req)
				if err == nil {
					t.Fatal("catalog failure was ignored")
				}
				var failed *Failure
				if tc.code != "" {
					if !errors.As(err, &failed) || failed.Code != tc.code || failed.Retryable != tc.retryable || failed.Unknown || failed.OperationID == "" {
						t.Fatalf("incorrect discovery classification: %+v", err)
					}
				} else if errors.As(err, &failed) && (failed.Retryable || failed.Unknown) {
					t.Fatalf("permanent discovery failure became retryable or ambiguous: %+v", failed)
				}
				if tc.cancelled != nil && !errors.Is(err, tc.cancelled) {
					t.Fatalf("lost context cause %v: %v", tc.cancelled, err)
				}
				if strings.Contains(err.Error(), "fixture-key") || strings.Contains(err.Error(), "private prompt") {
					t.Fatal("discovery failure leaked private data")
				}
				if generations != 0 || hooks.calls["model"] != 0 || discoveries > 1 {
					t.Fatalf(
						"failure generated, debited budget, or retried internally: generations=%d budgets=%v discoveries=%d",
						generations,
						hooks.calls,
						discoveries,
					)
				}
			})
		}
	}
}
