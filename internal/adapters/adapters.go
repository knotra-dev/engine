// Package adapters implements external work. It neither schedules graphs nor owns
// durable state; the engine supplies transactional budgets and operation journals.
package adapters

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"sort"
	"sync"

	"github.com/knotra-dev/engine/internal/contract"
	"github.com/knotra-dev/engine/internal/execution"
)

// Version identifies the execution ABI frozen by admission. Change it when a
// saved plan can no longer be executed with the same adapter semantics.
const Version = "knotra-adapters/v1"

type Request struct {
	RunID, InstanceID, Pipeline, ScopeID string
	Attempt                              int
	Plan                                 *contract.Plan
	Node                                 contract.Node
	Inputs                               contract.Values
	ToolArguments                        json.RawMessage
	service                              bool
	observation                          *executionObservation
}

type Operation struct{ ID, Kind, Effect, IdempotencyKey string }

type OperationState struct {
	Started, Completed bool
	Response           json.RawMessage
}

// Hooks must atomically record intent before work and persist responses before
// acknowledging success. A journal entry must never be shared across runs.
type Hooks interface {
	Reserve(context.Context, string) error
	PutArtifact(context.Context, string, string, []byte) (contract.Artifact, error)
	GetArtifact(context.Context, string) ([]byte, error)
	BeginOperation(context.Context, Operation) (OperationState, error)
	CompleteOperation(context.Context, string, json.RawMessage) error
	RegisterResource(context.Context, string, string, string) (execution.ResourceRecord, error)
	RecordMCPSession(context.Context, string, execution.MCPSessionRecord) error
	CloseResource(context.Context, string) error
}

// Failure carries execution decisions without coupling adapters to a workflow SDK.
type Failure struct {
	Code, Message, OperationID string
	Retryable, Unknown         bool
}

func (e *Failure) Error() string { return e.Code + ": " + e.Message }

func failure(code string, err error) error { return &Failure{Code: code, Message: err.Error()} }

type Runner struct {
	Hooks                                          Hooks
	HTTPClient                                     *http.Client
	DockerHost, HelperPath, WorkDir, FirewallImage string
	EngineID                                       string
	HostID                                         string
	LookupEnv                                      func(string) (string, bool)
	ResourceEvidence                               *execution.OutcomeFiles
	mu                                             sync.Mutex
	shared                                         *sessionCache
}

type sessionCache struct {
	mu            sync.Mutex
	docker        *dockerClient
	sessions      map[string]*mcpSession
	initializing  map[string]chan struct{}
	discoveryGate chan struct{}
	discovery     *mcpSession
	// ponytail: sorted active keys bound cleanup scans; use an ordered index if insertion cost matters.
	keys []string
}

// Caller holds the cache lock while indexing pending or connected sessions.
func (c *sessionCache) save(key string, session *mcpSession) {
	c.sessions[key] = session
	position, found := slices.BinarySearch(c.keys, key)
	if !found {
		c.keys = slices.Insert(c.keys, position, key)
	}
}

func (c *sessionCache) remove(key string) {
	delete(c.sessions, key)
	if position, found := slices.BinarySearch(c.keys, key); found {
		c.keys = slices.Delete(c.keys, position, position+1)
	}
}

func (r *Runner) cache() *sessionCache {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.shared == nil {
		r.shared = &sessionCache{sessions: map[string]*mcpSession{}, initializing: map[string]chan struct{}{}, discoveryGate: make(chan struct{}, 1)}
	}
	return r.shared
}

// WithHooks creates an execution-scoped runner while retaining the Docker client
// and run MCP sessions. Configure a runner before its first use; never mutate it
// concurrently.
func (r *Runner) WithHooks(h Hooks) *Runner {
	return &Runner{
		Hooks:            h,
		HTTPClient:       r.HTTPClient,
		DockerHost:       r.DockerHost,
		HelperPath:       r.HelperPath,
		WorkDir:          r.WorkDir,
		FirewallImage:    r.FirewallImage,
		EngineID:         r.EngineID,
		HostID:           r.HostID,
		LookupEnv:        r.LookupEnv,
		ResourceEvidence: r.ResourceEvidence,
		shared:           r.cache(),
	}
}

