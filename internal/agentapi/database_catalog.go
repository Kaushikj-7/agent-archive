package agentapi

import (
	"context"
	"time"
)

// DatabaseChat is compact native catalog evidence for a possible historical session.
// KeyID is the native record locator; identity disagreement is not silently repaired.
type DatabaseChat struct {
	CursorFacts CursorComposerFacts `json:"CursorFacts"`
	ID          string              `json:"ID"`
	KeyID       string              `json:"KeyID"`
	CreatedAt   time.Time           `json:"CreatedAt"`
	Folder      string              `json:"Folder"`
	WorkspaceID string              `json:"WorkspaceID"`
	// WorkspaceIdentifier reports that the native record carried a workspace
	// reference at all, even one that names no local folder or ID (a remote
	// or unknown URI shape). Such a chat is not known to be folderless.
	WorkspaceIdentifier bool `json:"WorkspaceIdentifier"`
	Malformed           bool `json:"Malformed"`
}

// DatabaseCatalog retains only compact planning metadata, never raw message content.
// A native catalog may need parent IDs before excluding child sessions.
type DatabaseCatalog struct {
	Chats       []DatabaseChat
	NewerFormat int
	Subagents   map[string][]string
}

// DatabaseRecord borrows one indexed native metadata value for its callback.
// No raw value remains valid after the callback returns.
type DatabaseRecord struct {
	Key   string
	Value []byte
}

// DatabaseCatalogHost owns query cursors, row errors and cleanup; the native
// owner chooses its indexed metadata query and interprets each borrowed value.
type DatabaseCatalogHost interface {
	Query(context.Context, string, func(DatabaseRecord) error) error
}

// DatabaseCatalogInspector owns native catalog schema and metadata interpretation.
type DatabaseCatalogInspector interface {
	InspectCatalog(context.Context, DatabaseCatalogHost) (DatabaseCatalog, error)
}

// DatabaseCatalogLookup projects implemented native metadata inventory owners.
type DatabaseCatalogLookup interface {
	LookupDatabaseCatalog(string) (DatabaseCatalogInspector, bool)
}

// CursorComposerFacts is comparable review data, never admission authority.
type CursorComposerFacts struct {
	VersionPresent bool                    `json:"VersionPresent"`
	Version        int                     `json:"Version"`
	Relationships  CursorRelationshipShape `json:"Relationships"`
	ChildIDsSHA256 string                  `json:"ChildIDsSHA256"`
}

// CursorRelationshipShape distinguishes absent relationships from malformed input.
type CursorRelationshipShape string

const (
	CursorRelationshipsAbsent    CursorRelationshipShape = "absent"
	CursorRelationshipsValid     CursorRelationshipShape = "valid"
	CursorRelationshipsMalformed CursorRelationshipShape = "malformed"
)
