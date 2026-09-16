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
	agentsvc "github.com/Paca-AI/api/internal/service/agent"
	"github.com/Paca-AI/api/internal/transport/http/presenter"
	"github.com/google/uuid"
	"io"
	"net/http"
)

type AgentRunHandler struct {
	Service     *agentsvc.Service
	InternalKey string
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
