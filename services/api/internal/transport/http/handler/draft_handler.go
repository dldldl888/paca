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
)

type DraftHandler struct{ svc *draftsvc.Service }

func NewDraftHandler(svc *draftsvc.Service) *DraftHandler { return &DraftHandler{svc} }
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
	if errors.Is(e, delivery.ErrBlocked) {
		e = draftdom.ErrPending
	}
	var validation *pg.DraftValidationError
	if errors.As(e, &validation) {
		e = apierr.New(apierr.CodeBadRequest, "draft has validation issues; refresh preview")
	}
	presenter.Error(w, r, e)
}
func (h *DraftHandler) List(w http.ResponseWriter, r *http.Request) {
	pid, _, e := draftIDs(r)
	if e != nil {
		draftError(w, r, e)
		return
	}
	items, e := h.svc.Repo.List(r.Context(), pid)
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
	d, e := h.svc.Repo.Create(r.Context(), pid, c)
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
	d, e := h.svc.Repo.Get(r.Context(), pid, id)
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
	d, e := h.svc.Repo.Update(r.Context(), pid, id, in.ExpectedVersion, in.Content)
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
	p, e := h.svc.Repo.Preview(r.Context(), pid, id, in.ExpectedVersion)
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
	d, e := h.svc.Import(r.Context(), pid, id, in.ExpectedVersion, in.GenerationID)
	if e != nil {
		draftError(w, r, e)
		return
	}
	presenter.OK(w, r, d)
}
