// Package docdom defines the document aggregate and its domain contracts.
package docdom

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// DocFolder represents a folder for organising documents within a project.
// Folders can be nested via ParentID (nil = root-level folder).
type DocFolder struct {
	ID        uuid.UUID
	ProjectID uuid.UUID
	ParentID  *uuid.UUID // nil = root-level folder
	Name      string
	Position  int
	CreatedBy *uuid.UUID
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Document is a rich-text document backed by BlockNote JSON blocks.
type Document struct {
	Visibility       string
	OwnerUserID      *uuid.UUID
	SourceDocumentID *uuid.UUID
	SourceTitle      *string
	SourceProjectID  *uuid.UUID
	SourceUpdatedAt  *time.Time
	AccessVersion    int64
	TaskIDs          []uuid.UUID
	ID               uuid.UUID
	ProjectID        uuid.UUID
	FolderID         *uuid.UUID // nil = root (not in any folder)
	Title            string
	Content          json.RawMessage // BlockNote JSON array of block objects; nil = no content
	Position         int
	CreatedBy        *uuid.UUID
	UpdatedBy        *uuid.UUID
	CreatedAt        time.Time
	UpdatedAt        time.Time
	DeletedAt        *time.Time // non-nil = soft-deleted
}

// DocSnapshot is a point-in-time copy of a document's content.
// Snapshots are created automatically on significant saves and can be used to
// display history and compute diffs between versions.
type DocSnapshot struct {
	ID             uuid.UUID
	DocumentID     uuid.UUID
	Title          string
	Content        json.RawMessage // BlockNote JSON at the time of the snapshot
	SnapshotNumber int64           // monotonically increasing per document
	CreatedBy      *uuid.UUID
	CreatedByName  string // denormalised from project_members → users
	CreatedAt      time.Time
}
