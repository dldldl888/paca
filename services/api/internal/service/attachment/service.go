// Package attachmentsvc implements the attachment application service.
package attachmentsvc

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"

	attachmentdom "github.com/Paca-AI/api/internal/domain/attachment"
	docdom "github.com/Paca-AI/api/internal/domain/doc"
	taskdom "github.com/Paca-AI/api/internal/domain/task"
	"github.com/Paca-AI/api/internal/platform/authz"
	"github.com/Paca-AI/api/internal/platform/storage"
)

// presignedUploadTTL is how long a presigned upload URL remains valid.
const presignedUploadTTL = time.Hour

// Service is the concrete implementation of attachmentdom.Service.
type Service struct {
	repo        attachmentdom.Repository
	taskChecker taskdom.AccessChecker
	docChecker  attachmentdom.DocOwnerChecker
	store       storage.Client
	bucket      string
}

// New returns a configured attachment service.
func New(repo attachmentdom.Repository, taskChecker taskdom.AccessChecker, docChecker attachmentdom.DocOwnerChecker, store storage.Client, bucket string) *Service {
	return &Service{repo: repo, taskChecker: taskChecker, docChecker: docChecker, store: store, bucket: bucket}
}

// InitiateUpload creates a pending File record and returns a presigned upload
// session.  For files >= MultipartThreshold a multipart upload is initiated
// with pre-signed URLs for each part; otherwise a single-part presigned PUT
// URL is returned.
func (s *Service) InitiateUpload(ctx context.Context, projectID uuid.UUID, in attachmentdom.InitiateUploadInput) (*attachmentdom.UploadSession, error) {
	if err := s.checkTask(ctx, projectID, in.TaskID, taskdom.AccessProgress); err != nil {
		return nil, err
	}
	actor, ok := authz.ActorIDFromContext(ctx)
	if !ok {
		return nil, attachmentdom.ErrFileNotFound
	}
	if strings.TrimSpace(in.FileName) == "" {
		return nil, attachmentdom.ErrFileNameEmpty
	}
	if strings.TrimSpace(in.ContentType) == "" {
		return nil, attachmentdom.ErrContentTypeEmpty
	}
	if in.FileSize <= 0 {
		return nil, attachmentdom.ErrFileSizeZero
	}

	fileID := uuid.New()
	// Build a storage key that is unique per file and organised by task.
	safeFileName := sanitizeFileName(in.FileName)
	storageKey := fmt.Sprintf("tasks/%s/%s/%s", in.TaskID.String(), fileID.String(), safeFileName)

	now := time.Now()
	f := &attachmentdom.File{
		ID:           fileID,
		StorageKey:   storageKey,
		Bucket:       s.bucket,
		FileName:     in.FileName,
		ContentType:  in.ContentType,
		FileSize:     in.FileSize,
		UploadStatus: attachmentdom.UploadStatusPending,
		UploadedBy:   &actor,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	if err := s.repo.CreateFile(ctx, f); err != nil {
		return nil, fmt.Errorf("attachment svc: create file: %w", err)
	}

	session := &attachmentdom.UploadSession{FileID: fileID}

	if in.FileSize >= storage.MultipartThreshold {
		// Multipart upload path.
		mu, err := s.store.InitiateMultipartUpload(ctx, s.bucket, storageKey, in.ContentType, in.FileSize, storage.DefaultPartSize, presignedUploadTTL)
		if err != nil {
			return nil, fmt.Errorf("attachment svc: initiate multipart: %w", err)
		}
		// Persist the S3 upload ID so CompleteUpload can reference it.
		if err := s.repo.UpdateFileStatus(ctx, fileID, attachmentdom.UploadStatusPending, &mu.UploadID); err != nil {
			return nil, fmt.Errorf("attachment svc: save upload id: %w", err)
		}
		session.IsMultipart = true
		session.Multipart = mu
	} else {
		// Single-part upload path.
		uploadURL, err := s.store.PresignPutObject(ctx, s.bucket, storageKey, in.ContentType, presignedUploadTTL)
		if err != nil {
			return nil, fmt.Errorf("attachment svc: presign put: %w", err)
		}
		session.UploadURL = uploadURL
	}

	return session, nil
}

// CompleteUpload marks the file as uploaded and creates the TaskAttachment
// join record.  For multipart uploads the caller must supply the completed
// parts so the service can call CompleteMultipartUpload on the object store.
func (s *Service) CompleteUpload(ctx context.Context, projectID uuid.UUID, in attachmentdom.CompleteUploadInput) (*attachmentdom.TaskAttachment, error) {
	if err := s.checkTask(ctx, projectID, in.TaskID, taskdom.AccessProgress); err != nil {
		return nil, err
	}
	actor, ok := authz.ActorIDFromContext(ctx)
	if !ok {
		return nil, attachmentdom.ErrFileNotFound
	}
	f, err := s.repo.FindFileByID(ctx, in.FileID)
	if err != nil {
		return nil, err
	}
	if !uploadMatches(f, "tasks", in.TaskID, in.FileID, actor) {
		return nil, attachmentdom.ErrFileNotFound
	}
	if f.UploadStatus != attachmentdom.UploadStatusPending {
		return nil, attachmentdom.ErrUploadNotPending
	}

	switch {
	case f.MultipartUploadID != nil && in.UploadID == nil:
		// File was initiated as multipart but caller did not provide an upload ID.
		return nil, attachmentdom.ErrMultipartUploadIDRequired
	case f.MultipartUploadID == nil && in.UploadID != nil:
		// Caller supplied an upload ID but the file is not a multipart upload.
		return nil, attachmentdom.ErrNotMultipartUpload
	case f.MultipartUploadID != nil && in.UploadID != nil && *in.UploadID != *f.MultipartUploadID:
		// Upload IDs present but refer to different sessions.
		return nil, attachmentdom.ErrUploadIDMismatch
	case f.MultipartUploadID != nil && in.UploadID != nil && len(in.Parts) == 0:
		// Multipart upload requires at least one completed part.
		return nil, attachmentdom.ErrMultipartPartsEmpty
	}

	if in.UploadID != nil {
		// Complete the multipart upload with the storage provider.
		parts := make([]storage.CompletedPart, 0, len(in.Parts))
		for _, p := range in.Parts {
			parts = append(parts, storage.CompletedPart{
				PartNumber: p.PartNumber,
				ETag:       p.ETag,
			})
		}
		if err := s.store.CompleteMultipartUpload(ctx, s.bucket, f.StorageKey, *in.UploadID, parts); err != nil {
			return nil, fmt.Errorf("attachment svc: complete multipart: %w", err)
		}
	}

	// Mark the file as uploaded and clear the multipart upload ID.
	if err := s.repo.UpdateFileStatus(ctx, in.FileID, attachmentdom.UploadStatusUploaded, nil); err != nil {
		return nil, fmt.Errorf("attachment svc: update file status: %w", err)
	}

	now := time.Now()
	a := &attachmentdom.TaskAttachment{
		ID:        uuid.New(),
		TaskID:    in.TaskID,
		FileID:    in.FileID,
		CreatedBy: &actor,
		CreatedAt: now,
	}
	if err := s.repo.CreateTaskAttachment(ctx, a); err != nil {
		return nil, fmt.Errorf("attachment svc: create task attachment: %w", err)
	}

	// Populate the file field for the response.
	f.UploadStatus = attachmentdom.UploadStatusUploaded
	a.File = f
	return a, nil
}

// GetDownloadURL returns a stable API URL; every download rechecks current access.
func (s *Service) GetDownloadURL(ctx context.Context, projectID, taskID, attachmentID uuid.UUID, ttl time.Duration, forceDownload bool) (string, error) {
	if _, err := s.GetAttachmentFile(ctx, projectID, taskID, attachmentID); err != nil {
		return "", err
	}
	url := fmt.Sprintf("/api/v1/projects/%s/tasks/%s/attachments/%s/download", projectID, taskID, attachmentID)
	if forceDownload {
		url += "?download=true"
	}
	return url, nil
}

// GetAttachmentFile resolves metadata only; object IO and delivery happen outside the authorization lease.
func (s *Service) GetAttachmentFile(ctx context.Context, projectID, taskID, attachmentID uuid.UUID) (*attachmentdom.File, error) {
	if err := s.checkTask(ctx, projectID, taskID, taskdom.AccessRead); err != nil {
		return nil, err
	}
	a, err := s.repo.FindTaskAttachmentByID(ctx, attachmentID)
	if err != nil {
		return nil, err
	}
	if a.TaskID != taskID {
		return nil, attachmentdom.ErrAttachmentNotFound
	}
	f, err := s.repo.FindFileByID(ctx, a.FileID)
	if err != nil {
		return nil, err
	}
	if f.UploadStatus != attachmentdom.UploadStatusUploaded {
		return nil, attachmentdom.ErrFileNotFound
	}
	return f, nil
}

// GetAttachmentContent reads the attachment's bytes directly from the object
// store (bypassing the presigned-URL flow) and returns them along with the
// file's content type. See the interface doc for why this exists.
func (s *Service) GetAttachmentContent(ctx context.Context, projectID, taskID, attachmentID uuid.UUID) ([]byte, string, error) {
	if err := s.checkTask(ctx, projectID, taskID, taskdom.AccessRead); err != nil {
		return nil, "", err
	}
	a, err := s.repo.FindTaskAttachmentByID(ctx, attachmentID)
	if err != nil {
		return nil, "", err
	}
	if a.TaskID != taskID {
		return nil, "", attachmentdom.ErrAttachmentNotFound
	}

	f, err := s.repo.FindFileByID(ctx, a.FileID)
	if err != nil {
		return nil, "", err
	}
	if f.FileSize > attachmentdom.MaxAttachmentContentSize {
		return nil, "", attachmentdom.ErrAttachmentContentTooLarge
	}

	bucket := f.Bucket
	if bucket == "" {
		bucket = s.bucket
	}

	// f.FileSize is declared by the client at upload-initiation time, but the
	// presigned PUT enforces nothing about the actual object it accepts — a
	// client can declare a small file_size and upload a larger object (see
	// CompleteUpload above, which never re-verifies actual size). Bound the
	// read itself by capping one byte over the limit: if the object is
	// larger, GetObject's io.LimitReader truncates the read at
	// MaxAttachmentContentSize+1 bytes with no error, so the post-read
	// length check below is what actually catches it. Mirrors
	// avatar_service.go's identical guard against the same presigned-PUT gap.
	data, err := s.store.GetObject(ctx, bucket, f.StorageKey, attachmentdom.MaxAttachmentContentSize+1)
	if err != nil {
		return nil, "", fmt.Errorf("attachment svc: get object: %w", err)
	}
	if int64(len(data)) > attachmentdom.MaxAttachmentContentSize {
		return nil, "", attachmentdom.ErrAttachmentContentTooLarge
	}
	return data, f.ContentType, nil
}

// ListTaskAttachments returns all confirmed attachments for the given task.
func (s *Service) ListTaskAttachments(ctx context.Context, projectID, taskID uuid.UUID) ([]*attachmentdom.TaskAttachment, error) {
	if err := s.checkTask(ctx, projectID, taskID, taskdom.AccessRead); err != nil {
		return nil, err
	}
	return s.repo.ListTaskAttachments(ctx, taskID)
}

// DeleteTaskAttachment removes the task→file association only.
// The underlying file record and object-store object are intentionally kept
// so the file can be referenced by other tasks or restored later.
// Verifies the attachment belongs to taskID before deleting.
func (s *Service) DeleteTaskAttachment(ctx context.Context, projectID, taskID, attachmentID uuid.UUID) error {
	if err := s.checkTask(ctx, projectID, taskID, taskdom.AccessProgress); err != nil {
		return err
	}
	a, err := s.repo.FindTaskAttachmentByID(ctx, attachmentID)
	if err != nil {
		return err
	}
	if a.TaskID != taskID {
		return attachmentdom.ErrAttachmentNotFound
	}
	ctx = authz.WithMutationCheck(ctx, func(ctx context.Context) error { return s.checkTask(ctx, projectID, taskID, taskdom.AccessProgress) })
	return s.repo.DeleteTaskAttachment(ctx, attachmentID)
}

// sanitizeFileName strips directory components and replaces path-unsafe
// characters to produce a safe object-key segment.
func sanitizeFileName(name string) string {
	// Keep only the base name to prevent path traversal.
	name = path.Base(name)
	// Replace whitespace with underscores.
	name = strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' {
			return '_'
		}
		return r
	}, name)
	if name == "" || name == "." {
		name = "file"
	}
	return name
}

