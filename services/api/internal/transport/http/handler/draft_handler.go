package handler

import (
	"errors"
	"github.com/Paca-AI/api/internal/apierr"
	draftdom "github.com/Paca-AI/api/internal/domain/draft"
	"github.com/Paca-AI/api/internal/platform/delivery"
	pg "github.com/Paca-AI/api/internal/repository/postgres"
	draftsvc "github.com/Paca-AI/api/internal/service/draft"
	"github.com/Paca-AI/api/internal/transport/http/presenter"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"io"
	"net/http"
	"strconv"
	"strings"
)

type DraftHandler struct {
	svc  *draftsvc.Service
	gate *delivery.Gate
}

func NewDraftHandler(svc *draftsvc.Service) *DraftHandler { return &DraftHandler{svc: svc} }
func (h *DraftHandler) WithDeliveryGate(gate *delivery.Gate) *DraftHandler {
	h.gate = gate
	return h
}
func draftIDs(r *http.Request) (uuid.UUID, uuid.UUID, error) {
	pid, e := uuid.Parse(chi.URLParam(r, "projectId"))
	if e != nil {
		return uuid.Nil, uuid.Nil, apierr.New(apierr.CodeBadRequest, "invalid project id")
	}
	id := uuid.Nil
	if raw := chi.URLParam(r, "draftId"); raw != "" {
		id, e = uuid.Parse(raw)
		if e != nil {
			return pid, id, apierr.New(apierr.CodeBadRequest, "invalid draft id")
		}
	}
	return pid, id, nil
}
func draftBody(w http.ResponseWriter, r *http.Request, out any) error {
	b, e := io.ReadAll(http.MaxBytesReader(w, r.Body, draftsvc.MaxBody))
	if e != nil {
		return apierr.New(apierr.CodeBadRequest, "body exceeds size limit")
	}
	if e = draftsvc.DecodeStrict(b, out); e != nil {
		return apierr.New(apierr.CodeBadRequest, "invalid draft JSON: "+e.Error())
	}
	return nil
}
func draftError(w http.ResponseWriter, r *http.Request, e error) {
	if errors.Is(e, draftsvc.ErrPublicWebSearchUnavailable) {
		e = apierr.New(apierr.CodeServiceUnavailable, "public web search is unavailable")
	}
	if errors.Is(e, draftsvc.ErrPublicWebSearchQueryInvalid) {
		e = apierr.New(apierr.CodeBadRequest, "public web search query is invalid")
	}
	if errors.Is(e, delivery.ErrBlocked) {
		e = draftdom.ErrPending
	}
	var validation *pg.DraftValidationError
	if errors.As(e, &validation) {
		e = apierr.New(apierr.CodeBadRequest, "draft has validation issues; refresh preview")
	}
	var generation *draftdom.ProjectDraftGenerationError
	if errors.As(e, &generation) {
		e = apierr.New(apierr.CodeBadRequest, generation.Error())
	}
	presenter.Error(w, r, e)
}
func (h *DraftHandler) List(w http.ResponseWriter, r *http.Request) {
	pid, _, e := draftIDs(r)
	if e != nil {
		draftError(w, r, e)
		return
	}
	r, leave := enterJSONDelivery(h.gate, w, r)
	if r == nil {
		return
	}
	defer leave()
	items, e := h.svc.List(r.Context(), pid)
	if e != nil {
		draftError(w, r, e)
		return
	}
	presenter.OK(w, r, map[string]any{"items": items})
}
func (h *DraftHandler) Create(w http.ResponseWriter, r *http.Request) {
	pid, _, e := draftIDs(r)
	if e != nil {
		draftError(w, r, e)
		return
	}
	var c draftdom.Content
	if e = draftBody(w, r, &c); e != nil {
		draftError(w, r, e)
		return
	}
	if e = c.ValidateStructure(); e != nil {
		draftError(w, r, apierr.New(apierr.CodeBadRequest, e.Error()))
		return
	}
	r, leave := enterJSONDelivery(h.gate, w, r)
	if r == nil {
		return
	}
	defer leave()
	d, e := h.svc.Create(r.Context(), pid, c)
	if e != nil {
		draftError(w, r, e)
		return
	}
	presenter.Created(w, r, d)
}
func (h *DraftHandler) Get(w http.ResponseWriter, r *http.Request) {
	pid, id, e := draftIDs(r)
	if e != nil {
		draftError(w, r, e)
		return
	}
	r, leave := enterJSONDelivery(h.gate, w, r)
	if r == nil {
		return
	}
	defer leave()
	d, e := h.svc.Get(r.Context(), pid, id)
	if e != nil {
		draftError(w, r, e)
		return
	}
	presenter.OK(w, r, d)
}
func (h *DraftHandler) Update(w http.ResponseWriter, r *http.Request) {
	pid, id, e := draftIDs(r)
	if e != nil {
		draftError(w, r, e)
		return
	}
	var in struct {
		draftdom.Content
		ExpectedVersion int `json:"expected_version"`
	}
	if e = draftBody(w, r, &in); e != nil {
		draftError(w, r, e)
		return
	}
	if e = in.Content.ValidateStructure(); e != nil {
		draftError(w, r, apierr.New(apierr.CodeBadRequest, e.Error()))
		return
	}
	r, leave := enterJSONDelivery(h.gate, w, r)
	if r == nil {
		return
	}
	defer leave()
	d, e := h.svc.Update(r.Context(), pid, id, in.ExpectedVersion, in.Content)
	if e != nil {
		draftError(w, r, e)
		return
	}
	presenter.OK(w, r, d)
}
func (h *DraftHandler) Preview(w http.ResponseWriter, r *http.Request) {
	pid, id, e := draftIDs(r)
	if e != nil {
		draftError(w, r, e)
		return
	}
	var in struct {
		ExpectedVersion int `json:"expected_version"`
	}
	if e = draftBody(w, r, &in); e != nil {
		draftError(w, r, e)
		return
	}
	r, leave := enterJSONDelivery(h.gate, w, r)
	if r == nil {
		return
	}
	defer leave()
	p, e := h.svc.Preview(r.Context(), pid, id, in.ExpectedVersion)
	if e != nil {
		draftError(w, r, e)
		return
	}
	presenter.OK(w, r, p)
}
func (h *DraftHandler) Publish(w http.ResponseWriter, r *http.Request) {
	pid, id, e := draftIDs(r)
	if e != nil {
		draftError(w, r, e)
		return
	}
	var in struct {
		ExpectedVersion int       `json:"expected_version"`
		PublicationID   uuid.UUID `json:"publication_id"`
		Confirmed       bool      `json:"confirmed"`
	}
	if e = draftBody(w, r, &in); e != nil {
		draftError(w, r, e)
		return
	}
	if !in.Confirmed || in.PublicationID == uuid.Nil {
		draftError(w, r, apierr.New(apierr.CodeBadRequest, "explicit confirmation and publication UUID required"))
		return
	}
	r, leave := enterJSONDelivery(h.gate, w, r)
	if r == nil {
		return
	}
	defer leave()
	d, e := h.svc.Publish(r.Context(), pid, id, in.ExpectedVersion, in.PublicationID)
	if e != nil {
		draftError(w, r, e)
		return
	}
	presenter.OK(w, r, d)
}
func (h *DraftHandler) Generate(w http.ResponseWriter, r *http.Request) {
	pid, id, e := draftIDs(r)
	if e != nil {
		draftError(w, r, e)
		return
	}
	var in struct {
		ExpectedVersion int       `json:"expected_version"`
		AgentID         uuid.UUID `json:"agent_id"`
	}
	if e = draftBody(w, r, &in); e != nil {
		draftError(w, r, e)
		return
	}
	r, leave := enterJSONDelivery(h.gate, w, r)
	if r == nil {
		return
	}
	defer leave()
	d, e := h.svc.Generate(r.Context(), pid, id, in.ExpectedVersion, in.AgentID)
	if e != nil {
		draftError(w, r, e)
		return
	}
	presenter.OK(w, r, d)
}
func (h *DraftHandler) Import(w http.ResponseWriter, r *http.Request) {
	pid, id, e := draftIDs(r)
	if e != nil {
		draftError(w, r, e)
		return
	}
	var in struct {
		ExpectedVersion int       `json:"expected_version"`
		GenerationID    uuid.UUID `json:"generation_id"`
	}
	if e = draftBody(w, r, &in); e != nil {
		draftError(w, r, e)
		return
	}
	r, leave := enterJSONDelivery(h.gate, w, r)
	if r == nil {
		return
	}
	defer leave()
	d, e := h.svc.Import(r.Context(), pid, id, in.ExpectedVersion, in.GenerationID)
	if e != nil {
		draftError(w, r, e)
		return
	}
	presenter.OK(w, r, d)
}

