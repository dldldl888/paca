package handler

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	agentdom "github.com/Paca-AI/api/internal/domain/agent"
	annotationdom "github.com/Paca-AI/api/internal/domain/annotation"
	automationdom "github.com/Paca-AI/api/internal/domain/automation"
	docdom "github.com/Paca-AI/api/internal/domain/doc"
	taskdom "github.com/Paca-AI/api/internal/domain/task"
	"github.com/Paca-AI/api/internal/platform/delivery"
	pg "github.com/Paca-AI/api/internal/repository/postgres"
	agentsvc "github.com/Paca-AI/api/internal/service/agent"
	"github.com/Paca-AI/api/internal/transport/http/presenter"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"io"
	"net/http"
)

type AgentRunHandler struct {
	Service     *agentsvc.Service
	InternalKey string
	Delivery    *pg.AuthorizationDelivery
}

func (h *AgentRunHandler) Authorize(w http.ResponseWriter, r *http.Request) {
	if h.InternalKey == "" || subtle.ConstantTimeCompare([]byte(h.InternalKey), []byte(r.Header.Get("X-Internal-Token"))) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var input struct {
		ConversationID string `json:"conversation_id"`
		AgentID        string `json:"agent_id"`
		BeginTurn      *bool  `json:"begin_turn"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	d.DisallowUnknownFields()
	err := d.Decode(&input)
	var extra any
	if err != nil || d.Decode(&extra) != io.EOF || input.BeginTurn == nil {
		http.Error(w, "invalid request", 400)
		return
	}
	cid, e1 := uuid.Parse(input.ConversationID)
	aid, e2 := uuid.Parse(input.AgentID)
	if e1 != nil || e2 != nil || cid == uuid.Nil || aid == uuid.Nil {
		http.Error(w, "invalid request", 400)
		return
	}
	result, err := h.Service.AuthorizeRun(r.Context(), cid, aid, *input.BeginTurn)
	if errors.Is(err, delivery.ErrBlocked) {
		http.Error(w, "authorization transition pending", 409)
		return
	}
	if err != nil {
		for _, refusal := range []error{agentdom.ErrConversationNotFound, agentdom.ErrChatSessionNotFound, agentdom.ErrAgentNotFound, taskdom.ErrTaskNotFound, taskdom.ErrTaskForbidden, docdom.ErrDocNotFound, annotationdom.ErrAnnotationNotFound, automationdom.ErrNotFound} {
			if errors.Is(err, refusal) {
				http.Error(w, "run authorization refused", http.StatusNotFound)
				return
			}
		}
		presenter.Error(w, r, err)
		return
	}
	presenter.OK(w, r, result)
}

func (h *AgentRunHandler) Recipients(w http.ResponseWriter, r *http.Request) {
	if h.InternalKey == "" || subtle.ConstantTimeCompare([]byte(h.InternalKey), []byte(r.Header.Get("X-Internal-Token"))) != 1 {
		http.Error(w, "unauthorized", 401)
		return
	}
	var input agentsvc.RealtimeScope
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536))
	d.DisallowUnknownFields()
	var extra any
	if d.Decode(&input) != nil || d.Decode(&extra) != io.EOF {
		http.Error(w, "invalid request", 400)
		return
	}
	result, err := h.Service.CurrentRecipients(r.Context(), input)
	if errors.Is(err, delivery.ErrBlocked) {
		http.Error(w, "authorization transition pending", 409)
		return
	}
	if err != nil {
		presenter.Error(w, r, err)
		return
	}
	presenter.OK(w, r, result)
}
func (h *AgentRunHandler) Participant(w http.ResponseWriter, r *http.Request) {
	if h.InternalKey == "" || subtle.ConstantTimeCompare([]byte(h.InternalKey), []byte(r.Header.Get("X-Internal-Token"))) != 1 {
		http.Error(w, "unauthorized", 401)
		return
	}
	if h.Delivery == nil {
		http.Error(w, "authorization unavailable", 503)
		return
	}
	if r.Method == http.MethodGet {
		state, err := h.Delivery.State(r.Context())
		if err != nil {
			presenter.Error(w, r, err)
			return
		}
		presenter.OK(w, r, state)
		return
	}
	if r.Method == http.MethodDelete {
		boot := chi.URLParam(r, "bootID")
		if _, err := uuid.Parse(boot); err != nil {
			http.Error(w, "invalid boot id", 400)
			return
		}
		if err := h.Delivery.Unenroll(r.Context(), boot); err != nil {
			presenter.Error(w, r, err)
			return
		}
		presenter.OK(w, r, map[string]bool{"removed": true})
		return
	}
	var in struct {
		BootID string `json:"boot_id"`
		Kind   string `json:"kind"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	d.DisallowUnknownFields()
	var extra any
	if d.Decode(&in) != nil || d.Decode(&extra) != io.EOF {
		http.Error(w, "invalid request", 400)
		return
	}
	state, err := h.Delivery.Enroll(r.Context(), in.BootID, in.Kind)
	if err != nil {
		presenter.Error(w, r, err)
		return
	}
	presenter.OK(w, r, state)
}
