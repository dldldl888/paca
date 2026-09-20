package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/Paca-AI/api/internal/platform/delivery"
	"github.com/Paca-AI/api/internal/platform/storage"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	attachmentdom "github.com/Paca-AI/api/internal/domain/attachment"
	domainauth "github.com/Paca-AI/api/internal/domain/auth"
	"github.com/Paca-AI/api/internal/transport/http/handler"
	httpmw "github.com/Paca-AI/api/internal/transport/http/middleware"
)

// ---------------------------------------------------------------------------
// Minimal fakes
// ---------------------------------------------------------------------------

type fakeAttachmentSvc struct {
	contentData []byte
	contentType string
	contentErr  error
}

func (f *fakeAttachmentSvc) InitiateUpload(_ context.Context, _ uuid.UUID, _ attachmentdom.InitiateUploadInput) (*attachmentdom.UploadSession, error) {
	return &attachmentdom.UploadSession{FileID: uuid.New()}, nil
}
func (f *fakeAttachmentSvc) CompleteUpload(_ context.Context, _ uuid.UUID, _ attachmentdom.CompleteUploadInput) (*attachmentdom.TaskAttachment, error) {
	return &attachmentdom.TaskAttachment{}, nil
}
func (f *fakeAttachmentSvc) GetDownloadURL(_ context.Context, _, _, _ uuid.UUID, _ time.Duration, _ bool) (string, error) {
	return "https://example.com/download", nil
}
func (f *fakeAttachmentSvc) ListTaskAttachments(_ context.Context, _, _ uuid.UUID) ([]*attachmentdom.TaskAttachment, error) {
	return nil, nil
}
func (f *fakeAttachmentSvc) DeleteTaskAttachment(_ context.Context, _, _, _ uuid.UUID) error {
	return nil
}
func (f *fakeAttachmentSvc) GetAttachmentContent(_ context.Context, _, _, _ uuid.UUID) ([]byte, string, error) {
	if f.contentErr != nil {
		return nil, "", f.contentErr
	}
	return f.contentData, f.contentType, nil
}

var _ attachmentdom.Service = (*fakeAttachmentSvc)(nil)

// ---------------------------------------------------------------------------
// Router helper
// ---------------------------------------------------------------------------

// injectAuthClaims injects JWT claims for the attachment handler tests
// (InitiateUpload/CompleteUpload require claims.Subject for the uploader UUID).
func injectAuthClaimsMiddleware(sub string) func(http.Handler) http.Handler {
	claims := &domainauth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{Subject: sub},
		Kind:             "access",
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), httpmw.ClaimsContextKey(), claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func newAttachmentRouterWithAuth() chi.Router {
	h := handler.NewAttachmentHandler(&fakeAttachmentSvc{})
	r := chi.NewRouter()
	r.Use(injectAuthClaimsMiddleware(uuid.New().String()))
	r.Route("/projects/{projectId}/tasks/{taskId}/attachments", func(r chi.Router) {
		r.Post("/initiate-upload", h.InitiateUpload)
		r.Post("/complete-upload", h.CompleteUpload)
	})
	return r
}

func newAttachmentContentRouter(svc *fakeAttachmentSvc, gated ...bool) chi.Router {
	h := handler.NewAttachmentHandler(svc)
	if len(gated) == 0 || gated[0] {
		gate := delivery.NewGate()
		_ = gate.Update("1", false)
		h.WithFileDelivery(handler.NewFileDelivery(gate, handlerFileStore{data: svc.contentData}, "test", nil))
	}
	r := chi.NewRouter()
	r.Use(injectAuthClaimsMiddleware(uuid.New().String()))
	r.Route("/projects/{projectId}/tasks/{taskId}/attachments", func(r chi.Router) {
		r.Get("/{attachmentId}/content", h.GetAttachmentContent)
	})
	return r
}

func doAttachRequest(t *testing.T, r chi.Router, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf *bytes.Buffer
	if body != nil {
		b, _ := json.Marshal(body)
		buf = bytes.NewBuffer(b)
	} else {
		buf = bytes.NewBuffer(nil)
	}
	req := httptest.NewRequestWithContext(context.Background(), method, path, buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(deadlineRecorder{w}, req)
	return w
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestInitiateUpload_MissingFileName_Returns400(t *testing.T) {
	r := newAttachmentRouterWithAuth()
	projectID := uuid.New()
	taskID := uuid.New()
	path := "/projects/" + projectID.String() + "/tasks/" + taskID.String() + "/attachments/initiate-upload"

	w := doAttachRequest(t, r, http.MethodPost, path,
		map[string]any{"content_type": "image/png", "file_size": 1024})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing file_name, got %d: %s", w.Code, w.Body.String())
	}
}

func TestInitiateUpload_MissingContentType_Returns400(t *testing.T) {
	r := newAttachmentRouterWithAuth()
	projectID := uuid.New()
	taskID := uuid.New()
	path := "/projects/" + projectID.String() + "/tasks/" + taskID.String() + "/attachments/initiate-upload"

	w := doAttachRequest(t, r, http.MethodPost, path,
		map[string]any{"file_name": "photo.png", "content_type": "", "file_size": 1024})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing content_type, got %d: %s", w.Code, w.Body.String())
	}
}