func projectDraftID(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "draftId"))
	if err != nil {
		return uuid.Nil, apierr.New(apierr.CodeBadRequest, "invalid project draft id")
	}
	return id, nil
}

func (h *DraftHandler) ProjectList(w http.ResponseWriter, r *http.Request) {
	r, leave := enterJSONDelivery(h.gate, w, r)
	if r == nil {
		return
	}
	defer leave()
	items, err := h.svc.ListProjectDrafts(r.Context())
	if err != nil {
		draftError(w, r, err)
		return
	}
	presenter.OK(w, r, map[string]any{"items": items})
}

func (h *DraftHandler) ProjectMaterials(w http.ResponseWriter, r *http.Request) {
	r, leave := enterJSONDelivery(h.gate, w, r)
	if r == nil {
		return
	}
	defer leave()
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 50 {
			draftError(w, r, apierr.New(apierr.CodeBadRequest, "limit must be an integer between 1 and 50"))
			return
		}
		limit = parsed
	}
	items, err := h.svc.SearchProjectDraftMaterials(r.Context(), strings.TrimSpace(r.URL.Query().Get("search")), limit)
	if err != nil {
		draftError(w, r, err)
		return
	}
	presenter.OK(w, r, map[string]any{"items": items, "coverage": map[string]any{"scope": "accessible_projects", "limit": limit}})
}

