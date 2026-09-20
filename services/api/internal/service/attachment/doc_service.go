package attachmentsvc

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	attachmentdom "github.com/Paca-AI/api/internal/domain/attachment"
	"github.com/Paca-AI/api/internal/platform/authz"
	"github.com/Paca-AI/api/internal/platform/storage"
)

// InitiateDocUpload creates a pending File record for a document and returns a
// presigned upload session.  The storage key is organised under docs/{docId}/
// to keep document files separate from task files.
func (s *Service) InitiateDocUpload(ctx context.Context, in attachmentdom.DocUploadInput) (*attachmentdom.UploadSession, error) {
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
	safeFileName := sanitizeFileName(in.FileName)
	storageKey := fmt.Sprintf("docs/%s/%s/%s", in.DocID.String(), fileID.String(), safeFileName)

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
		return nil, fmt.Errorf("attachment svc: create doc file: %w", err)
	}

	session := &attachmentdom.UploadSession{FileID: fileID}

	if in.FileSize >= storage.MultipartThreshold {
		mu, err := s.store.InitiateMultipartUpload(ctx, s.bucket, storageKey, in.ContentType, in.FileSize, storage.DefaultPartSize, presignedUploadTTL)
		if err != nil {
			return nil, fmt.Errorf("attachment svc: initiate multipart for doc: %w", err)
		}
		if err := s.repo.UpdateFileStatus(ctx, fileID, attachmentdom.UploadStatusPending, &mu.UploadID); err != nil {
			return nil, fmt.Errorf("attachment svc: save upload id for doc: %w", err)
		}
		session.IsMultipart = true
		session.Multipart = mu
	} else {
		uploadURL, err := s.store.PresignPutObject(ctx, s.bucket, storageKey, in.ContentType, presignedUploadTTL)
		if err != nil {
			return nil, fmt.Errorf("attachment svc: presign put for doc: %w", err)
		}
		session.UploadURL = uploadURL
	}

	return session, nil
}

// CompleteDocUpload marks the file as uploaded and returns the updated file record.
func (s *Service) CompleteDocUpload(ctx context.Context, in attachmentdom.DocCompleteUploadInput) (*attachmentdom.File, error) {
	if s.docChecker == nil {
		return nil, attachmentdom.ErrDocNotInProject
	}
	if err := s.docChecker.DocBelongsToProject(ctx, in.ProjectID, in.DocID); err != nil {
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
	if !uploadMatches(f, "docs", in.DocID, in.FileID, actor) {
		return nil, attachmentdom.ErrFileNotFound
	}
	if f.UploadStatus != attachmentdom.UploadStatusPending {
		return nil, attachmentdom.ErrUploadNotPending
	}

	switch {
	case f.MultipartUploadID != nil && in.UploadID == nil:
		return nil, attachmentdom.ErrMultipartUploadIDRequired
	case f.MultipartUploadID == nil && in.UploadID != nil:
		return nil, attachmentdom.ErrNotMultipartUpload
	case f.MultipartUploadID != nil && in.UploadID != nil && *in.UploadID != *f.MultipartUploadID:
		return nil, attachmentdom.ErrUploadIDMismatch
	case f.MultipartUploadID != nil && in.UploadID != nil && len(in.Parts) == 0:
		return nil, attachmentdom.ErrMultipartPartsEmpty
	}

	if in.UploadID != nil {
		parts := make([]storage.CompletedPart, 0, len(in.Parts))
		for _, p := range in.Parts {
			parts = append(parts, storage.CompletedPart{
				PartNumber: p.PartNumber,
				ETag:       p.ETag,
			})
		}
		if err := s.store.CompleteMultipartUpload(ctx, s.bucket, f.StorageKey, *in.UploadID, parts); err != nil {
			return nil, fmt.Errorf("attachment svc: complete multipart for doc: %w", err)
		}
	}

	if err := s.repo.UpdateFileStatus(ctx, in.FileID, attachmentdom.UploadStatusUploaded, nil); err != nil {
		return nil, fmt.Errorf("attachment svc: update doc file status: %w", err)
	}

	f.UploadStatus = attachmentdom.UploadStatusUploaded
	return f, nil
}

// GetDocFileDownloadURL preserves the URL contract without granting bearer storage access.
func (s *Service) GetDocFileDownloadURL(ctx context.Context, projectID, docID, fileID uuid.UUID, ttl time.Duration) (string, error) {
	if _, err := s.GetDocFile(ctx, projectID, docID, fileID); err != nil {
		return "", err
	}
	return fmt.Sprintf("/api/v1/projects/%s/docs/%s/files/%s/content", projectID, docID, fileID), nil
}

func (s *Service) GetDocFile(ctx context.Context, projectID, docID, fileID uuid.UUID) (*attachmentdom.File, error) {
	if s.docChecker == nil {
		return nil, attachmentdom.ErrDocNotInProject
	}
	if err := s.docChecker.DocBelongsToProject(ctx, projectID, docID); err != nil {
		return nil, err
	}
	f, err := s.repo.FindFileByID(ctx, fileID)
	if err != nil {
		return nil, err
	}
	if !docFileKeyHasDocPrefix(f.StorageKey, docID) {
		return nil, attachmentdom.ErrDocFileMismatch
	}
	if f.UploadStatus != attachmentdom.UploadStatusUploaded {
		return nil, attachmentdom.ErrFileNotFound
	}
	return f, nil
}

// DeleteDocFile removes the file record from the database and deletes the
// object from the object store. docID is used to verify the file belongs to
// the document by checking the storage key prefix (docs/{docId}/), and
// projectID is used to verify docID itself belongs to the caller's
// authorized project.
func (s *Service) DeleteDocFile(ctx context.Context, projectID, docID, fileID uuid.UUID) error {
	if err := s.docChecker.DocBelongsToProject(ctx, projectID, docID); err != nil {
		return err
	}

	f, err := s.repo.FindFileByID(ctx, fileID)
	if err != nil {
		return err
	}

	if !docFileKeyHasDocPrefix(f.StorageKey, docID) {
		return attachmentdom.ErrDocFileMismatch
	}

	bucket := f.Bucket
	if bucket == "" {
		bucket = s.bucket
	}

	// ponytail: failed also marks revoked files awaiting cleanup; split lifecycle states if automated cleanup is added.
	ctx = authz.WithMutationCheck(ctx, func(ctx context.Context) error { return s.docChecker.DocBelongsToProject(ctx, projectID, docID) })
	if err := s.repo.UpdateFileStatus(ctx, fileID, attachmentdom.UploadStatusFailed, nil); err != nil {
		return err
	}
	// Logical removal is now committed. Slow physical cleanup holds no lease or transaction.
	if err := s.store.DeleteObject(ctx, bucket, f.StorageKey); err != nil {
		return fmt.Errorf("attachment svc: file revoked; physical cleanup failed, retry deletion: %w", err)
	}

	return s.repo.DeleteFile(ctx, fileID)
}

// docFileKeyHasDocPrefix returns true when storageKey starts with
// the expected "docs/{docID}/" prefix, confirming the file belongs to the document.
func docFileKeyHasDocPrefix(storageKey string, docID uuid.UUID) bool {
	return strings.HasPrefix(storageKey, fmt.Sprintf("docs/%s/", docID.String()))
}
