package attachmentsvc

import (
	"context"
	"errors"
	attachmentdom "github.com/Paca-AI/api/internal/domain/attachment"
	"github.com/google/uuid"
	"strings"
	"testing"
	"time"
)

func TestDownloadURLRequiresAuthenticatedDelivery(t *testing.T) {
	svc, tid, aid := newTestService([]byte("hello"), 5)
	for _, f := range svc.repo.(*fakeRepo).files {
		f.UploadStatus = attachmentdom.UploadStatusUploaded
	}
	pid := uuid.New()
	got, err := svc.GetDownloadURL(context.Background(), pid, tid, aid, time.Minute, true)
	want := "/api/v1/projects/" + pid.String() + "/tasks/" + tid.String() + "/attachments/" + aid.String() + "/download?download=true"
	if err != nil || got != want {
		t.Fatalf("authenticated download URL = %q, %v; want %q", got, err, want)
	}
	for _, f := range svc.repo.(*fakeRepo).files {
		f.StorageKey = "docs/" + tid.String() + "/" + f.ID.String() + "/file.txt"
		got, err = svc.GetDocFileDownloadURL(context.Background(), pid, tid, f.ID, time.Minute)
		if err != nil || !strings.HasPrefix(got, "/api/v1/projects/") || !strings.HasSuffix(got, "/content") {
			t.Fatalf("authenticated doc URL = %q, %v", got, err)
		}
	}
}

type cleanupRepo struct{ *fakeRepo }

func (r cleanupRepo) UpdateFileStatus(_ context.Context, id uuid.UUID, status attachmentdom.UploadStatus, _ *string) error {
	r.files[id].UploadStatus = status
	return nil
}
func (r cleanupRepo) DeleteFile(_ context.Context, id uuid.UUID) error {
	delete(r.files, id)
	return nil
}

type cleanupStore struct {
	*fakeStore
	fail bool
	file *attachmentdom.File
	t    *testing.T
}

func (s *cleanupStore) DeleteObject(context.Context, string, string) error {
	if s.file.UploadStatus != attachmentdom.UploadStatusFailed {
		s.t.Error("storage deletion preceded durable revocation")
	}
	if s.fail {
		return errors.New("injected cleanup outage")
	}
	return nil
}
func TestDeleteDocFileRevokesBeforeCleanupAndRetries(t *testing.T) {
	doc, file := uuid.New(), uuid.New()
	f := &attachmentdom.File{ID: file, StorageKey: "docs/" + doc.String() + "/" + file.String() + "/file", UploadStatus: attachmentdom.UploadStatusUploaded}
	repo := cleanupRepo{&fakeRepo{files: map[uuid.UUID]*attachmentdom.File{file: f}}}
	store := &cleanupStore{fakeStore: &fakeStore{}, fail: true, file: f, t: t}
	svc := New(repo, nil, fakeDocChecker{}, store, "test")
	if err := svc.DeleteDocFile(context.Background(), uuid.New(), doc, file); err == nil {
		t.Fatal("cleanup failure was hidden")
	}
	if f.UploadStatus != attachmentdom.UploadStatusFailed || repo.files[file] == nil {
		t.Fatal("cleanup failure lost durable retry metadata or left file readable")
	}
	if _, err := svc.GetDocFile(context.Background(), uuid.New(), doc, file); !errors.Is(err, attachmentdom.ErrFileNotFound) {
		t.Fatal("failed cleanup file still downloadable", err)
	}
	store.fail = false
	if err := svc.DeleteDocFile(context.Background(), uuid.New(), doc, file); err != nil {
		t.Fatal(err)
	}
	if repo.files[file] != nil {
		t.Fatal("successful retry retained file record")
	}
}