func (h *DraftHandler) ProjectWebSearch(w http.ResponseWriter, r *http.Request) {
	r, leave := enterJSONDelivery(h.gate, w, r)
	if r == nil {
		return
	}
	defer leave()
	limit := 10
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 50 {
			draftError(w, r, apierr.New(apierr.CodeBadRequest, "limit must be an integer between 1 and 50"))
			return
		}
		limit = parsed
	}
	items, err := h.svc.SearchProjectDraftWeb(r.Context(), strings.TrimSpace(r.URL.Query().Get("q")), limit)
	if err != nil {
		draftError(w, r, err)
		return
	}
	presenter.OK(w, r, map[string]any{"items": items, "coverage": map[string]any{"source": "public", "limit": limit}})
}

func (h *DraftHandler) ProjectPreview(w http.ResponseWriter, r *http.Request) {
	id, err := projectDraftID(r)
	if err != nil {
		draftError(w, r, err)
		return
	}
	var in struct {
		ExpectedVersion int `json:"expected_version"`
	}
	if err = draftBody(w, r, &in); err != nil {
		draftError(w, r, err)
		return
	}
	if in.ExpectedVersion < 1 {
		draftError(w, r, apierr.New(apierr.CodeBadRequest, "expected_version must be positive"))
		return
	}
	r, leave := enterJSONDelivery(h.gate, w, r)
	if r == nil {
		return
	}
	defer leave()
	preview, err := h.svc.PreviewProjectDraft(r.Context(), id, in.ExpectedVersion)
	if err != nil {
		draftError(w, r, err)
		return
	}
	presenter.OK(w, r, preview)
}

