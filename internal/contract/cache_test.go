package contract

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestExpressionCacheBounded(t *testing.T) {
	testCompilationCache(t, &expressions, expression, func(i, size int) string {
		source := fmt.Sprintf("args.cache_bound_%d + 0.5", i)
		return source + strings.Repeat(" ", max(0, size-len(source)))
	}, 8192, func(compiled *checkedExpression) error {
		if !reflect.DeepEqual(compiled.refs, []string{"args.cache_bound_0"}) {
			return fmt.Errorf("references changed: %v", compiled.refs)
		}
		result, _, err := compiled.program.Eval(map[string]any{"args": map[string]any{"cache_bound_0": 1.0}})
		if err != nil {
			return err
		}
		if result.Value() != 1.5 {
			return fmt.Errorf("numeric result changed: %v", result)
		}
		return nil
	})
}

func TestDataSchemaCacheBounded(t *testing.T) {
	testCompilationCache(t, &dataSchemas, func(source string) (*jsonschema.Schema, error) {
		return compileDataSchema(json.RawMessage(source))
	}, func(i, size int) string {
		source := fmt.Sprintf(`{"$comment":"cache_bound_%d","$defs":{"exact":{"const":9007199254740993}},"$ref":"#/$defs/exact"}`, i)
		return source + strings.Repeat(" ", max(0, size-len(source)))
	}, 256<<10, func(compiled *jsonschema.Schema) error {
		if err := compiled.Validate(int64(9007199254740993)); err != nil {
			return err
		}
		if err := compiled.Validate(int64(9007199254740992)); err == nil {
			return fmt.Errorf("schema lost exact numeric constraint")
		}
		return nil
	})
}

func testCompilationCache[T comparable](t *testing.T, cache *compilationCache[T], compile func(string) (T, error), source func(int, int) string, maximumSourceBytes int, validate func(T) error) {
	t.Helper()
	for _, test := range []struct {
		name        string
		size, count int
	}{
		{"entries", 0, compilationCacheEntries + 1},
		{"key_bytes", maximumSourceBytes, compilationCacheKeyBytes/maximumSourceBytes + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			key := source(0, test.size)
			first, err := compile(key)
			if err != nil {
				t.Fatal(err)
			}
			if reused, err := compile(key); err != nil || reused != first {
				t.Fatalf("cached result was not reused: %v", err)
			}
			for i := 1; i < test.count; i++ {
				if _, err := compile(source(i, test.size)); err != nil {
					t.Fatal(err)
				}
			}
			assertCompilationCacheBounded(t, cache)
			if _, cached := cache.load(key); cached {
				t.Fatal("old compilation retained after exceeding cache limit")
			}
			if recompiled, err := compile(key); err != nil || recompiled == first {
				t.Fatalf("evicted result was not recompiled: %v", err)
			}
			if err := validate(first); err != nil {
				t.Fatalf("eviction changed a result still in use: %v", err)
			}
		})
	}

	t.Run("concurrent_reuse_and_eviction", func(t *testing.T) {
		key := source(0, 0)
		first, err := compile(key)
		if err != nil {
			t.Fatal(err)
		}
		var workers sync.WaitGroup
		for worker := range 16 {
			workers.Go(func() {
				for i := range 32 {
					if _, err := compile(key); err != nil {
						t.Error(err)
						return
					}
					if _, err := compile(source(1000+worker*32+i, 0)); err != nil {
						t.Error(err)
						return
					}
					if err := validate(first); err != nil {
						t.Errorf("concurrent use of evicted result: %v", err)
						return
					}
				}
			})
		}
		workers.Wait()
		assertCompilationCacheBounded(t, cache)
	})
}

func assertCompilationCacheBounded[T any](t *testing.T, cache *compilationCache[T]) {
	t.Helper()
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	keyBytes := 0
	for key := range cache.entries {
		keyBytes += len(key)
	}
	if len(cache.entries) > compilationCacheEntries || keyBytes > compilationCacheKeyBytes {
		t.Fatalf("cache retained %d entries and %d key bytes", len(cache.entries), keyBytes)
	}
}
