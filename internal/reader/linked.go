package reader

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/wangjohn/agent-archive/internal/archive"
	"github.com/wangjohn/agent-archive/internal/storage"
)

// LinkedAvailability is a read-time observation, separate from the historical
// link in a source bundle. Metadata availability does not verify child source
// bytes; show CHILD --transcript performs that check on explicit selection.
type LinkedAvailability struct {
	SessionID string      `json:"session_id"`
	State     LinkedState `json:"state"`
}

// LinkedState is what a read found for one linked session.
type LinkedState string

const (
	// LinkedStateUnavailable means the link is not a subagent link, or the
	// parent recorded the child as unavailable.
	LinkedStateUnavailable LinkedState = "unavailable"
	// LinkedStateIdentityMismatch means the child's key or metadata does not
	// describe a child of this parent.
	LinkedStateIdentityMismatch LinkedState = "identity_mismatch"
	// LinkedStatePending means the child has not published yet.
	LinkedStatePending LinkedState = "pending"
	// LinkedStateUnavailableOrExpired means the child's metadata is gone.
	LinkedStateUnavailableOrExpired LinkedState = "unavailable_or_expired"
	// LinkedStateLookupFailed means reading the child's metadata failed.
	LinkedStateLookupFailed LinkedState = "lookup_failed"
	// LinkedStateMetadataAvailable means the child's metadata is readable and
	// matches the parent.
	LinkedStateMetadataAvailable LinkedState = "metadata_available"
)

// ResolveLinkedSessions reads only direct child metadata. It does not follow
// grandchildren, download transcripts, or pin children against retention.
// One missing or malformed child must not make its parent's metadata unreadable.
func ResolveLinkedSessions(ctx context.Context, store storage.ObjectStore, parent archive.Metadata) []LinkedAvailability {
	if len(parent.LinkedSessions) == 0 {
		return nil
	}
	result := make([]LinkedAvailability, len(parent.LinkedSessions))

	var next atomic.Int64
	var workers sync.WaitGroup
	for range min(8, len(result)) {
		workers.Go(func() {
			for {
				i := int(next.Add(1) - 1)
				if i >= len(result) {
					return
				}
				result[i] = resolveLinkedSession(ctx, store, parent, parent.LinkedSessions[i])
			}
		})
	}
	workers.Wait()
	return result
}

func resolveLinkedSession(ctx context.Context, store storage.ObjectStore, parent archive.Metadata, link archive.LinkedSessionReference) LinkedAvailability {
	item := LinkedAvailability{SessionID: link.SessionID, State: LinkedStateUnavailable}
	if link.Relationship != "subagent" || link.Status == archive.LinkedSessionUnavailable {
		return item
	}
	key, err := archive.MetadataObjectKey(parent.Harness.Name, link.SessionID)
	if err != nil || link.SessionID == parent.SessionID {
		item.State = LinkedStateIdentityMismatch
		return item
	}
	if ctx.Err() != nil {
		item.State = LinkedStateLookupFailed
		return item
	}
	child, err := ReadMetadata(ctx, store, key)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		if link.Status == archive.LinkedSessionPending {
			item.State = LinkedStatePending
		} else {
			item.State = LinkedStateUnavailableOrExpired
		}
	case err != nil:
		item.State = LinkedStateLookupFailed
	case child.SessionID != link.SessionID || child.ParentSessionID != parent.SessionID || child.ProjectID != parent.ProjectID || child.MachineID != parent.MachineID || child.Harness.Name != parent.Harness.Name:
		item.State = LinkedStateIdentityMismatch
	default:
		item.State = LinkedStateMetadataAvailable
	}
	return item
}
