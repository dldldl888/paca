package agentsvc

import (
	"context"
	"encoding/json"
	"errors"
	agentdom "github.com/Paca-AI/api/internal/domain/agent"
	docdom "github.com/Paca-AI/api/internal/domain/doc"
	"github.com/Paca-AI/api/internal/platform/authz"
	"github.com/google/uuid"
	"strings"
	"testing"
	"time"
)

type companyFixture struct {
	agentdom.Repository
	docdom.DocumentService
	owner, allowed, denied uuid.UUID
	conversation           *agentdom.AgentConversation
	documents              []*docdom.Document
	refs                   []agentdom.ContextItemRef
	mergeErr, readErr      error
}

func (f *companyFixture) FindConversationByID(context.Context, uuid.UUID) (*agentdom.AgentConversation, error) {
	return f.conversation, nil
}
func (f *companyFixture) FindAgentByID(context.Context, uuid.UUID) (*agentdom.Agent, error) {
	return &agentdom.Agent{AgentScope: agentdom.AgentScopeGlobal}, nil
}
func (f *companyFixture) CurrentChatActor(_ context.Context, p, m, u uuid.UUID) (uuid.UUID, error) {
	if u != f.owner {
		return uuid.Nil, agentdom.ErrConversationNotFound
	}
	return u, nil
}
func (f *companyFixture) MergeConversationContext(_ context.Context, _ uuid.UUID, refs []agentdom.ContextItemRef) error {
	if f.mergeErr != nil {
		return f.mergeErr
	}
	f.refs = append(f.refs, refs...)
	return nil
}
func (f *companyFixture) BeginConversationTurn(context.Context, uuid.UUID, uuid.UUID, bool) (bool, error) {
	return false, nil
}
func (f *companyFixture) ListCompanyProjects(_ context.Context, owner uuid.UUID) ([]uuid.UUID, error) {
	if owner != f.owner {
		panic("client actor trusted")
	}
	return []uuid.UUID{f.denied, f.allowed}, nil
}
func (f *companyFixture) ListCompanyDocuments(_ context.Context, projects []uuid.UUID, limit int) ([]*docdom.Document, error) {
	if len(projects) != 1 || projects[0] != f.allowed {
		panic("permission must precede query/limit")
	}
	return f.documents, f.readErr
}
func (f *companyFixture) ListGlobalPermissions(context.Context, uuid.UUID) ([]authz.Permission, error) {
	return nil, nil
}
func (f *companyFixture) ListProjectPermissions(_ context.Context, _ uuid.UUID, p uuid.UUID) ([]authz.Permission, error) {
	if p == f.allowed {
		return []authz.Permission{authz.PermissionDocsRead}, nil
	}
	return nil, nil
}
func (f *companyFixture) GetDocument(_ context.Context, id uuid.UUID) (*docdom.Document, error) {
	for _, d := range f.documents {
		if d.ID == id {
			return d, nil
		}
	}
	return nil, docdom.ErrDocNotFound
}
func companyTestService() (*Service, *companyFixture) {
	f := &companyFixture{owner: uuid.New(), allowed: uuid.New(), denied: uuid.New()}
	f.conversation = &agentdom.AgentConversation{ID: uuid.New(), AgentID: uuid.New(), ActorUserID: &f.owner, Audience: agentdom.AudienceOwnerPrivate, Status: "running", ContextItems: []agentdom.ContextItemRef{}}
	s := New(f, nil, nil, nil).WithPrivateRuns(uuid.New(), f, f, nil, nil).WithAuthorizer(authz.NewAuthorizer(f))
	return s, f
}
func TestCompanyContextAuthorizedSourcesAndFreshness(t *testing.T) {
	s, f := companyTestService()
	d := &docdom.Document{ID: uuid.New(), ProjectID: f.allowed, Title: "合成制度", UpdatedAt: time.Date(2026, 9, 17, 1, 2, 3, 0, time.UTC), Content: json.RawMessage(`[{"content":[{"type":"text","text":"制度甲"}],"children":[{"content":[{"type":"text","text":"子条款"}]}]},{"content":{"type":"tableContent","rows":[{"cells":[[{"type":"text","text":"表格职责"}]]}]}}]`)}
	f.documents = []*docdom.Document{d}
	result, err := s.CompanyContext(authz.WithActorID(context.Background(), uuid.New()), f.conversation.ID, f.conversation.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"制度甲", "子条款", "表格职责", "更新时间", d.ID.String(), "2026-09-17T01:02:03Z"} {
		if !strings.Contains(result.Text, want) {
			t.Fatalf("missing %q in %s", want, result.Text)
		}
	}
	if len(result.Sources) != 1 || result.OwnerUserID != f.owner.String() || len(f.refs) != 1 || f.refs[0].Type != agentdom.ContextItemDoc {
		t.Fatalf("identity or durable refs: %+v %+v", result, f.refs)
	}
	d.Content = json.RawMessage(`[{"content":[{"type":"text","text":"新版"}]}]`)
	d.UpdatedAt = d.UpdatedAt.Add(time.Hour)
	result, err = s.CompanyContext(context.Background(), f.conversation.ID, f.conversation.AgentID)
	if err != nil || !strings.Contains(result.Text, "新版") || !strings.Contains(result.Text, "02:02:03Z") {
		t.Fatal(result, err)
	}
}
func TestCompanyContextEmptyLimitsAndFailures(t *testing.T) {
	for _, mode := range []string{"empty", "limits", "merge_failure", "read_failure", "wrong_agent", "stopped"} {
		t.Run(mode, func(t *testing.T) {
			s, f := companyTestService()
			aid := f.conversation.AgentID
			content, _ := json.Marshal([]any{map[string]any{"content": []any{map[string]any{"type": "text", "text": strings.Repeat("界", 7000)}}}})
			if mode != "empty" {
				for i := 0; i < 21; i++ {
					f.documents = append(f.documents, &docdom.Document{ID: uuid.New(), ProjectID: f.allowed, Title: "制度", Content: content})
				}
			}
			switch mode {
			case "merge_failure":
				f.mergeErr = errors.New("persist failed")
			case "read_failure":
				f.readErr = errors.New("read failed")
			case "wrong_agent":
				aid = uuid.New()
			case "stopped":
				f.conversation.Status = "stopped"
			}
			out, err := s.CompanyContext(context.Background(), f.conversation.ID, aid)
			if mode != "empty" && mode != "limits" {
				if err == nil || out != nil {
					t.Fatal("returned body on failure", out, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "empty" && (!strings.Contains(out.Text, "暂无") || len(out.Sources) != 0) {
				t.Fatal(out)
			}
			if mode == "limits" {
				if !out.Truncated || !strings.Contains(out.Text, "截断") || len(out.Sources) > 20 || len([]rune(out.Text)) > 32000 {
					t.Fatal("unbounded or silent truncation", len([]rune(out.Text)), out.Truncated, len(out.Sources))
				}
				if len(f.refs) != len(out.Sources) {
					t.Fatal("source refs missing")
				}
			}
		})
	}
}

func TestCompanyBlockTextBounds(t *testing.T) {
	text, truncated, err := companyBlockText(json.RawMessage(`[{"content":[{"type":"text","text":"你好世"},{"type":"text","text":"界"}],"props":{"url":"private-url","text":"hidden"}}]`), 3)
	if err != nil || text != "你好世" || !truncated {
		t.Fatal("exact text boundary silently lost suffix", text, truncated, err)
	}
	text, truncated, err = companyBlockText(json.RawMessage(`[{"content":[{"type":"link","href":"private-url","content":[{"type":"text","text":"标签"}]}],"props":{"text":"hidden"}}]`), 100)
	if err != nil || truncated || strings.Contains(text, "private-url") || strings.Contains(text, "hidden") || !strings.Contains(text, "标签") {
		t.Fatal(text, truncated, err)
	}
	_, truncated, err = companyBlockText(json.RawMessage(strings.Repeat(" ", 1<<20+1)), 100)
	if err != nil || !truncated {
		t.Fatal("oversized structure not bounded", err)
	}
	if _, _, err = companyBlockText(json.RawMessage(`{"bad"`), 100); err == nil {
		t.Fatal("invalid JSON accepted")
	}
}
