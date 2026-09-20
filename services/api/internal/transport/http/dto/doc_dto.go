package dto

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	docdom "github.com/Paca-AI/api/internal/domain/doc"
)

// =============================================================================
// Folder DTOs
// =============================================================================

// CreateFolderRequest is the body for POST /projects/:projectId/docs/folders.
type CreateFolderRequest struct {
	Name     string     `json:"name" binding:"required"`
	ParentID *uuid.UUID `json:"parent_id"`
}

// UpdateFolderRequest is the body for PATCH /projects/:projectId/docs/folders/:folderId.
type UpdateFolderRequest struct {
	Name     string       `json:"name"`
	ParentID OptionalUUID `json:"parent_id"`
	Position *int         `json:"position"`
}

// DocFolderResponse is the public representation of a doc folder.
type DocFolderResponse struct {
	ID        uuid.UUID  `json:"id"`
	ProjectID uuid.UUID  `json:"project_id"`
	ParentID  *uuid.UUID `json:"parent_id,omitempty"`
	Name      string     `json:"name"`
	Position  int        `json:"position"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// DocFolderFromEntity maps a domain DocFolder to a DocFolderResponse DTO.
func DocFolderFromEntity(f *docdom.DocFolder) DocFolderResponse {
	return DocFolderResponse{
		ID:        f.ID,
		ProjectID: f.ProjectID,
		ParentID:  f.ParentID,
		Name:      f.Name,
		Position:  f.Position,
		CreatedAt: f.CreatedAt,
		UpdatedAt: f.UpdatedAt,
	}
}

// =============================================================================
// Document DTOs
// =============================================================================

// CreateDocumentRequest is the body for POST /projects/:projectId/docs.
type CreateDocumentRequest struct {
	Visibility       string          `json:"visibility"`
	SourceDocumentID *uuid.UUID      `json:"source_document_id"`
	TaskIDs          []uuid.UUID     `json:"task_ids"`
	OwnerUserID      *uuid.UUID      `json:"owner_user_id"`
	Title            string          `json:"title"`
	FolderID         *uuid.UUID      `json:"folder_id"`
	Content          json.RawMessage `json:"content"`
}

// UpdateDocumentRequest is the body for PATCH /projects/:projectId/docs/:docId.
type UpdateDocumentRequest struct {
	Visibility       json.RawMessage `json:"visibility"`
	OwnerUserID      json.RawMessage `json:"owner_user_id"`
	SourceDocumentID json.RawMessage `json:"source_document_id"`
	TaskIDs          json.RawMessage `json:"task_ids"`
	AccessVersion    json.RawMessage `json:"access_version"`
	Title            *string         `json:"title"`
	Content          OptionalJSON    `json:"content"`
	FolderID         OptionalUUID    `json:"folder_id"`
	Position         *int            `json:"position"`
}

// DocumentResponse is the full public representation of a document (including content).
type DocumentResponse struct {
	Visibility       string      `json:"visibility"`
	OwnerUserID      *uuid.UUID  `json:"owner_user_id"`
	SourceDocumentID *uuid.UUID  `json:"source_document_id,omitempty"`
	SourceTitle      *string     `json:"source_title,omitempty"`
	SourceProjectID  *uuid.UUID  `json:"source_project_id,omitempty"`
	SourceUpdatedAt  *time.Time  `json:"source_updated_at,omitempty"`
	AccessVersion    int64       `json:"access_version"`
	TaskIDs          []uuid.UUID `json:"task_ids"`

	ID        uuid.UUID       `json:"id"`
	ProjectID uuid.UUID       `json:"project_id"`
	FolderID  *uuid.UUID      `json:"folder_id,omitempty"`
	Title     string          `json:"title"`
	Content   json.RawMessage `json:"content"`
	Position  int             `json:"position"`
	CreatedBy *uuid.UUID      `json:"created_by,omitempty"`
	UpdatedBy *uuid.UUID      `json:"updated_by,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// DocumentListItemResponse is a lightweight representation used in list responses
// (content is omitted to keep responses lean).
type DocumentListItemResponse struct {
	Visibility       string      `json:"visibility"`
	OwnerUserID      *uuid.UUID  `json:"owner_user_id"`
	SourceDocumentID *uuid.UUID  `json:"source_document_id,omitempty"`
	SourceTitle      *string     `json:"source_title,omitempty"`
	SourceProjectID  *uuid.UUID  `json:"source_project_id,omitempty"`
	SourceUpdatedAt  *time.Time  `json:"source_updated_at,omitempty"`
	AccessVersion    int64       `json:"access_version"`
	TaskIDs          []uuid.UUID `json:"task_ids"`

	ID        uuid.UUID  `json:"id"`
	ProjectID uuid.UUID  `json:"project_id"`
	FolderID  *uuid.UUID `json:"folder_id,omitempty"`
	Title     string     `json:"title"`
	Position  int        `json:"position"`
	CreatedBy *uuid.UUID `json:"created_by,omitempty"`
	UpdatedBy *uuid.UUID `json:"updated_by,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// DocumentFromEntity maps a domain Document to a DocumentResponse DTO.
func DocumentFromEntity(d *docdom.Document) DocumentResponse {
	return DocumentResponse{
		Visibility: d.Visibility, OwnerUserID: d.OwnerUserID, SourceDocumentID: d.SourceDocumentID, SourceTitle: d.SourceTitle, SourceProjectID: d.SourceProjectID, SourceUpdatedAt: d.SourceUpdatedAt, AccessVersion: d.AccessVersion, TaskIDs: d.TaskIDs,
		ID:        d.ID,
		ProjectID: d.ProjectID,
		FolderID:  d.FolderID,
		Title:     d.Title,
		Content:   d.Content,
		Position:  d.Position,
		CreatedBy: d.CreatedBy,
		UpdatedBy: d.UpdatedBy,
		CreatedAt: d.CreatedAt,
		UpdatedAt: d.UpdatedAt,
	}
}

// DocumentListItemFromEntity maps a domain Document to a DocumentListItemResponse DTO.
func DocumentListItemFromEntity(d *docdom.Document) DocumentListItemResponse {
	return DocumentListItemResponse{
		Visibility: d.Visibility, OwnerUserID: d.OwnerUserID, SourceDocumentID: d.SourceDocumentID, SourceTitle: d.SourceTitle, SourceProjectID: d.SourceProjectID, SourceUpdatedAt: d.SourceUpdatedAt, AccessVersion: d.AccessVersion, TaskIDs: d.TaskIDs,
		ID:        d.ID,
		ProjectID: d.ProjectID,
		FolderID:  d.FolderID,
		Title:     d.Title,
		Position:  d.Position,
		CreatedBy: d.CreatedBy,
		UpdatedBy: d.UpdatedBy,
		CreatedAt: d.CreatedAt,
		UpdatedAt: d.UpdatedAt,
	}
}

// =============================================================================
// Snapshot DTOs
// =============================================================================

// DocSnapshotResponse is the public representation of a document snapshot.
type DocSnapshotResponse struct {
	ID             uuid.UUID       `json:"id"`
	DocumentID     uuid.UUID       `json:"document_id"`
	Title          string          `json:"title"`
	Content        json.RawMessage `json:"content"`
	SnapshotNumber int64           `json:"snapshot_number"`
	CreatedBy      *uuid.UUID      `json:"created_by,omitempty"`
	CreatedByName  string          `json:"created_by_name,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
}

// DocSnapshotFromEntity maps a domain DocSnapshot to a DocSnapshotResponse DTO.
func DocSnapshotFromEntity(s *docdom.DocSnapshot) DocSnapshotResponse {
	return DocSnapshotResponse{
		ID:             s.ID,
		DocumentID:     s.DocumentID,
		Title:          s.Title,
		Content:        s.Content,
		SnapshotNumber: s.SnapshotNumber,
		CreatedBy:      s.CreatedBy,
		CreatedByName:  s.CreatedByName,
		CreatedAt:      s.CreatedAt,
	}
}

// =============================================================================
// Activity DTOs
// =============================================================================

// AddDocCommentRequest is the body for POST /projects/:projectId/docs/:docId/comments.
type AddDocCommentRequest struct {
	Content json.RawMessage `json:"content" binding:"required"`
}

// UpdateDocCommentRequest is the body for PATCH /projects/:projectId/docs/:docId/comments/:commentId.
type UpdateDocCommentRequest struct {
	Content json.RawMessage `json:"content" binding:"required"`
}

// DocActivityResponse is the public representation of a doc activity entry.
type DocActivityResponse struct {
	ID            uuid.UUID  `json:"id"`
	DocumentID    uuid.UUID  `json:"document_id"`
	ActorID       *uuid.UUID `json:"actor_id,omitempty"`
	ActorName     string     `json:"actor_name,omitempty"`
	ActorUsername string     `json:"actor_username,omitempty"`
	// ActorAvatarURL/ActorAvatarThumbURL are presigned GET URLs, populated by
	// the handler (not this mapper) via attachmentdom.AvatarService — nil
	// when the actor has no avatar.
	ActorAvatarURL      *string         `json:"actor_avatar_url,omitempty"`
	ActorAvatarThumbURL *string         `json:"actor_avatar_thumb_url,omitempty"`
	ActivityType        string          `json:"activity_type"`
	Content             json.RawMessage `json:"content"`
	CreatedAt           time.Time       `json:"created_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
	DeletedAt           *time.Time      `json:"deleted_at,omitempty"`
}

// DocActivityFromEntity maps a domain Activity to a DocActivityResponse DTO.
func DocActivityFromEntity(a *docdom.Activity) DocActivityResponse {
	return DocActivityResponse{
		ID:            a.ID,
		DocumentID:    a.DocumentID,
		ActorID:       a.ActorID,
		ActorName:     a.ActorName,
		ActorUsername: a.ActorUsername,
		ActivityType:  string(a.ActivityType),
		Content:       a.Content,
		CreatedAt:     a.CreatedAt,
		UpdatedAt:     a.UpdatedAt,
		DeletedAt:     a.DeletedAt,
	}
}