func (h *DraftHandler) ProjectPublish(w http.ResponseWriter, r *http.Request) {
	id, err := projectDraftID(r)
	if err != nil {
		draftError(w, r, err)
		return
	}
	var in struct {
		ExpectedVersion int       `json:"expected_version"`
		PublicationID   uuid.UUID `json:"publication_id"`
	}
	if err = draftBody(w, r, &in); err != nil {
		draftError(w, r, err)
		return
	}
	if in.ExpectedVersion < 1 || in.PublicationID == uuid.Nil {
		draftError(w, r, apierr.New(apierr.CodeBadRequest, "expected_version and publication_id are required"))
		return
	}
	r, leave := enterJSONDelivery(h.gate, w, r)
	if r == nil {
		return
	}
	defer leave()
	publication, err := h.svc.PublishProjectDraft(r.Context(), id, in.ExpectedVersion, in.PublicationID)
	if err != nil {
		draftError(w, r, err)
		return
	}
	presenter.OK(w, r, publication)
}

func (h *DraftHandler) ProjectFileList(w http.ResponseWriter, r *http.Request) {
	id, err := projectDraftID(r)
	if err != nil {
		draftError(w, r, err)
		return
	}
	r, leave := enterJSONDelivery(h.gate, w, r)
	if r == nil {
		return
	}
	defer leave()
	items, err := h.svc.ListProjectDraftFiles(r.Context(), id)
	if err != nil {
		draftError(w, r, err)
		return
	}
	presenter.OK(w, r, map[string]any{"items": items})
}

func (h *DraftHandler) ProjectFileInitiate(w http.ResponseWriter, r *http.Request) {
	id, err := projectDraftID(r)
	if err != nil {
		draftError(w, r, err)
		return
	}
	var in struct {
		FileName    string `json:"file_name"`
		ContentType string `json:"content_type"`
		FileSize    int64  `json:"file_size"`
	}
	if err = draftBody(w, r, &in); err != nil {
		draftError(w, r, err)
		return
	}
	r, leave := enterJSONDelivery(h.gate, w, r)
	if r == nil {
		return
	}
	defer leave()
	session, err := h.svc.InitiateProjectDraftFile(r.Context(), id, in.FileName, in.ContentType, in.FileSize)
	if err != nil {
		draftError(w, r, err)
		return
	}
	presenter.Created(w, r, session)
}

func (h *DraftHandler) ProjectFileComplete(w http.ResponseWriter, r *http.Request) {
	id, err := projectDraftID(r)
	if err != nil {
		draftError(w, r, err)
		return
	}
	var in struct {
		FileID uuid.UUID `json:"file_id"`
	}
	if err = draftBody(w, r, &in); err != nil {
		draftError(w, r, err)
		return
	}
	if in.FileID == uuid.Nil {
		draftError(w, r, apierr.New(apierr.CodeBadRequest, "file_id is required"))
		return
	}
	r, leave := enterJSONDelivery(h.gate, w, r)
	if r == nil {
		return
	}
	defer leave()
	file, err := h.svc.CompleteProjectDraftFile(r.Context(), id, in.FileID)
	if err != nil {
		draftError(w, r, err)
		return
	}
	presenter.OK(w, r, file)
}

func (h *DraftHandler) ProjectFileRetry(w http.ResponseWriter, r *http.Request) {
	id, err := projectDraftID(r)
	if err != nil {
		draftError(w, r, err)
		return
	}
	var in struct {
		FileID uuid.UUID `json:"file_id"`
	}
	if err = draftBody(w, r, &in); err != nil {
		draftError(w, r, err)
		return
	}
	if in.FileID == uuid.Nil {
		draftError(w, r, apierr.New(apierr.CodeBadRequest, "file_id is required"))
		return
	}
	r, leave := enterJSONDelivery(h.gate, w, r)
	if r == nil {
		return
	}
	defer leave()
	session, err := h.svc.RetryProjectDraftFile(r.Context(), id, in.FileID)
	if err != nil {
		draftError(w, r, err)
		return
	}
	presenter.OK(w, r, session)
}

