package adapters

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/michael-bill/knotra/internal/contract"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func runSessionRequest(endpoint string) Request {
	req := testRequest("")
	req.Plan.Profile.Spec.MCP = map[string]contract.MCPConnection{"local": {
		Transport: "streamable_http", URL: endpoint, AllowRunSession: true,
	}}
	req.Plan.Pipelines[req.Pipeline].Spec.MCP = map[string]contract.MCPResource{"tools": {Connection: "local", Session: "run"}}
	return req
}

func TestRunSessionInitializationIsIndependentAndCancelable(t *testing.T) {
	var initializations atomic.Int32
	started, unblock := make(chan struct{}), make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(unblock) })
	server := mcp.NewServer(&mcp.Implementation{Name: "session-test", Version: "1"}, nil)
	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost && req.URL.Path == "/slow" {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Error(err)
				return
			}
			req.Body = io.NopCloser(bytes.NewReader(body))
			var message struct{ Method string }
			if err := json.Unmarshal(body, &message); err != nil {
				t.Error(err)
				return
			}
			if message.Method == "initialize" {
				if initializations.Add(1) == 1 {
					close(started)
				}
				select {
				case <-unblock:
				case <-req.Context().Done():
					return
				}
			}
		}
		transport.ServeHTTP(w, req)
	}))
	defer remote.Close()
	base := &Runner{}
	hooks := newHooks()
	defer func() {
		release.Do(func() { close(unblock) })
		_ = base.Close()
	}()
	type result struct {
		session *mcpSession
		err     error
	}
	lookup := func(ctx context.Context, req Request) <-chan result {
		out := make(chan result, 1)
		go func() {
			session, _, err := base.WithHooks(hooks).session(ctx, req, "tools")
			out <- result{session, err}
		}()
		return out
	}
	await := func(out <-chan result) result {
		t.Helper()
		select {
		case result := <-out:
			return result
		case <-time.After(time.Second):
			release.Do(func() { close(unblock) })
			t.Fatal("session lookup waited for unrelated initialization")
			return result{}
		}
	}
	fast := runSessionRequest(remote.URL + "/fast")
	fast.RunID = "fast-run"
	cached := await(lookup(t.Context(), fast))
	if cached.err != nil {
		t.Fatal(cached.err)
	}
	slow := runSessionRequest(remote.URL + "/slow")
	first := lookup(t.Context(), slow)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("slow initialization did not start")
	}
	if page, err := base.SessionPage(""); err != nil || len(page) != 2 {
		t.Fatalf("initializing session missing from cleanup inventory: %v, %v", page, err)
	}
	if err := base.ReleaseSession(slow.RunID + "/" + slow.ScopeID + "/tools"); err == nil {
		t.Fatal("nonblocking cleanup accepted an initializing session")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if result := await(lookup(ctx, slow)); !errors.Is(result.err, context.DeadlineExceeded) {
		t.Fatalf("waiting caller ignored cancellation: %v", result.err)
	}
	if result := await(lookup(t.Context(), fast)); result.err != nil || result.session != cached.session {
		t.Fatalf("unrelated cached session was blocked or replaced: %v", result.err)
	}
	firstFastKey := fast.RunID + "/" + fast.ScopeID + "/tools"
	fast.ScopeID = "other"
	if result := await(lookup(t.Context(), fast)); result.err != nil || result.session == cached.session {
		t.Fatalf("unrelated new session did not initialize independently: %v", result.err)
	}
	closed := make(chan error, 1)
	go func() {
		closed <- errors.Join(
			base.ReleaseSession(firstFastKey),
			base.ReleaseSession(fast.RunID+"/"+fast.ScopeID+"/tools"),
		)
	}()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		release.Do(func() { close(unblock) })
		t.Fatal("cleanup waited for an unrelated run's initialization")
	}
	if page, err := base.SessionPage(""); err != nil || len(page) != 1 || page[0] != slow.RunID+"/"+slow.ScopeID+"/tools" {
		t.Fatalf("session cleanup affected another run or leaked completed entries: %v, %v", page, err)
	}
	var waiters []<-chan result
	for range 8 {
		waiters = append(waiters, lookup(t.Context(), slow))
	}
	release.Do(func() { close(unblock) })
	connected := await(first)
	if connected.err != nil {
		t.Fatal(connected.err)
	}
	for _, waiter := range waiters {
		if result := await(waiter); result.err != nil || result.session != connected.session {
			t.Fatalf("same-key initialization was not shared: %v", result.err)
		}
	}
	if initializations.Load() != 1 {
		t.Fatalf("same-key callers created %d physical sessions", initializations.Load())
	}
}

func TestRunnerCloseWaitsForSessionInitialization(t *testing.T) {
	started, unblock := make(chan struct{}), make(chan struct{})
	var release, announce sync.Once
	defer release.Do(func() { close(unblock) })
	server := mcp.NewServer(&mcp.Implementation{Name: "session-test", Version: "1"}, nil)
	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost && req.Header.Get("Mcp-Session-Id") == "" {
			announce.Do(func() { close(started) })
			select {
			case <-unblock:
			case <-req.Context().Done():
				return
			}
		}
		transport.ServeHTTP(w, req)
	}))
	defer remote.Close()
	runner := &Runner{Hooks: newHooks()}
	defer func() {
		release.Do(func() { close(unblock) })
		_ = runner.Close()
	}()
	req := runSessionRequest(remote.URL)
	connected := make(chan error, 1)
	go func() {
		_, _, err := runner.session(t.Context(), req, "tools")
		connected <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("initialization did not start")
	}
	closed := make(chan error, 1)
	go func() { closed <- runner.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("cleanup returned before initialization completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	release.Do(func() { close(unblock) })
	if err := <-connected; err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if page, err := runner.SessionPage(""); err != nil || len(page) != 0 {
		t.Fatalf("cleanup lost the initializing session: %v, %v", page, err)
	}
}
