// Package draftdom defines the private planning document and its employee projection.
package draftdom

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrNotFound          = errors.New("draft not found")
	ErrConflict          = errors.New("draft version or publication conflict")
	ErrPending           = errors.New("draft generation is not complete")
	ErrProvenanceUnknown = errors.New("draft source history is unknown; create a new draft from currently readable sources")
)

type Item struct {
	ID               string   `json:"id"`
	Title            string   `json:"title"`
	Requirements     string   `json:"requirements"`
	Criteria         string   `json:"criteria"`
	AssigneeMemberID string   `json:"assignee_member_id"`
	DueDate          string   `json:"due_date"`
	StartDate        string   `json:"start_date"`
	Importance       int      `json:"importance"`
	Dependencies     []string `json:"dependencies"`
}
type Content struct {
	Goal        string   `json:"goal"`
	Items       []Item   `json:"items"`
	Sources     []string `json:"sources"`
	Assumptions []string `json:"assumptions"`
}

// ProjectDraftContent is the private, pre-project portion of a new project
// plan. It intentionally stores stable user/document identifiers only; no
// project or member row is created until a later publish transaction.
type ProjectDraftContent struct {
	Name          string                    `json:"name"`
	Goal          string                    `json:"goal"`
	ManagerUserID string                    `json:"manager_user_id,omitempty"`
	Participants  []ProjectDraftParticipant `json:"participants"`
	Materials     []ProjectDraftMaterial    `json:"materials"`
}

type ProjectDraftParticipant struct {
	UserID string `json:"user_id"`
	Role   string `json:"role,omitempty"`
}

type ProjectDraftMaterial struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"`
	Purpose  string   `json:"purpose,omitempty"`
	TaskIDs  []string `json:"task_ids,omitempty"`
	Required bool     `json:"required"`
}

type ProjectDraft struct {
	ProjectDraftContent
	ID            uuid.UUID `json:"id"`
	OwnerUserID   uuid.UUID `json:"owner_user_id"`
	ChatSessionID uuid.UUID `json:"chat_session_id"`
	Version       int       `json:"version"`
	State         string    `json:"state"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func (c *ProjectDraftContent) Normalize() {
	if c.Participants == nil {
		c.Participants = []ProjectDraftParticipant{}
	}
	if c.Materials == nil {
		c.Materials = []ProjectDraftMaterial{}
	}
}

func (c ProjectDraftContent) ValidateStructure() error {
	if strings.TrimSpace(c.Name) == "" || utf8.RuneCountInString(c.Name) > 200 {
		return errors.New("project name is required and must be at most 200 characters")
	}
	if utf8.RuneCountInString(c.Goal) > 10000 {
		return errors.New("project goal exceeds limit")
	}
	if c.ManagerUserID != "" {
		id, err := uuid.Parse(c.ManagerUserID)
		if err != nil || id == uuid.Nil || id.String() != c.ManagerUserID {
			return errors.New("manager_user_id must be a canonical UUID")
		}
	}
	if len(c.Participants) > 100 || len(c.Materials) > 100 {
		return errors.New("too many project participants or materials")
	}
	seenUsers := map[string]bool{}
	for _, participant := range c.Participants {
		id, err := uuid.Parse(participant.UserID)
		if err != nil || id == uuid.Nil || id.String() != participant.UserID || seenUsers[participant.UserID] {
			return errors.New("participant user_id must be unique canonical UUIDs")
		}
		seenUsers[participant.UserID] = true
		if utf8.RuneCountInString(participant.Role) > 200 {
			return errors.New("participant role exceeds limit")
		}
	}
	seenMaterials := map[string]bool{}
	for _, material := range c.Materials {
		id, err := uuid.Parse(material.ID)
		if err != nil || id == uuid.Nil || id.String() != material.ID || seenMaterials[material.ID] {
			return errors.New("material id must be unique canonical UUID")
		}
		if material.Kind != "document" && material.Kind != "file" {
			return errors.New("material kind must be document or file")
		}
		seenMaterials[material.ID] = true
		if utf8.RuneCountInString(material.Purpose) > 500 {
			return errors.New("material purpose exceeds limit")
		}
		for _, taskID := range material.TaskIDs {
			task, err := uuid.Parse(taskID)
			if err != nil || task == uuid.Nil || task.String() != taskID {
				return errors.New("material task_ids must be canonical UUIDs")
			}
		}
	}
	return nil
}

type Generation struct {
	ID             uuid.UUID  `json:"id"`
	Version        int        `json:"version"`
	AgentID        uuid.UUID  `json:"agent_id"`
	Status         string     `json:"status"`
	ConversationID *uuid.UUID `json:"conversation_id,omitempty"`
	ChatSessionID  *uuid.UUID `json:"chat_session_id,omitempty"`
	Error          string     `json:"error,omitempty"`
}
type PublishedTask struct {
	ItemID     string    `json:"item_id"`
	TaskID     uuid.UUID `json:"task_id"`
	TaskNumber int64     `json:"task_number"`
}
type SourceBinding struct {
	ConversationID uuid.UUID `json:"conversation_id"`
	AgentID        uuid.UUID `json:"agent_id"`
	ChatSessionID  uuid.UUID `json:"chat_session_id"`
}
type Provenance struct {
	Version  int             `json:"version"`
	Bindings []SourceBinding `json:"bindings"`
}
type Draft struct {
	Content
	ID               uuid.UUID       `json:"id"`
	ProjectID        uuid.UUID       `json:"project_id"`
	OwnerUserID      uuid.UUID       `json:"owner_user_id"`
	Version          int             `json:"version"`
	State            string          `json:"state"`
	Generation       *Generation     `json:"generation,omitempty"`
	PublicationID    *uuid.UUID      `json:"publication_id,omitempty"`
	PublishedVersion int             `json:"published_version,omitempty"`
	PublishedTasks   []PublishedTask `json:"published_tasks"`
	PublishedAt      *time.Time      `json:"published_at,omitempty"`
	Provenance       *Provenance     `json:"-"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}
