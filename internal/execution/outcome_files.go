package execution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/knotra-dev/engine/internal/blob"
	"github.com/knotra-dev/engine/internal/fsutil"
)

var ErrInvalidOutcome = errors.New("invalid durable outcome")

var ErrOutcomeConflict = errors.New("outcome already exists with different evidence")

type outcomeFile struct {
	SHA256 string          `json:"sha256"`
	Data   json.RawMessage `json:"data"`
}

// OutcomeFiles holds durable execution evidence on a filesystem or object store.
// Outcomes and initialization identities are immutable; a serialized MCP session
// replaces its current-call record. Backups must include uncommitted outcomes
// and MCP IDs together with the database.
type OutcomeFiles struct {
	root  *os.Root
	blobs blob.Store
}

func OpenRemoteOutcomeFiles(blobs blob.Store) (*OutcomeFiles, error) {
	if blobs == nil {
		return nil, errors.New("durable outcome storage is required")
	}
	return &OutcomeFiles{blobs: blobs}, nil
}

func OpenOutcomeFiles(directory string) (*OutcomeFiles, error) {
	root, err := fsutil.OpenPrivateRoot(directory)
	if err != nil {
		return nil, err
	}
	return &OutcomeFiles{root: root}, nil
}

func (s *OutcomeFiles) Close() error {
	if s.root == nil {
		return nil
	}
	return s.root.Close()
}

// SavedAt reads envelope metadata without loading potentially large outputs.
func (s *OutcomeFiles) SavedAt(key string) (time.Time, error) {
	return s.SavedAtContext(context.Background(), key)
}

func (s *OutcomeFiles) SavedAtContext(ctx context.Context, key string) (time.Time, error) {
	if !validKey(key) {
		return time.Time{}, fmt.Errorf("%w: invalid outcome key", ErrInvalidOutcome)
	}
	if s.blobs != nil {
		return s.blobs.SavedAt(ctx, "outcomes/"+key+".json")
	}
	info, err := s.root.Lstat(key + ".json")
	if err != nil {
		return time.Time{}, err
	}
	if !info.Mode().IsRegular() {
		return time.Time{}, fmt.Errorf("%w: outcome must be a regular file", ErrInvalidOutcome)
	}
	return info.ModTime(), nil
}

// Put waits for durable publication without replacing an existing result.
// Identical delivery is idempotent; conflicting evidence is an explicit error.
func (s *OutcomeFiles) Put(outcome Outcome) (string, error) {
	if err := outcome.Validate(); err != nil {
		return "", err
	}
	key, err := outcome.Ownership.OutcomeKey()
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(outcome)
	if err != nil {
		return "", err
	}
	return s.put(key, data)
}

func (s *OutcomeFiles) put(key string, data []byte) (string, error) {
	return s.publish(key, data, false)
}

func (s *OutcomeFiles) publish(key string, data []byte, replace bool) (string, error) {
	sum := sha256.Sum256(data)
	encoded, err := json.Marshal(outcomeFile{SHA256: hex.EncodeToString(sum[:]), Data: data})
	if err != nil {
		return "", err
	}
	if s.blobs != nil {
		if err := s.blobs.Put(context.Background(), "outcomes/"+key+".json", encoded, replace); err != nil {
			if errors.Is(err, blob.ErrConflict) {
				return "", ErrOutcomeConflict
			}
			return "", err
		}
		return key, nil
	}
	temporary := ".outcome-" + uuid.NewString()
	file, err := s.root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	defer func() { _ = s.root.Remove(temporary) }()
	if _, err := file.Write(encoded); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	if replace {
		if err := s.root.Rename(temporary, key+".json"); err != nil {
			return "", err
		}
	} else if err := s.root.Link(temporary, key+".json"); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
		previous, err := s.read(key)
		if err != nil {
			return "", err
		}
		if !bytes.Equal(previous, data) {
			return "", ErrOutcomeConflict
		}
	}
	dir, err := s.root.Open(".")
	if err != nil {
		return "", err
	}
	err = fsutil.SyncOpenDir(dir)
	closeErr := dir.Close()
	if err != nil {
		return "", err
	}
	return key, closeErr
}

func validKey(key string) bool {
	if len(key) != sha256.Size*2 {
		return false
	}
	for _, c := range key {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func decodeStrict(data []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("unexpected trailing outcome data")
	}
	return nil
}

func (s *OutcomeFiles) read(key string) ([]byte, error) {
	return s.readBoundedContext(context.Background(), key, 0)
}

func (s *OutcomeFiles) readBoundedContext(ctx context.Context, key string, limit int64) ([]byte, error) {
	if !validKey(key) {
		return nil, fmt.Errorf("%w: invalid outcome key", ErrInvalidOutcome)
	}
	data, err := s.readEnvelope(ctx, key, limit)
	if errors.Is(err, blob.ErrTooLarge) {
		return nil, fmt.Errorf("%w: evidence exceeds byte limit", ErrInvalidOutcome)
	}
	if err != nil {
		return nil, err
	}
	var envelope outcomeFile
	if err := decodeStrict(data, &envelope); err != nil {
		return nil, fmt.Errorf("%w: decode envelope: %w", ErrInvalidOutcome, err)
	}
	sum := sha256.Sum256(envelope.Data)
	if hex.EncodeToString(sum[:]) != envelope.SHA256 {
		return nil, fmt.Errorf("%w: checksum mismatch", ErrInvalidOutcome)
	}
	return envelope.Data, nil
}

func (s *OutcomeFiles) readEnvelope(ctx context.Context, key string, limit int64) ([]byte, error) {
	if s.blobs != nil {
		return s.blobs.Get(ctx, "outcomes/"+key+".json", limit)
	}
	name := key + ".json"
	info, err := s.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: outcome must be a regular file", ErrInvalidOutcome)
	}
	file, err := s.root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	// These are executor-produced durable records. An arbitrary byte cap here
	// would make a contract-valid large output impossible to recover.
	var reader io.Reader = file
	if limit > 0 {
		reader = io.LimitReader(file, limit+1)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if limit > 0 && int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: evidence exceeds byte limit", ErrInvalidOutcome)
	}
	return data, nil
}

func (s *OutcomeFiles) Get(key string) (Outcome, error) {
	return s.GetContext(context.Background(), key)
}

func (s *OutcomeFiles) GetContext(ctx context.Context, key string) (Outcome, error) {
	var outcome Outcome
	data, err := s.readBoundedContext(ctx, key, 0)
	if err != nil {
		return outcome, err
	}
	if err := decodeStrict(data, &outcome); err != nil {
		return outcome, errors.Join(ErrInvalidOutcome, err)
	}
	if err := outcome.Validate(); err != nil {
		return outcome, errors.Join(ErrInvalidOutcome, err)
	}
	expected, err := outcome.Ownership.OutcomeKey()
	if err != nil || expected != key {
		return outcome, fmt.Errorf("%w: ownership does not match storage key", ErrInvalidOutcome)
	}
	return outcome, nil
}
