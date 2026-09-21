package agentsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	agentdom "github.com/Paca-AI/api/internal/domain/agent"
	docdom "github.com/Paca-AI/api/internal/domain/doc"
	"github.com/Paca-AI/api/internal/platform/authz"
	"github.com/Paca-AI/api/internal/platform/database"
	"github.com/Paca-AI/api/internal/platform/delivery"
	"github.com/google/uuid"
	"strings"
	"time"
	"unicode/utf8"
)

type companyRepository interface {
	ListCompanyProjects(context.Context, uuid.UUID) ([]uuid.UUID, error)
	ListCompanyDocuments(context.Context, []uuid.UUID, int) ([]*docdom.Document, error)
}
type CompanySource struct {
	DocumentID       string `json:"document_id"`
	ProjectID        string `json:"project_id"`
	Title            string `json:"title"`
	URL              string `json:"url"`
	UpdatedAt        string `json:"updated_at"`
	Truncated        bool   `json:"truncated"`
	SourceDocumentID string `json:"source_document_id,omitempty"`
	SourceProjectID  string `json:"source_project_id,omitempty"`
	SourceTitle      string `json:"source_title,omitempty"`
}
type CompanyContext struct {
	AuthorizationEpoch string          `json:"authorization_epoch"`
	ConversationID     string          `json:"conversation_id"`
	AgentID            string          `json:"agent_id"`
	InstanceID         string          `json:"instance_id"`
	OwnerUserID        string          `json:"owner_user_id"`
	Text               string          `json:"text"`
	Sources            []CompanySource `json:"sources"`
	Truncated          bool            `json:"truncated"`
}

