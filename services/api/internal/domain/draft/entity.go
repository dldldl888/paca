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

// ProjectDraftValidationError marks user-editable draft content as invalid so
// HTTP callers receive a client error instead of an internal server error.
type ProjectDraftValidationError struct{ Message string }

func (e *ProjectDraftValidationError) Error() string { return e.Message }

// ProjectDraftGenerationError is a recoverable Hermes-output failure. The
// draft and conversation remain available for another attempt.
type ProjectDraftGenerationError struct{ Message string }

func (e *ProjectDraftGenerationError) Error() string { return e.Message }

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
	Items         []ProjectDraftItem        `json:"items"`
	Participants  []ProjectDraftParticipant `json:"participants"`
	Materials     []ProjectDraftMaterial    `json:"materials"`
}

type ProjectDraftItem struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	Requirements   string   `json:"requirements"`
	Criteria       string   `json:"criteria"`
	AssigneeUserID string   `json:"assignee_user_id,omitempty"`
	DueDate        string   `json:"due_date,omitempty"`
	StartDate      string   `json:"start_date,omitempty"`
	Importance     int      `json:"importance"`
	Dependencies   []string `json:"dependencies"`
}

type ProjectDraftParticipant struct {
	UserID string `json:"user_id"`
	Role   string `json:"role,omitempty"`
}

type ProjectDraftMaterial struct {
	ID              string   `json:"id"`
	Kind            string   `json:"kind"`
	SourceType      string   `json:"source_type,omitempty"`
	SourceLocation  string   `json:"source_location,omitempty"`
	SourceVersion   string   `json:"source_version,omitempty"`
	ProcessedStatus string   `json:"processed_status,omitempty"`
	Purpose         string   `json:"purpose,omitempty"`
	TaskIDs         []string `json:"task_ids,omitempty"`
	Required        bool     `json:"required"`
}

// ProjectDraftMaterialCandidate is readable internal-document metadata for a
// private project draft. Content is fetched through the normal document
// endpoint after its permission check.
type ProjectDraftMaterialCandidate struct {
	ID              uuid.UUID `json:"id"`
	ProjectID       uuid.UUID `json:"project_id"`
	Title           string    `json:"title"`
	SourceType      string    `json:"source_type"`
	SourceLocation  string    `json:"source_location"`
	SourceVersion   string    `json:"source_version"`
	ProcessedStatus string    `json:"processed_status"`
	Visibility      string    `json:"visibility"`
}

// ProjectDraftWebCandidate is public-web metadata returned to a private draft.
// The page is unverified until a later preview/material step explicitly adds it.
type ProjectDraftWebCandidate struct {
	Title           string    `json:"title"`
	URL             string    `json:"url"`
	Snippet         string    `json:"snippet,omitempty"`
	SourceType      string    `json:"source_type"`
	AccessedAt      time.Time `json:"accessed_at"`
	ReadStatus      string    `json:"read_status"`
	ProcessedStatus string    `json:"processed_status"`
}

type ProjectDraftPreviewIssue struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

type ProjectDraftPreview struct {
	ID           uuid.UUID                  `json:"id"`
	Version      int                        `json:"version"`
	Name         string                     `json:"name"`
	Goal         string                     `json:"goal"`
	Items        []ProjectDraftItem         `json:"items"`
	Participants []ProjectDraftParticipant  `json:"participants"`
	Materials    []ProjectDraftMaterial     `json:"materials"`
	Issues       []ProjectDraftPreviewIssue `json:"issues"`
	CanPublish   bool                       `json:"can_publish"`
}