func (r *Runner) httpClient() *http.Client {
	client := &http.Client{}
	if r.HTTPClient != nil {
		*client = *r.HTTPClient
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client
}

func (r *Runner) Execute(ctx context.Context, req Request) (contract.Values, error) {
	if r.Hooks == nil || req.Plan == nil {
		return nil, fmt.Errorf("adapters: hooks and plan are required")
	}
	// Direct in-process callers can construct plans without admission metadata.
	// Persisted engine plans always have a version and must never drift silently.
	if version := req.Plan.Runtime.AdapterVersion; version != "" && version != Version {
		return nil, failure(
			"ADAPTER_VERSION_UNSUPPORTED",
			fmt.Errorf("admitted plan requires adapter version %q; worker has %q", version, Version),
		)
	}

	req.observation = r.newObservation(ctx, req)
	defer req.observation.finish(ctx)

	switch req.Node.Type {
	case "llm":
		return r.llm(ctx, req)
	case "agent":
		return r.agent(ctx, req)
	case "code":
		return r.code(ctx, req)
	case "tool":
		return r.tool(ctx, req)
	default:
		return nil, fmt.Errorf("adapters: unsupported leaf type %q", req.Node.Type)
	}
}

func (r *Runner) secret(p contract.Profile, name string) (string, error) {
	s, ok := p.Spec.Secrets[name]
	if !ok {
		return "", fmt.Errorf("unknown secret reference %q", name)
	}
	lookup := r.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	value, ok := lookup(s.Env)
	if !ok || value == "" {
		return "", fmt.Errorf("secret %q is unavailable", name)
	}
	return value, nil
}

func (r *Runner) credential(p contract.Profile, c contract.Credential) (string, error) {
	if c.Value != nil {
		return *c.Value, nil
	}
	return r.secret(p, c.SecretRef)
}

func operationID(req Request, name string, perAttempt bool) string {
	id := req.RunID + "/" + req.InstanceID + "/" + name
	if perAttempt {
		id += fmt.Sprintf("/attempt/%d", req.Attempt)
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

func (r *Runner) operation(ctx context.Context, op Operation, fn func(admit func() error) (json.RawMessage, error)) (json.RawMessage, error) {
	state, err := r.Hooks.BeginOperation(ctx, op)
	if err != nil {
		return nil, err
	}
	if state.Completed {
		return state.Response, nil
	}
	if state.Started {
		safeRetry := op.Effect == "read" || op.IdempotencyKey != ""
		return nil, &Failure{
			Code:        "OUTCOME_UNKNOWN",
			Message:     "operation was started without a durable response",
			OperationID: op.ID,
			Unknown:     !safeRetry,
			Retryable:   safeRetry,
		}
	}

	admitted := false
	result, err := fn(func() error {
		if admitted {
			return fmt.Errorf("operation already admitted")
		}
		if op.Kind != "" {
			if err := r.Hooks.Reserve(ctx, op.Kind); err != nil {
				return err
			}
		}
		admitted = true
		return nil
	})
	if err != nil {
		var typed *Failure
		if errors.As(err, &typed) {
			cloned := *typed
			if cloned.OperationID == "" {
				cloned.OperationID = op.ID
			}
			return nil, &cloned
		}
		if !admitted {
			return nil, err
		}
		// Transport failure after sending cannot prove absence of external effects.
		safeRetry := op.Effect == "read" || op.IdempotencyKey != ""
		return nil, &Failure{
			Code:        "EXTERNAL_CALL_FAILED",
			Message:     err.Error(),
			OperationID: op.ID,
			Unknown:     !safeRetry,
			Retryable:   safeRetry,
		}
	}
	if !admitted {
		return nil, fmt.Errorf("operation returned a result without physical-call admission")
	}
	if err = r.Hooks.CompleteOperation(ctx, op.ID, result); err != nil {
		return nil, &Failure{
			Code:        "OUTCOME_UNKNOWN",
			Message:     "cannot persist external response",
			OperationID: op.ID,
			Unknown:     true,
		}
	}

	return result, nil
}

func pipeline(req Request) *contract.Pipeline { return req.Plan.Pipelines[req.Pipeline] }

func textSource(req Request, t contract.TextSource) (string, error) {
	if t.File == "" {
		return t.Text, nil
	}

	for _, f := range req.Plan.Package.Files {
		if f.Path == t.File {
			return string(f.Content), nil
		}
	}

	return "", fmt.Errorf("package text %q is missing", t.File)
}

func (r *Runner) Close() error {
	cache := r.cache()
	cache.discoveryGate <- struct{}{}
	failures := cache.closeDiscovery()
	<-cache.discoveryGate
	cache.mu.Lock()
	keys := slices.Clone(cache.keys)
	docker := cache.docker
	cache.mu.Unlock()
	for _, key := range keys {
		failures = errors.Join(failures, r.releaseSession(key, true))
	}
	if docker != nil {
		docker.client.CloseIdleConnections()
	}
	return failures
}

// SessionPage returns at most 64 sorted cache identities without waiting for
// another run's initialization. A busy cache is retried by maintenance.
func (r *Runner) SessionPage(cursor string) ([]string, error) {
	cache := r.cache()
	if !cache.mu.TryLock() {
		return nil, errors.New("shared MCP session initialization is busy")
	}
	defer cache.mu.Unlock()
	start := sort.Search(len(cache.keys), func(i int) bool { return cache.keys[i] > cursor })
	return slices.Clone(cache.keys[start:min(start+64, len(cache.keys))]), nil
}

// ReleaseSession removes an entry after local closure and HTTP cleanup succeeds
// or the server reports 404/405. It never
// waits on a different session's initialization or an active call.
func (r *Runner) ReleaseSession(key string) error {
	return r.releaseSession(key, false)
}

func (r *Runner) releaseSession(key string, wait bool) error {
	cache := r.cache()
	if wait {
		cache.mu.Lock()
	} else if !cache.mu.TryLock() {
		return errors.New("shared MCP session initialization is busy")
	}
	s := cache.sessions[key]
	initializing := cache.initializing[key]
	cache.mu.Unlock()
	if initializing != nil {
		if !wait {
			return errors.New("shared MCP session initialization is busy")
		}
		<-initializing
		return r.releaseSession(key, true)
	}
	if s == nil {
		return nil
	}
	if !s.mu.TryLock() {
		return errors.New("shared MCP session has an active call")
	}

	defer s.mu.Unlock()
	if err := s.close(); err != nil {
		return err
	}
	if !cache.mu.TryLock() {
		return errors.New("shared MCP session initialization is busy")
	}

	defer cache.mu.Unlock()
	if cache.sessions[key] == s {
		cache.remove(key)
	}
	return nil
}