// --- Doc ownership checker --------------------------------------------------

type docOwnerChecker struct {
	repo docdom.DocumentRepository
}

// NewDocOwnerChecker returns a attachmentdom.DocOwnerChecker that validates
// document→project ownership via the document repository.
func NewDocOwnerChecker(repo docdom.DocumentRepository) attachmentdom.DocOwnerChecker {
	return &docOwnerChecker{repo: repo}
}

func (c *docOwnerChecker) DocBelongsToProject(ctx context.Context, projectID, docID uuid.UUID) error {
	d, err := c.repo.FindDocumentByID(ctx, docID)
	if err != nil {
		if errors.Is(err, docdom.ErrDocNotFound) {
			return attachmentdom.ErrDocNotInProject
		}
		return err
	}
	if d.ProjectID != projectID {
		return attachmentdom.ErrDocNotInProject
	}
	return nil
}

func (s *Service) checkTask(ctx context.Context, projectID, taskID uuid.UUID, action taskdom.AccessAction) error {
	if s.taskChecker == nil {
		return taskdom.ErrTaskNotFound
	}
	_, err := s.taskChecker.CheckTaskAccess(ctx, projectID, taskID, action)
	return err
}

// Pending uploads bind the namespace, object, file and authenticated uploader.
// Completed annotation screenshots use their trusted task association for reads.
func uploadMatches(f *attachmentdom.File, namespace string, objectID, fileID, actor uuid.UUID) bool {
	return objectID != uuid.Nil && actor != uuid.Nil && f.ID == fileID && f.UploadedBy != nil && *f.UploadedBy == actor && strings.HasPrefix(f.StorageKey, fmt.Sprintf("%s/%s/%s/", namespace, objectID, fileID))
}