type ProjectDraftFile struct {
	ID               uuid.UUID `json:"id"`
	DraftID          uuid.UUID `json:"draft_id"`
	FileID           uuid.UUID `json:"file_id"`
	FileName         string    `json:"file_name"`
	ContentType      string    `json:"content_type"`
	FileSize         int64     `json:"file_size"`
	UploadStatus     string    `json:"upload_status"`
	ProcessingStatus string    `json:"processing_status"`
	ProcessingError  string    `json:"processing_error,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type ProjectDraft struct {
	ProjectDraftContent
	ID            uuid.UUID               `json:"id"`
	OwnerUserID   uuid.UUID               `json:"owner_user_id"`
	ChatSessionID uuid.UUID               `json:"chat_session_id"`
	Version       int                     `json:"version"`
	State         string                  `json:"state"`
	Generation    *ProjectDraftGeneration `json:"generation,omitempty"`
	CreatedAt     time.Time               `json:"created_at"`
	UpdatedAt     time.Time               `json:"updated_at"`
}

// ProjectDraftGeneration records the one Hermes turn currently associated
// with a private project draft. BaseEventIndex makes the import boundary
// explicit: only events appended after the generation request are eligible.
type ProjectDraftGeneration struct {
	ID             uuid.UUID  `json:"id"`
	Version        int        `json:"version"`
	AgentID        uuid.UUID  `json:"agent_id"`
	Status         string     `json:"status"` // starting | running | imported | failed
	BaseEventIndex int        `json:"base_event_index"`
	ConversationID *uuid.UUID `json:"conversation_id,omitempty"`
	Error          string     `json:"error,omitempty"`
}

func (c *ProjectDraftContent) Normalize() {
	if c.Items == nil {
		c.Items = []ProjectDraftItem{}
	}
	if c.Participants == nil {
		c.Participants = []ProjectDraftParticipant{}
	}
	if c.Materials == nil {
		c.Materials = []ProjectDraftMaterial{}
	}
	for i := range c.Items {
		if c.Items[i].Dependencies == nil {
			c.Items[i].Dependencies = []string{}
		}
	}
}

func (c ProjectDraftContent) ValidateStructure() error {
	invalid := func(message string) error { return &ProjectDraftValidationError{Message: message} }
	if strings.TrimSpace(c.Name) == "" || utf8.RuneCountInString(c.Name) > 200 {
		return invalid("project name is required and must be at most 200 characters")
	}
	if utf8.RuneCountInString(c.Goal) > 10000 {
		return invalid("project goal exceeds limit")
	}
	if c.ManagerUserID != "" {
		id, err := uuid.Parse(c.ManagerUserID)
		if err != nil || id == uuid.Nil || id.String() != c.ManagerUserID {
			return invalid("manager_user_id must be a canonical UUID")
		}
	}
	if len(c.Items) > 30 || len(c.Participants) > 100 || len(c.Materials) > 100 {
		return invalid("too many project items, participants or materials")
	}
	itemIDs := map[string]bool{}
	for _, item := range c.Items {
		id, err := uuid.Parse(item.ID)
		if err != nil || id == uuid.Nil || id.String() != item.ID || itemIDs[item.ID] {
			return invalid("project item id must be unique canonical UUIDs")
		}
		itemIDs[item.ID] = true
	}
	for _, item := range c.Items {
		if utf8.RuneCountInString(item.Title) > 200 || utf8.RuneCountInString(item.Requirements)+utf8.RuneCountInString(item.Criteria) > 10000 || len(item.DueDate) > 64 || len(item.StartDate) > 64 || item.Importance < 0 || item.Importance > 4 {
			return invalid("project item exceeds field limits")
		}
		if item.AssigneeUserID != "" {
			assignee, err := uuid.Parse(item.AssigneeUserID)
			if err != nil || assignee == uuid.Nil || assignee.String() != item.AssigneeUserID {
				return invalid("project item assignee_user_id must be a canonical UUID")
			}
		}
		seenDependencies := map[string]bool{}
		for _, dependencyID := range item.Dependencies {
			dependency, err := uuid.Parse(dependencyID)
			if err != nil || dependency == uuid.Nil || dependency.String() != dependencyID || dependencyID == item.ID || seenDependencies[dependencyID] || !itemIDs[dependencyID] {
				return invalid("project item dependencies must reference unique canonical UUIDs")
			}
			seenDependencies[dependencyID] = true
		}
	}
	seenUsers := map[string]bool{}
	for _, participant := range c.Participants {
		id, err := uuid.Parse(participant.UserID)
		if err != nil || id == uuid.Nil || id.String() != participant.UserID || seenUsers[participant.UserID] {
			return invalid("participant user_id must be unique canonical UUIDs")
		}
		seenUsers[participant.UserID] = true
		if utf8.RuneCountInString(participant.Role) > 200 {
			return invalid("participant role exceeds limit")
		}
	}
	seenMaterials := map[string]bool{}
	for _, material := range c.Materials {
		id, err := uuid.Parse(material.ID)
		if err != nil || id == uuid.Nil || id.String() != material.ID || seenMaterials[material.ID] {
			return invalid("material id must be unique canonical UUID")
		}
		if material.Kind != "document" && material.Kind != "file" {
			return invalid("material kind must be document or file")
		}
		seenMaterials[material.ID] = true
		if material.SourceType != "" && material.SourceType != "internal" && material.SourceType != "upload" && material.SourceType != "web" && material.SourceType != "ai_summary" {
			return invalid("material source_type is invalid")
		}
		if material.ProcessedStatus != "" && material.ProcessedStatus != "pending" && material.ProcessedStatus != "ready" && material.ProcessedStatus != "failed" && material.ProcessedStatus != "unverified" {
			return invalid("material processed_status is invalid")
		}
		if utf8.RuneCountInString(material.SourceLocation) > 2000 || utf8.RuneCountInString(material.SourceVersion) > 200 || utf8.RuneCountInString(material.Purpose) > 500 {
			return invalid("material source metadata exceeds limit")
		}
		for _, taskID := range material.TaskIDs {
			task, err := uuid.Parse(taskID)
			if err != nil || task == uuid.Nil || task.String() != taskID {
				return invalid("material task_ids must be canonical UUIDs")
			}
			if !itemIDs[taskID] {
				return invalid("material task_ids must reference project items")
			}
		}
	}
	return nil
}

func (d *ProjectDraft) Preview() *ProjectDraftPreview {
	p := &ProjectDraftPreview{
		ID: d.ID, Version: d.Version, Name: d.Name, Goal: d.Goal,
		Items: append([]ProjectDraftItem{}, d.Items...), Participants: append([]ProjectDraftParticipant{}, d.Participants...), Materials: append([]ProjectDraftMaterial{}, d.Materials...),
		Issues: []ProjectDraftPreviewIssue{}, CanPublish: true,
	}
	add := func(field, message string) {
		p.Issues = append(p.Issues, ProjectDraftPreviewIssue{Field: field, Message: message})
	}
	if err := d.ProjectDraftContent.ValidateStructure(); err != nil {
		add("draft", err.Error())
	}
	if strings.TrimSpace(d.Goal) == "" {
		add("goal", "项目目标不能为空")
	}
	if len(d.Items) == 0 {
		add("items", "至少添加一项任务")
	}
	if len(d.Participants) == 0 {
		add("participants", "至少选择一名参与人")
	}
	for i, item := range d.Items {
		prefix := fmt.Sprintf("items[%d].", i)
		if strings.TrimSpace(item.Title) == "" {
			add(prefix+"title", "任务标题不能为空")
		}
		if strings.TrimSpace(item.Requirements) == "" {
			add(prefix+"requirements", "任务要求不能为空")
		}
		if strings.TrimSpace(item.Criteria) == "" {
			add(prefix+"criteria", "验收标准不能为空")
		}
		if strings.TrimSpace(item.AssigneeUserID) == "" {
			add(prefix+"assignee_user_id", "请选择任务负责人")
		}
		if _, err := time.Parse(time.RFC3339, item.DueDate); err != nil {
			add(prefix+"due_date", "截止时间必须包含明确时区")
		}
		if item.StartDate != "" {
			start, startErr := time.Parse(time.RFC3339, item.StartDate)
			due, dueErr := time.Parse(time.RFC3339, item.DueDate)
			if startErr != nil {
				add(prefix+"start_date", "开始时间必须包含明确时区")
			} else if dueErr == nil && start.After(due) {
				add(prefix+"start_date", "开始时间不得晚于截止时间")
			}
		}
	}
	for i, material := range d.Materials {
		if material.Required && material.ProcessedStatus != "ready" {
			add(fmt.Sprintf("materials[%d]", i), "必要资料必须可读取且已处理")
		}
	}
	p.CanPublish = len(p.Issues) == 0
	return p
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
