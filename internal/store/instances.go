package store

import (
	"context"
	"encoding/json"

	"github.com/knotra-dev/engine/internal/protocol"
	"github.com/knotra-dev/engine/internal/store/db"
)

// MaxInstancePageBytes bounds the instance contribution to run snapshots and pages.
const MaxInstancePageBytes = 1 << 20

func (s *Store) Instances(ctx context.Context, runID, cursor string) (protocol.Page[protocol.Instance], error) {
	documents, err := db.New(s.Pool).ListInstances(ctx, db.ListInstancesParams{RunID: runID, Cursor: cursor, MaxBytes: MaxInstancePageBytes})
	if err != nil {
		return protocol.Page[protocol.Instance]{}, err
	}
	return instancePage(documents)
}

func instancePage(documents [][]byte) (protocol.Page[protocol.Instance], error) {
	page := protocol.Page[protocol.Instance]{Items: []protocol.Instance{}}
	size := 0
	for _, document := range documents {
		var instance protocol.Instance
		if err := json.Unmarshal(document, &instance); err != nil {
			return page, err
		}
		// Match the SQL byte budget so its extra record always remains a cursor marker.
		if len(page.Items) > 0 && (len(page.Items) >= 100 || size+len(document) > MaxInstancePageBytes) {
			cursor := page.Items[len(page.Items)-1].ID
			page.NextCursor = &cursor
			break
		}
		page.Items = append(page.Items, instance)
		size += len(document)
	}
	return page, nil
}
