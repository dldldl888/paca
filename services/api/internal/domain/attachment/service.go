package attachmentdom

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/Paca-AI/api/internal/platform/storage"
)

// Service is the combined attachment management contract.
type Service interface {
	GetAttachmentFile(ctx context.Context, projectID, taskID, attachmentID uuid.UUID) (*File, error)
	// InitiateUpload creates a pending File record and returns either a
	// single-part presigned PUT URL (files < MultipartThreshold) or the
	// multipart upload descriptor with per-part presigned URLs.
	// Verifies taskID belongs to projectID before proceeding.
	InitiateUpload(ctx context.Context, projectID uuid.UUID, in InitiateUploadInput) (*UploadSession, error)

	// CompleteUpload marks the File as uploaded and creates the
	// TaskAttachment join record.  For multipart uploads the caller must
	// supply the completed parts so the server can call
	// CompleteMultipartUpload on the object store.
	// Verifies taskID belongs to projectID before proceeding.
	CompleteUpload(ctx context.Context, projectID uuid.UUID, in CompleteUploadInput) (*TaskAttachment, error)

	// GetDownloadURL returns a same-origin authenticated API URL. The ttl
	// argument is retained for caller compatibility; URLs grant no bearer access.
	GetDownloadURL(ctx context.Context, projectID, taskID, attachmentID uuid.UUID, ttl time.Duration, forceDownload bool) (string, error)

	// GetAttachmentContent preserves the bounded raw-read contract for internal callers.
	// Browser delivery uses the full streaming download route; MCP's HTTP content
	// route retains MaxAttachmentContentSize and returns ErrAttachmentContentTooLarge.
	GetAttachmentContent(ctx context.Context, projectID, taskID, attachmentID uuid.UUID) (data []byte, contentType string, err error)

	// ListTaskAttachments returns all confirmed attachments for the given task.
	// Verifies taskID belongs to projectID before proceeding.
	ListTaskAttachments(ctx context.Context, projectID, taskID uuid.UUID) ([]*TaskAttachment, error)

	// DeleteTaskAttachment removes the task→file association for the given
	// attachment. Verifies the attachment belongs to taskID and that taskID
	// belongs to projectID before deleting.
	DeleteTaskAttachment(ctx context.Context, projectID, taskID, attachmentID uuid.UUID) error
}

// UploadSession is returned by InitiateUpload and carries everything the
// client needs to upload the file directly to the object store.
type UploadSession struct {
	FileID      uuid.UUID `json:"file_id"`
	IsMultipart bool      `json:"is_multipart"`
	// UploadURL is set for single-part uploads (IsMultipart=false).
	UploadURL string `json:"upload_url,omitempty"`
	// Multipart is set for large files (IsMultipart=true).
	Multipart *storage.MultipartUpload `json:"multipart,omitempty"`
}

// InitiateUploadInput carries the client-supplied file metadata for starting
// an upload.
type InitiateUploadInput struct {
	TaskID      uuid.UUID
	FileName    string
	ContentType string
	FileSize    int64
	UploadedBy  uuid.UUID // user performing the upload
}

// CompletedPart mirrors storage.CompletedPart for multipart completion.
type CompletedPart struct {
	PartNumber int
	ETag       string
}

// CompleteUploadInput carries parameters for finishing an upload.
type CompleteUploadInput struct {
	FileID    uuid.UUID
	TaskID    uuid.UUID
	CreatedBy uuid.UUID
	// UploadID and Parts are required only for multipart uploads.
	UploadID *string
	Parts    []CompletedPart
}