func TestInitiateUpload_ZeroFileSize_Returns400(t *testing.T) {
	r := newAttachmentRouterWithAuth()
	projectID := uuid.New()
	taskID := uuid.New()
	path := "/projects/" + projectID.String() + "/tasks/" + taskID.String() + "/attachments/initiate-upload"

	w := doAttachRequest(t, r, http.MethodPost, path,
		map[string]any{"file_name": "photo.png", "content_type": "image/png", "file_size": 0})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for zero file_size, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCompleteUpload_MissingFileID_Returns400(t *testing.T) {
	r := newAttachmentRouterWithAuth()
	projectID := uuid.New()
	taskID := uuid.New()
	path := "/projects/" + projectID.String() + "/tasks/" + taskID.String() + "/attachments/complete-upload"

	// file_id absent → decodes to uuid.Nil → returns 400
	w := doAttachRequest(t, r, http.MethodPost, path, map[string]any{})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing file_id, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetAttachmentContent_ReturnsRawBytesWithContentType(t *testing.T) {
	svc := &fakeAttachmentSvc{contentData: []byte("hello world"), contentType: "text/plain"}
	r := newAttachmentContentRouter(svc)
	projectID := uuid.New()
	taskID := uuid.New()
	attachmentID := uuid.New()
	path := "/projects/" + projectID.String() + "/tasks/" + taskID.String() +
		"/attachments/" + attachmentID.String() + "/content"

	w := doAttachRequest(t, r, http.MethodGet, path, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "text/plain" {
		t.Fatalf("expected Content-Type text/plain, got %q", got)
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("expected X-Content-Type-Options nosniff, got %q", got)
	}
	if got := w.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "attachment") {
		t.Fatalf("expected Content-Disposition attachment, got %q", got)
	}
	if got := w.Body.String(); got != "hello world" {
		t.Fatalf("expected body %q, got %q", "hello world", got)
	}
}

func TestGetAttachmentContent_TooLarge_Returns413(t *testing.T) {
	svc := &fakeAttachmentSvc{contentErr: attachmentdom.ErrAttachmentContentTooLarge}
	r := newAttachmentContentRouter(svc)
	projectID := uuid.New()
	taskID := uuid.New()
	attachmentID := uuid.New()
	path := "/projects/" + projectID.String() + "/tasks/" + taskID.String() +
		"/attachments/" + attachmentID.String() + "/content"

	w := doAttachRequest(t, r, http.MethodGet, path, nil)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d: %s", w.Code, w.Body.String())
	}
}

// Document completion must bind the route document/project, never body IDs.
type docCompletionCapture struct {
	attachmentdom.DocFileService
	input attachmentdom.DocCompleteUploadInput
	calls int
}

func (s *docCompletionCapture) CompleteDocUpload(_ context.Context, in attachmentdom.DocCompleteUploadInput) (*attachmentdom.File, error) {
	s.calls++
	s.input = in
	return &attachmentdom.File{ID: in.FileID}, nil
}
func TestDocCompleteUploadBindsRoute(t *testing.T) {
	svc := &docCompletionCapture{}
	r := chi.NewRouter()
	r.Post("/projects/{projectId}/docs/{docId}/files/complete-upload", handler.NewDocFileHandler(svc).CompleteDocUpload)
	project, doc, file := uuid.New(), uuid.New(), uuid.New()
	path := "/projects/" + project.String() + "/docs/" + doc.String() + "/files/complete-upload"
	result := doAttachRequest(t, r, http.MethodPost, path, map[string]any{"file_id": file, "doc_id": uuid.New(), "project_id": uuid.New()})
	if result.Code != http.StatusCreated || svc.calls != 1 || svc.input.ProjectID != project || svc.input.DocID != doc || svc.input.FileID != file {
		t.Fatalf("route binding lost: status=%d input=%+v", result.Code, svc.input)
	}
	result = doAttachRequest(t, r, http.MethodPost, "/projects/invalid/docs/"+doc.String()+"/files/complete-upload", map[string]any{"file_id": file})
	if result.Code != http.StatusBadRequest || svc.calls != 1 {
		t.Fatal("invalid route reached completion")
	}
}

func TestAttachmentContentFailsWithoutDeliveryGate(t *testing.T) {
	r := newAttachmentContentRouter(&fakeAttachmentSvc{contentData: []byte("secret"), contentType: "text/plain"}, false)
	w := doAttachRequest(t, r, "GET", "/projects/"+uuid.NewString()+"/tasks/"+uuid.NewString()+"/attachments/"+uuid.NewString()+"/content", nil)
	if w.Code != 503 {
		t.Fatalf("missing gate must fail closed: status=%d body=%s", w.Code, w.Body.String())
	}
}

func (f *fakeAttachmentSvc) GetAttachmentFile(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*attachmentdom.File, error) {
	return &attachmentdom.File{FileName: "fixture", ContentType: f.contentType}, f.contentErr
}

type handlerFileStore struct {
	storage.Client
	data []byte
}

func (s handlerFileStore) OpenObject(context.Context, string, string, string) (*storage.Object, error) {
	return &storage.Object{Body: io.NopCloser(bytes.NewReader(s.data)), Size: int64(len(s.data))}, nil
}

type deadlineRecorder struct{ *httptest.ResponseRecorder }

func (deadlineRecorder) SetWriteDeadline(time.Time) error { return nil }
