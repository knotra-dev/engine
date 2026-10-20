// Package blob stores shared durable bytes independently of PostgreSQL.
package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

var ErrConflict = errors.New("durable object already exists with different bytes")
var ErrTooLarge = errors.New("durable object exceeds byte limit")

// Store requires atomic writes, conditional creation and consistent reads.
// A successful write must remain available after the writer process exits.
type Store interface {
	// A zero limit permits complete contract-valid outcome records.
	Get(ctx context.Context, key string, limit int64) ([]byte, error)
	// With replace=false, identical bytes are idempotent and different bytes conflict.
	Put(ctx context.Context, key string, data []byte, replace bool) error
	SavedAt(ctx context.Context, key string) (time.Time, error)
}

type S3Config struct {
	Bucket    string
	Prefix    string
	Region    string
	Endpoint  string
	PathStyle bool
}

const requestTimeout = 30 * time.Second

type s3Store struct {
	client   *s3.Client
	bucket   string
	prefix   string
	identity string
}

// NewS3 uses the standard AWS credential chain, including environment and workload
// identities. Construction does not probe or create buckets.
func NewS3(ctx context.Context, settings S3Config) (*s3Store, error) {
	if settings.Bucket == "" || strings.ContainsAny(settings.Bucket, "/\\\r\n\t ") {
		return nil, errors.New("invalid S3 bucket")
	}
	if _, err := normalizeEndpoint(settings.Endpoint); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var options []func(*config.LoadOptions) error
	if settings.Region != "" {
		options = append(options, config.WithRegion(settings.Region))
	}
	cfg, err := config.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return nil, storageError(ctx, "configuration", err)
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	client := s3.NewFromConfig(cfg, func(options *s3.Options) {
		if settings.Endpoint != "" {
			options.BaseEndpoint = aws.String(settings.Endpoint)
		}
		options.UsePathStyle = settings.PathStyle
		options.RetryMaxAttempts = 3
		// Optional AWS checksum negotiation is not supported by every S3 provider.
		// Artifact hashes and outcome envelopes validate the stored bytes instead.
		options.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		options.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	})
	endpoint, err := normalizeEndpoint(aws.ToString(client.Options().BaseEndpoint))
	if err != nil {
		return nil, err
	}
	prefix := strings.Trim(settings.Prefix, "/")
	identity, _ := json.Marshal([]string{endpoint, cfg.Region, settings.Bucket, prefix})
	sum := sha256.Sum256(identity)
	return &s3Store{client: client, bucket: settings.Bucket, prefix: prefix, identity: hex.EncodeToString(sum[:])}, nil
}

// Identity binds a database to the resolved namespace, including SDK endpoint overrides.
func (s *s3Store) Identity() string { return s.identity }

func normalizeEndpoint(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	endpoint, err := url.Parse(value)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return "", errors.New("invalid S3 endpoint")
	}
	endpoint.Host = strings.ToLower(endpoint.Host)
	return strings.TrimRight(endpoint.String(), "/"), nil
}

func (s *s3Store) key(key string) string {
	if s.prefix == "" {
		return key
	}
	return s.prefix + "/" + key
}

func (s *s3Store) Get(ctx context.Context, key string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	return s.get(ctx, key, limit)
}

func (s *s3Store) get(ctx context.Context, key string, limit int64) ([]byte, error) {
	result, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.key(key))})
	if err != nil {
		return nil, storageError(ctx, "read", err)
	}
	defer func() { _ = result.Body.Close() }()
	if limit > 0 && result.ContentLength != nil && *result.ContentLength > limit {
		return nil, ErrTooLarge
	}
	var reader io.Reader = result.Body
	if limit > 0 {
		reader = io.LimitReader(reader, limit+1)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, storageError(ctx, "read", err)
	}
	if limit > 0 && int64(len(data)) > limit {
		return nil, ErrTooLarge
	}
	return data, nil
}

func (s *s3Store) Put(ctx context.Context, key string, data []byte, replace bool) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	input := &s3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.key(key)), Body: bytes.NewReader(data), ContentLength: aws.Int64(int64(len(data)))}
	if replace {
		previous, err := s.head(ctx, key)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			input.IfNoneMatch = aws.String("*")
		} else {
			if previous.ETag == nil || *previous.ETag == "" {
				return errors.New("durable object has no version for conditional replacement")
			}
			// A timed-out older request must not overwrite a later call record.
			input.IfMatch = previous.ETag
		}
	} else {
		input.IfNoneMatch = aws.String("*")
	}
	_, err := s.client.PutObject(ctx, input)
	if err == nil {
		return nil
	}
	if statusCode(err) == http.StatusPreconditionFailed || statusCode(err) == http.StatusConflict {
		// A timed-out upload may have succeeded; compare instead of overwriting.
		previous, readErr := s.get(ctx, key, int64(len(data))+1)
		if errors.Is(readErr, ErrTooLarge) || (readErr == nil && !bytes.Equal(previous, data)) {
			return ErrConflict
		}
		return readErr
	}
	return storageError(ctx, "write", err)
}

func (s *s3Store) SavedAt(ctx context.Context, key string) (time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	result, err := s.head(ctx, key)
	if err != nil {
		return time.Time{}, err
	}
	if result.LastModified == nil {
		return time.Time{}, errors.New("durable object has no modification time")
	}
	return *result.LastModified, nil
}

func (s *s3Store) head(ctx context.Context, key string) (*s3.HeadObjectOutput, error) {
	result, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(s.key(key))})
	if err != nil {
		if statusCode(err) == http.StatusNotFound {
			// HEAD omits the error body, so distinguish a missing bucket with GET.
			_, readErr := s.get(ctx, key, 1)
			if readErr != nil && !errors.Is(readErr, ErrTooLarge) {
				return nil, readErr
			}
			return nil, errors.New("durable object metadata read failed (HTTP 404)")
		}
		return nil, storageError(ctx, "metadata read", err)
	}
	return result, nil
}

func statusCode(err error) int {
	var response interface{ HTTPStatusCode() int }
	if errors.As(err, &response) {
		return response.HTTPStatusCode()
	}
	return 0
}

// Provider errors may contain credentials, request URLs or response bodies.
func storageError(ctx context.Context, operation string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if statusCode(err) == http.StatusNotFound {
		var apiError interface{ ErrorCode() string }
		if errors.As(err, &apiError) && apiError.ErrorCode() == "NoSuchKey" {
			return os.ErrNotExist
		}
	}
	if status := statusCode(err); status != 0 {
		return fmt.Errorf("durable object %s failed (HTTP %d)", operation, status)
	}
	return errors.New("durable object " + operation + " failed")
}