type Issue struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}
type Preview struct {
	ID         uuid.UUID `json:"id"`
	ProjectID  uuid.UUID `json:"project_id"`
	Version    int       `json:"version"`
	Items      []Item    `json:"items"`
	Issues     []Issue   `json:"issues"`
	CanPublish bool      `json:"can_publish"`
}

func (c *Content) Normalize() {
	if c.Items == nil {
		c.Items = []Item{}
	}
	if c.Sources == nil {
		c.Sources = []string{}
	}
	if c.Assumptions == nil {
		c.Assumptions = []string{}
	}
	for i := range c.Items {
		if c.Items[i].Dependencies == nil {
			c.Items[i].Dependencies = []string{}
		}
	}
}
func (c Content) ValidateStructure() error {
	if utf8.RuneCountInString(c.Goal) > 10000 || len(c.Items) > 30 {
		return fmt.Errorf("goal or item count exceeds limit")
	}
	for _, values := range [][]string{c.Sources, c.Assumptions} {
		if len(values) > 30 {
			return fmt.Errorf("too many sources or assumptions")
		}
		for _, v := range values {
			if utf8.RuneCountInString(v) > 2000 {
				return fmt.Errorf("source or assumption exceeds limit")
			}
		}
	}
	seen := map[string]bool{}
	for _, item := range c.Items {
		id, e := uuid.Parse(item.ID)
		if e != nil || id == uuid.Nil || id.String() != item.ID || seen[item.ID] {
			return fmt.Errorf("item id must be a unique nonzero UUID")
		}
		seen[item.ID] = true
		if utf8.RuneCountInString(item.Title) > 200 || utf8.RuneCountInString(item.Requirements)+utf8.RuneCountInString(item.Criteria) > 10000 || len(item.Dependencies) > 30 || len(item.DueDate) > 64 || len(item.StartDate) > 64 || len(item.AssigneeMemberID) > 36 || item.Importance < 0 || item.Importance > 4 {
			return fmt.Errorf("item %s exceeds field limits", item.ID)
		}
	}
	return nil
}

// Project is the single whitelist used for preview and publication; background never enters it.
func (d *Draft) Project() Preview {
	p := Preview{ID: d.ID, ProjectID: d.ProjectID, Version: d.Version, Items: append([]Item{}, d.Items...), Issues: []Issue{}}
	add := func(field, msg string) { p.Issues = append(p.Issues, Issue{field, msg}) }
	if len(d.Items) == 0 {
		add("items", "至少添加一项任务")
	}
	ids := map[string]bool{}
	for _, it := range d.Items {
		ids[it.ID] = true
	}
	edges := map[string][]string{}
	for i, it := range p.Items {
		prefix := fmt.Sprintf("items[%d].", i)
		for _, f := range []struct{ k, v string }{{"title", it.Title}, {"requirements", it.Requirements}, {"criteria", it.Criteria}} {
			if strings.TrimSpace(f.v) == "" {
				add(prefix+f.k, "必填")
			}
		}
		if id, e := uuid.Parse(it.AssigneeMemberID); e != nil || id == uuid.Nil {
			add(prefix+"assignee_member_id", "请选择有效的人类项目成员")
		}
		due, e := time.Parse(time.RFC3339, it.DueDate)
		if e != nil {
			add(prefix+"due_date", "截止时间必须包含明确时区偏移")
		}
		if it.StartDate != "" {
			start, se := time.Parse(time.RFC3339, it.StartDate)
			if se != nil {
				add(prefix+"start_date", "开始时间必须包含明确时区偏移")
			} else if e == nil && start.After(due) {
				add(prefix+"start_date", "开始时间不得晚于截止时间")
			}
		}
		depSeen := map[string]bool{}
		for _, dep := range it.Dependencies {
			if dep == it.ID || !ids[dep] || depSeen[dep] {
				add(prefix+"dependencies", "依赖不能自指、重复或指向不存在的任务")
			}
			depSeen[dep] = true
			edges[it.ID] = append(edges[it.ID], dep)
		}
	}
	visiting, done := map[string]bool{}, map[string]bool{}
	var cycle func(string) bool
	cycle = func(id string) bool {
		if visiting[id] {
			return true
		}
		if done[id] {
			return false
		}
		visiting[id] = true
		for _, dep := range edges[id] {
			if cycle(dep) {
				return true
			}
		}
		visiting[id] = false
		done[id] = true
		return false
	}
	for id := range ids {
		if cycle(id) {
			add("items.dependencies", "任务依赖存在循环")
			break
		}
	}
	p.CanPublish = len(p.Issues) == 0
	return p
}
func (it Item) Description() json.RawMessage {
	blocks := []any{}
	for _, text := range []string{"交付要求", it.Requirements, "完成标准", it.Criteria} {
		blocks = append(blocks, map[string]any{"type": "paragraph", "content": []any{map[string]any{"type": "text", "text": text, "styles": map[string]any{}}}})
	}
	b, _ := json.Marshal(blocks)
	return b
}
