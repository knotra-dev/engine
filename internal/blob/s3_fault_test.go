package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestS3LateMutableWriteCannotReplaceNewerCall(t *testing.T) {
	var mu sync.Mutex
	var current []byte
	var version string
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	store := newTestS3(t, func(w http.ResponseWriter, r *http.Request) {
		var data []byte
		if r.Method == http.MethodPut {
			var err error
			data, err = io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			if string(data) == "call-1-completed" {
				close(started)
				<-release // A cancelled HTTP request can still finish on the server.
				defer close(finished)
			}
		}
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPut {
			if (r.Header.Get("If-None-Match") == "*" && current != nil) || (r.Header.Get("If-Match") != "" && r.Header.Get("If-Match") != version) {
				w.WriteHeader(http.StatusPreconditionFailed)
				_, _ = io.WriteString(w, "<Error><Code>PreconditionFailed</Code></Error>")
				return
			}
			current = data
			sum := sha256.Sum256(current)
			version = `"` + hex.EncodeToString(sum[:]) + `"`
		}
		w.Header().Set("ETag", version)
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(current)))
		}
		if r.Method == http.MethodGet {
			_, _ = w.Write(current)
		}
	})
	t.Cleanup(unblock)
	if err := store.Put(t.Context(), "current-call", []byte("call-1-active"), false); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- store.Put(ctx, "current-call", []byte("call-1-completed"), true) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("completion marker did not reach server")
	}
	if err := <-result; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("completion marker did not time out: %v", err)
	}
	if err := store.Put(t.Context(), "current-call", []byte("call-2-active"), true); err != nil {
		t.Fatal(err)
	}
	unblock()
	<-finished
	data, err := store.Get(t.Context(), "current-call", 100)
	if err != nil || string(data) != "call-2-active" {
		t.Fatalf("late completion discarded current unconfirmed call: %q %v", data, err)
	}
}

func TestS3Generic404IsNotProofOfMissingObject(t *testing.T) {
	store := newTestS3(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "<Error><Code>NotFound</Code><Message>upstream route unavailable</Message></Error>")
	})
	if _, err := store.Get(t.Context(), "outcome", 100); err == nil {
		t.Fatalf("ambiguous missing route succeeded: %v", err)
	} else if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("gateway absence was treated as missing execution evidence: %v", err)
	}
}

func TestS3ImmutableRetryVerifiesPreviouslyCommittedBytes(t *testing.T) {
	var puts, gets atomic.Int32
	store := newTestS3(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			if r.Header.Get("If-None-Match") != "*" {
				t.Error("retry removed immutable write precondition")
			}
			if puts.Add(1) == 1 {
				// The object committed, but its success response was lost upstream.
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, "<Error><Code>InternalError</Code></Error>")
			} else {
				w.WriteHeader(http.StatusPreconditionFailed)
				_, _ = io.WriteString(w, "<Error><Code>PreconditionFailed</Code></Error>")
			}
			return
		}
		gets.Add(1)
		_, _ = io.WriteString(w, "original")
	})
	if err := store.Put(t.Context(), "outcome", []byte("original"), false); err != nil || puts.Load() != 2 || gets.Load() != 1 {
		t.Fatalf("committed retry did not verify immutable evidence: puts=%d gets=%d error=%v", puts.Load(), gets.Load(), err)
	}
}

func TestS3MutableReplacementRequiresConditionalVersionSupport(t *testing.T) {
	for _, mode := range []string{"missing-etag", "unsupported-condition"} {
		t.Run(mode, func(t *testing.T) {
			var puts atomic.Int32
			store := newTestS3(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					if mode != "missing-etag" {
						w.Header().Set("ETag", `"original-version"`)
					}
					w.Header().Set("Content-Length", "8")
					return
				}
				puts.Add(1)
				if r.Header.Get("If-Match") != `"original-version"` {
					t.Error("replacement omitted original version precondition")
				}
				w.WriteHeader(http.StatusNotImplemented)
				_, _ = io.WriteString(w, "<Error><Code>NotImplemented</Code><Message>test-secret private-endpoint</Message></Error>")
			})
			if err := store.Put(t.Context(), "current-call", []byte("next"), true); err == nil || errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unsafe replacement succeeded or looked missing: %v", err)
			}
			if mode == "missing-etag" && puts.Load() != 0 {
				t.Fatal("replacement wrote without a version")
			}
			if mode == "unsupported-condition" && puts.Load() != 1 {
				t.Fatal("replacement retried without a condition")
			}
		})
	}
}

func TestS3DuplicateVerificationOutageDoesNotClaimSuccess(t *testing.T) {
	var puts atomic.Int32
	store := newTestS3(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			puts.Add(1)
			w.WriteHeader(http.StatusPreconditionFailed)
			_, _ = io.WriteString(w, "<Error><Code>PreconditionFailed</Code></Error>")
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "<Error><Code>AccessDenied</Code><Message>test-secret private-endpoint</Message></Error>")
	})
	if err := store.Put(t.Context(), "outcome", []byte("original"), false); err == nil || errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrConflict) || puts.Load() != 1 {
		t.Fatalf("unverified duplicate was accepted or overwritten: puts=%d error=%v", puts.Load(), err)
	}
}
