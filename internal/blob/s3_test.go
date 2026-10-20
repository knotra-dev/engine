package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestS3(t *testing.T, handler http.HandlerFunc) Store {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "test-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
	t.Setenv("AWS_SESSION_TOKEN", "test-token")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	store, err := NewS3(t.Context(), S3Config{Bucket: "bucket", Region: "region", Endpoint: server.URL, Prefix: "/deployment/engine/", PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestS3ConditionalPublicationAcrossClients(t *testing.T) {
	var mu sync.Mutex
	objects := map[string][]byte{}
	modified := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	store := newTestS3(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/bucket/deployment/engine/") {
			t.Errorf("wrong namespace: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") == "" || r.Header.Get("X-Amz-Security-Token") != "test-token" {
			t.Error("default environment credentials were not used")
		}
		if r.Header.Get("X-Amz-Sdk-Checksum-Algorithm") != "" || r.Header.Get("X-Amz-Checksum-Mode") != "" || r.Header.Get("X-Amz-Trailer") != "" {
			t.Error("unexpected optional AWS checksum negotiation")
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			previous, found := objects[r.URL.Path]
			sum := sha256.Sum256(previous)
			version := `"` + hex.EncodeToString(sum[:]) + `"`
			if (found && r.Header.Get("If-None-Match") == "*") || (r.Header.Get("If-Match") != "" && r.Header.Get("If-Match") != version) {
				w.WriteHeader(http.StatusPreconditionFailed)
				_, _ = io.WriteString(w, "<Error><Code>PreconditionFailed</Code></Error>")
				return
			}
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			objects[r.URL.Path] = data
		case http.MethodGet, http.MethodHead:
			data, found := objects[r.URL.Path]
			if !found {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, "<Error><Code>NoSuchKey</Code></Error>")
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.Header().Set("Last-Modified", modified.Format(http.TimeFormat))
			sum := sha256.Sum256(data)
			w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
			if r.Method == http.MethodGet {
				_, _ = w.Write(data)
			}
		default:
			t.Errorf("unexpected method %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	ctx := t.Context()
	var writers sync.WaitGroup
	for range 12 {
		writers.Go(func() {
			if err := store.Put(ctx, "artifacts/hash", []byte("original"), false); err != nil {
				t.Errorf("concurrent identical publication: %v", err)
			}
		})
	}
	writers.Wait()
	if err := store.Put(ctx, "artifacts/hash", []byte("different"), false); !errors.Is(err, ErrConflict) {
		t.Fatalf("overwrote immutable object: %v", err)
	}
	// A second client shares only the remote namespace, with no local directory.
	first, ok := store.(*s3Store)
	if !ok {
		t.Fatal("S3 constructor returned an unexpected store")
	}
	reader := &s3Store{client: first.client, bucket: first.bucket, prefix: first.prefix}
	data, err := reader.Get(ctx, "artifacts/hash", 8)
	if err != nil || !bytes.Equal(data, []byte("original")) {
		t.Fatalf("immutable bytes changed: %q %v", data, err)
	}
	if _, err := reader.Get(ctx, "artifacts/hash", 7); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("unbounded read: %v", err)
	}
	if saved, err := reader.SavedAt(ctx, "artifacts/hash"); err != nil || saved != modified {
		t.Fatalf("wrong saved time: %s %v", saved, err)
	}
	for _, data := range []string{"call-1", "call-2", "completed"} {
		if err := store.Put(ctx, "outcomes/current-call", []byte(data), true); err != nil {
			t.Fatal(err)
		}
	}
	data, err = reader.Get(ctx, "outcomes/current-call", 0)
	if err != nil || string(data) != "completed" {
		t.Fatalf("current call did not replace: %q %v", data, err)
	}
	if err := store.Put(ctx, "artifacts/empty", nil, false); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, "artifacts/empty", nil, false); err != nil {
		t.Fatalf("empty object was not idempotent: %v", err)
	}
}

func TestS3ErrorsDistinguishMissingFromUnavailableAndRedactDetails(t *testing.T) {
	store := newTestS3(t, func(w http.ResponseWriter, r *http.Request) {
		code, status := "AccessDenied", http.StatusForbidden
		if strings.HasSuffix(r.URL.Path, "/missing") {
			code, status = "NoSuchKey", http.StatusNotFound
		}
		if strings.HasSuffix(r.URL.Path, "/bucket-missing") {
			code, status = "NoSuchBucket", http.StatusNotFound
		}
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, "<Error><Code>%s</Code><Message>test-secret test-token https://private-endpoint/</Message></Error>", code)
	})
	for _, key := range []string{"missing", "bucket-missing", "unavailable"} {
		_, err := store.Get(t.Context(), key, 100)
		if err == nil {
			t.Fatalf("%s unexpectedly succeeded", key)
		}
		if errors.Is(err, os.ErrNotExist) != (key == "missing") {
			t.Errorf("%s classified incorrectly: %v", key, err)
		}
		if strings.Contains(err.Error(), "test-") || strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), "private-endpoint") {
			t.Errorf("provider details leaked: %v", err)
		}
	}
	if err := store.Put(t.Context(), "unavailable", []byte("data"), false); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("write outage classified as absence: %v", err)
	}
	if _, err := store.SavedAt(t.Context(), "missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("metadata absence classified incorrectly: %v", err)
	}
	if _, err := store.SavedAt(t.Context(), "bucket-missing"); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing bucket metadata classified as missing object: %v", err)
	}
}

func TestS3RequestsHonorCancellationAndStreamingByteLimit(t *testing.T) {
	store := newTestS3(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/slow") {
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("test server cannot stream responses")
			return
		}
		flusher.Flush()
		_, _ = io.WriteString(w, "larger than limit")
	})
	if _, err := store.Get(t.Context(), "stream", 5); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("streaming response exceeded limit: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := store.Get(ctx, "slow", 10); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("read ignored cancellation: %v", err)
	}
}

func TestS3RejectsEndpointCredentialsWithoutEchoingThem(t *testing.T) {
	for _, endpoint := range []string{"ftp://private", "https://user:secret@private/", "https://private/?token=secret", "https://private/#secret", "://secret"} {
		_, err := NewS3(t.Context(), S3Config{Bucket: "bucket", Region: "region", Endpoint: endpoint})
		if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private") {
			t.Fatalf("unsafe endpoint error: %v", err)
		}
	}
}

func TestS3IdentityUsesResolvedEndpointOverrides(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
	t.Setenv("AWS_REGION", "region")
	t.Setenv("AWS_ENDPOINT_URL_S3", "https://first-storage.example/")
	settings := S3Config{Bucket: "bucket", Prefix: "/deployment/engine/"}
	first, err := NewS3(t.Context(), settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_ENDPOINT_URL_S3", "https://second-storage.example/")
	second, err := NewS3(t.Context(), settings)
	if err != nil {
		t.Fatal(err)
	}
	settings.Endpoint = "https://first-storage.example"
	settings.Prefix = "deployment/engine"
	explicit, err := NewS3(t.Context(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if first.Identity() == second.Identity() || first.Identity() != explicit.Identity() || len(first.Identity()) != 64 {
		t.Fatal("namespace identity ignored SDK endpoint override or normalization")
	}
	t.Setenv("AWS_ENDPOINT_URL_S3", "https://secret@private/")
	settings.Endpoint = ""
	if _, err := NewS3(t.Context(), settings); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe SDK endpoint override accepted: %v", err)
	}
}