func (h *DraftHandler) ProjectCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		draftdom.ProjectDraftContent
		ChatSessionID uuid.UUID `json:"chat_session_id"`
	}
	if err := draftBody(w, r, &in); err != nil {
		draftError(w, r, err)
		return
	}
	r, leave := enterJSONDelivery(h.gate, w, r)
	if r == nil {
		return
	}
	defer leave()
	d, err := h.svc.CreateProjectDraft(r.Context(), in.ProjectDraftContent, in.ChatSessionID)
	if err != nil {
		draftError(w, r, err)
		return
	}
	presenter.Created(w, r, d)
}

func (h *DraftHandler) ProjectGet(w http.ResponseWriter, r *http.Request) {
	id, err := projectDraftID(r)
	if err != nil {
		draftError(w, r, err)
		return
	}
	r, leave := enterJSONDelivery(h.gate, w, r)
	if r == nil {
		return
	}
	defer leave()
	d, err := h.svc.GetProjectDraft(r.Context(), id)
	if err != nil {
		draftError(w, r, err)
		return
	}
	presenter.OK(w, r, d)
}

func (h *DraftHandler) ProjectUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := projectDraftID(r)
	if err != nil {
		draftError(w, r, err)
		return
	}
	var in struct {
		draftdom.ProjectDraftContent
		ExpectedVersion int `json:"expected_version"`
	}
	if err = draftBody(w, r, &in); err != nil {
		draftError(w, r, err)
		return
	}
	r, leave := enterJSONDelivery(h.gate, w, r)
	if r == nil {
		return
	}
	defer leave()
	d, err := h.svc.UpdateProjectDraft(r.Context(), id, in.ExpectedVersion, in.ProjectDraftContent)
	if err != nil {
		draftError(w, r, err)
		return
	}
	presenter.OK(w, r, d)
}

func (h *DraftHandler) ProjectGenerate(w http.ResponseWriter, r *http.Request) {
	id, err := projectDraftID(r)
	if err != nil {
		draftError(w, r, err)
		return
	}
	var in struct {
		ExpectedVersion int       `json:"expected_version"`
		AgentID         uuid.UUID `json:"agent_id"`
		Message         string    `json:"message"`
	}
	if err = draftBody(w, r, &in); err != nil {
		draftError(w, r, err)
		return
	}
	r, leave := enterJSONDelivery(h.gate, w, r)
	if r == nil {
		return
	}
	defer leave()
	d, err := h.svc.GenerateProjectDraft(r.Context(), id, in.ExpectedVersion, in.AgentID, in.Message)
	if err != nil {
		draftError(w, r, err)
		return
	}
	presenter.OK(w, r, d)
}

func (h *DraftHandler) ProjectImport(w http.ResponseWriter, r *http.Request) {
	id, err := projectDraftID(r)
	if err != nil {
		draftError(w, r, err)
		return
	}
	var in struct {
		ExpectedVersion int       `json:"expected_version"`
		GenerationID    uuid.UUID `json:"generation_id"`
	}
	if err = draftBody(w, r, &in); err != nil {
		draftError(w, r, err)
		return
	}
	r, leave := enterJSONDelivery(h.gate, w, r)
	if r == nil {
		return
	}
	defer leave()
	d, err := h.svc.ImportProjectDraft(r.Context(), id, in.ExpectedVersion, in.GenerationID)
	if err != nil {
		draftError(w, r, err)
		return
	}
	presenter.OK(w, r, d)
}
