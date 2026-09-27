package attachmentsvc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	attachmentdom "github.com/Paca-AI/api/internal/domain/attachment"
	docdom "github.com/Paca-AI/api/internal/domain/doc"
)

// fakeDocRepoForChecker is a minimal docdom.DocumentRepository backing
// NewDocAccessChecker in tests. Only FindDocumentByID is exercised by the
// checker; the remaining methods are unused stubs.
type fakeDocRepoForChecker struct {
	docs map[uuid.UUID]*docdom.Document
}

func (r *fakeDocRepoForChecker) ListDocuments(context.Context, uuid.UUID, *uuid.UUID, *string, *string, *int, *uuid.UUID) ([]*docdom.Document, bool, error) {
	return nil, false, nil
}
func (r *fakeDocRepoForChecker) FindDocumentByID(_ context.Context, id uuid.UUID) (*docdom.Document, error) {
	d, ok := r.docs[id]
	if !ok {
		return nil, docdom.ErrDocNotFound
	}
	return d, nil
}
func (r *fakeDocRepoForChecker) CreateDocument(context.Context, *docdom.Document) error { return nil }
func (r *fakeDocRepoForChecker) UpdateDocument(context.Context, *docdom.Document) error { return nil }
func (r *fakeDocRepoForChecker) DeleteDocument(context.Context, uuid.UUID) error        { return nil }

var _ docdom.DocumentRepository = (*fakeDocRepoForChecker)(nil)

// ---------------------------------------------------------------------------
// docAccessChecker (GHSA-xwmv-9c7h-g947 / PACA-003)
// ---------------------------------------------------------------------------

func TestDocAccessChecker_CurrentObjectPolicy(t *testing.T) {
	projectID := uuid.New()
	docID := uuid.New()
	docRepo := &fakeDocRepoForChecker{docs: map[uuid.UUID]*docdom.Document{
		docID: {ID: docID, ProjectID: projectID},
	}}
	checker := NewDocAccessChecker(docRepo)

	if _, err := checker.CheckDocumentAccess(context.Background(), projectID, docID, docdom.AccessRead); err != nil {
		t.Fatalf("expected nil for matching project, got %v", err)
	}

	if _, err := checker.CheckDocumentAccess(context.Background(), uuid.New(), docID, docdom.AccessRead); !errors.Is(err, attachmentdom.ErrDocNotInProject) {
		t.Fatalf("expected ErrDocNotInProject for mismatched project, got %v", err)
	}

	if _, err := checker.CheckDocumentAccess(context.Background(), projectID, uuid.New(), docdom.AccessRead); !errors.Is(err, attachmentdom.ErrDocNotInProject) {
		t.Fatalf("expected ErrDocNotInProject for a nonexistent document, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Service.GetDocFileDownloadURL / Service.DeleteDocFile
// ---------------------------------------------------------------------------

// newDocFileTestService wires a Service around a doc file whose storage key
// is scoped under docID, plus a real docAccessChecker backed by docRepo — so
// tests exercise the full project-scoping chain end to end, not just the
// pre-existing storage-key-prefix check.
func newDocFileTestService(docRepo *fakeDocRepoForChecker) (svc *Service, fileID, docID uuid.UUID) {
	docID = uuid.New()
	fileID = uuid.New()
	repo := &fakeRepo{
		files: map[uuid.UUID]*attachmentdom.File{
			fileID: {
				ID:           fileID,
				StorageKey:   "docs/" + docID.String() + "/" + fileID.String() + "/file.txt",
				Bucket:       "test-bucket",
				FileName:     "file.txt",
				ContentType:  "text/plain",
				FileSize:     5,
				UploadStatus: attachmentdom.UploadStatusUploaded,
			},
		},
		attachments: map[uuid.UUID]*attachmentdom.TaskAttachment{},
	}
	store := &fakeStore{objectBytes: []byte("hello")}
	svc = New(repo, fakeTaskChecker{}, NewDocAccessChecker(docRepo), store, "test-bucket")
	return svc, fileID, docID
}

func TestGetDocFileDownloadURL_WrongProject_ReturnsNotFound(t *testing.T) {
	ownerProjectID := uuid.New()
	attackerProjectID := uuid.New()
	docRepo := &fakeDocRepoForChecker{docs: map[uuid.UUID]*docdom.Document{}}
	svc, fileID, docID := newDocFileTestService(docRepo)
	docRepo.docs[docID] = &docdom.Document{ID: docID, ProjectID: ownerProjectID}

	_, err := svc.GetDocFileDownloadURL(context.Background(), attackerProjectID, docID, fileID, 15*time.Minute)
	if !errors.Is(err, attachmentdom.ErrDocNotInProject) {
		t.Fatalf("expected ErrDocNotInProject for cross-project download, got %v", err)
	}
}

func TestGetDocFileDownloadURL_OwnProject_PassesOwnershipCheck(t *testing.T) {
	ownerProjectID := uuid.New()
	docRepo := &fakeDocRepoForChecker{docs: map[uuid.UUID]*docdom.Document{}}
	svc, fileID, docID := newDocFileTestService(docRepo)
	docRepo.docs[docID] = &docdom.Document{ID: docID, ProjectID: ownerProjectID}

	url, err := svc.GetDocFileDownloadURL(context.Background(), ownerProjectID, docID, fileID, time.Minute)
	if err != nil || !strings.HasSuffix(url, "/content") {
		t.Fatalf("same-project API URL: %q %v", url, err)
	}
}

func TestDeleteDocFile_WrongProject_ReturnsNotFound(t *testing.T) {
	ownerProjectID := uuid.New()
	attackerProjectID := uuid.New()
	docRepo := &fakeDocRepoForChecker{docs: map[uuid.UUID]*docdom.Document{}}
	svc, fileID, docID := newDocFileTestService(docRepo)
	docRepo.docs[docID] = &docdom.Document{ID: docID, ProjectID: ownerProjectID}

	err := svc.DeleteDocFile(context.Background(), attackerProjectID, docID, fileID)
	if !errors.Is(err, attachmentdom.ErrDocNotInProject) {
		t.Fatalf("expected ErrDocNotInProject for cross-project delete, got %v", err)
	}

	if _, findErr := svc.repo.FindFileByID(context.Background(), fileID); findErr != nil {
		t.Errorf("file must not be deleted by a rejected cross-project request: %v", findErr)
	}
}

func TestDeleteDocFile_OwnProject_PassesOwnershipCheck(t *testing.T) {
	ownerProjectID := uuid.New()
	docRepo := &fakeDocRepoForChecker{docs: map[uuid.UUID]*docdom.Document{}}
	svc, fileID, docID := newDocFileTestService(docRepo)
	docRepo.docs[docID] = &docdom.Document{ID: docID, ProjectID: ownerProjectID}

	// fakeStore.DeleteObject is an unimplemented stub, so this can't
	// actually succeed — the point is that the request gets past the
	// project-ownership gate instead of being rejected by it.
	err := svc.DeleteDocFile(context.Background(), ownerProjectID, docID, fileID)
	if errors.Is(err, attachmentdom.ErrDocNotInProject) {
		t.Fatalf("same-project delete must not be rejected by the ownership check, got %v", err)
	}
}

// Test fixture's explicit legacy object policy; real scopes are exercised with Postgres.
func (r *fakeDocRepoForChecker) CheckDocumentAccess(ctx context.Context, project, id uuid.UUID, _ docdom.AccessAction) (*docdom.Document, error) {
	d, err := r.FindDocumentByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if project != uuid.Nil && d.ProjectID != project {
		return nil, docdom.ErrDocNotFound
	}
	return d, nil
}