// CompanyContext selects current material; accumulated ordinary doc refs remain
// authoritative even when a document later leaves the selection folder.
func (s *Service) CompanyContext(ctx context.Context, conversation, agent uuid.UUID) (*CompanyContext, error) {
	ctx, _, leave, err := s.enterDelivery(ctx)
	if err != nil {
		return nil, err
	}
	defer leave()
	a, err := s.AuthorizeRun(ctx, conversation, agent, false)
	if err != nil {
		return nil, err
	}
	if a.Status != "running" {
		return nil, agentdom.ErrConversationNotFound
	}
	repo, ok := s.runRepo.(companyRepository)
	if !ok {
		return nil, errors.New("company context unavailable")
	}
	owner := uuid.MustParse(a.OwnerUserID)
	human, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	actorCtx := authz.WithActorID(delivery.CopyLease(database.CopyTransaction(human, ctx), ctx), owner)
	projects, err := repo.ListCompanyProjects(actorCtx, owner)
	if err != nil {
		return nil, err
	}
	allowed := []uuid.UUID{}
	for _, pid := range projects {
		// Require membership as well as docs.read, exactly as ordinary doc refs do.
		if _, err = s.runRepo.CurrentChatActor(actorCtx, pid, uuid.Nil, owner); err != nil {
			return nil, err
		}
		canRead, e := s.authorizer.HasPermissions(actorCtx, owner, &pid, "", authz.PermissionDocsRead)
		if e != nil {
			return nil, e
		}
		if canRead {
			allowed = append(allowed, pid)
		}
	}
	docs, err := repo.ListCompanyDocuments(actorCtx, allowed, 21)
	if err != nil {
		return nil, err
	}
	result := &CompanyContext{AuthorizationEpoch: a.AuthorizationEpoch, ConversationID: a.ConversationID, AgentID: a.AgentID, InstanceID: a.InstanceID, OwnerUserID: a.OwnerUserID, Sources: []CompanySource{}}
	var b strings.Builder
	b.WriteString("当前公司资料（仅当前用户有权读取的私有项目根目录“公司资料”直属文档）。\n")
	refs := []agentdom.ContextItemRef{}
	remaining := 32000 - utf8.RuneCountInString(b.String()) - 100
	for _, d := range docs {
		if len(result.Sources) == 20 || remaining < 600 {
			result.Truncated = true
			break
		}
		title := []rune(d.Title)
		if len(title) > 256 {
			title = title[:256]
		}
		source := CompanySource{DocumentID: d.ID.String(), ProjectID: d.ProjectID.String(), Title: string(title), URL: fmt.Sprintf("/projects/%s/docs/%s", d.ProjectID, d.ID), UpdatedAt: d.UpdatedAt.UTC().Format(time.RFC3339)}
		if d.SourceDocumentID != nil && d.SourceProjectID != nil && d.SourceUpdatedAt != nil && d.SourceTitle != nil {
			source.SourceDocumentID = d.SourceDocumentID.String()
			source.SourceProjectID = d.SourceProjectID.String()
			source.SourceTitle = string([]rune(*d.SourceTitle)[:min(256, len([]rune(*d.SourceTitle)))])
			source.UpdatedAt = d.SourceUpdatedAt.UTC().Format(time.RFC3339)
		}
		header := fmt.Sprintf("\n来源：%q\n链接：%s\n更新时间：%s\n", source.Title, source.URL, source.UpdatedAt)
		if source.SourceDocumentID != "" {
			header += fmt.Sprintf("原文：%q\n原文标识：/projects/%s/docs/%s\n", source.SourceTitle, source.SourceProjectID, source.SourceDocumentID)
		}
		if remaining <= utf8.RuneCountInString(header)+40 {
			result.Truncated = true
			break
		}
		limit := min(6000, remaining-utf8.RuneCountInString(header)-20)
		body, truncated, e := companyBlockText(d.Content, limit)
		if e != nil {
			return nil, e
		}
		source.Truncated = truncated
		result.Truncated = result.Truncated || truncated
		if body == "" {
			body = "（文档无可用文本）"
		}
		section := header + body + "\n"
		if truncated {
			section += "[本篇已截断]\n"
		}
		b.WriteString(section)
		remaining -= utf8.RuneCountInString(section)
		result.Sources = append(result.Sources, source)
		pid := d.ProjectID.String()
		refs = append(refs, agentdom.ContextItemRef{Type: agentdom.ContextItemDoc, ID: d.ID.String(), ProjectID: &pid, Title: source.Title})
	}
	if len(result.Sources) == 0 {
		b.WriteString("暂无可用公司资料；请明确告知资料缺失，不推断公司事实。\n")
	}
	if result.Truncated {
		b.WriteString("\n[资料已截断：最多20篇、每篇6000字符、合计32000字符；未覆盖全部资料。]\n")
	}
	checked, err := s.checkContext(actorCtx, uuid.Nil, owner, refs, map[uuid.UUID]bool{})
	if err != nil {
		return nil, err
	}
	// No body leaves this service unless every disclosed document is durable.
	if err = s.runRepo.MergeConversationContext(ctx, conversation, checked); err != nil {
		return nil, err
	}
	result.Text = b.String()
	return result, nil
}

// Only BlockNote text and structural containers are visited; URLs/metadata are
// never extracted or fetched. Oversized/deep documents explicitly truncate.
func companyBlockText(raw json.RawMessage, limit int) (string, bool, error) {
	if len(raw) == 0 {
		return "", false, nil
	}
	if len(raw) > 1<<20 {
		return "（文档结构过大，正文未载入）", true, nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false, err
	}
	var b strings.Builder
	remaining, nodes, truncated := limit, 0, false
	var walk func(any, int)
	walk = func(value any, depth int) {
		nodes++
		if depth > 32 || nodes > 10000 || remaining <= 0 {
			truncated = true
			return
		}
		switch v := value.(type) {
		case []any:
			for i, child := range v {
				walk(child, depth+1)
				if remaining <= 0 || nodes > 10000 {
					if i < len(v)-1 {
						truncated = true
					}
					break
				}
			}
		case map[string]any:
			if v["type"] == "text" {
				if text, ok := v["text"].(string); ok {
					r := []rune(text)
					if len(r) > remaining {
						r = r[:remaining]
						truncated = true
					}
					b.WriteString(string(r))
					remaining -= len(r)
				}
			}
			for _, key := range []string{"content", "children", "rows", "cells"} {
				if child, ok := v[key]; ok {
					walk(child, depth+1)
					if remaining > 0 {
						b.WriteByte('\n')
						remaining--
					}
				}
			}
		}
	}
	walk(value, 0)
	return strings.TrimSpace(b.String()), truncated, nil
}
