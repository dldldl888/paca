package draftdom

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func validProjectDraftContent() ProjectDraftContent {
	itemID := "30000000-0000-4000-8000-000000000001"
	return ProjectDraftContent{
		Name:          "新项目",
		Goal:          "完成首轮交付",
		ManagerUserID: "10000000-0000-4000-8000-000000000001",
		Items: []ProjectDraftItem{{
			ID:           itemID,
			Title:        "首个任务",
			Requirements: "需求",
			Criteria:     "标准",
			Dependencies: []string{},
		}},
		Participants: []ProjectDraftParticipant{{
			UserID: "10000000-0000-4000-8000-000000000001",
			Role:   "负责人",
		}},
		Materials: []ProjectDraftMaterial{{
			ID:              "20000000-0000-4000-8000-000000000001",
			Kind:            "document",
			Purpose:         "背景资料",
			SourceType:      "internal",
			ProcessedStatus: "ready",
			TaskIDs:         []string{itemID},
			Required:        true,
		}},
	}
}

func TestProjectDraftContentValidateStructure(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*ProjectDraftContent)
		want string
	}{
		{name: "valid", mut: func(*ProjectDraftContent) {}},
		{name: "name required", mut: func(c *ProjectDraftContent) { c.Name = "  " }, want: "name"},
		{name: "name limit", mut: func(c *ProjectDraftContent) { c.Name = strings.Repeat("名", 201) }, want: "name"},
		{name: "manager canonical", mut: func(c *ProjectDraftContent) { c.ManagerUserID = "{" + c.ManagerUserID + "}" }, want: "manager_user_id"},
		{name: "participant duplicate", mut: func(c *ProjectDraftContent) {
			c.Participants = append(c.Participants, c.Participants[0])
		}, want: "participant"},
		{name: "task assignee", mut: func(c *ProjectDraftContent) { c.Items[0].AssigneeUserID = "not-a-user" }, want: "assignee_user_id"},
		{name: "dependency cycle", mut: func(c *ProjectDraftContent) {
			secondID := "30000000-0000-4000-8000-000000000002"
			c.Items = append(c.Items, ProjectDraftItem{ID: secondID})
			c.Items[0].Dependencies = []string{secondID}
			c.Items[1].Dependencies = []string{c.Items[0].ID}
		}, want: "cycle"},
		{name: "material kind", mut: func(c *ProjectDraftContent) { c.Materials[0].Kind = "copy" }, want: "kind"},
		{name: "material id", mut: func(c *ProjectDraftContent) { c.Materials[0].ID = uuid.NewString() + "-extra" }, want: "material"},
		{name: "material read status", mut: func(c *ProjectDraftContent) { c.Materials[0].ReadStatus = "maybe" }, want: "read_status"},
		{name: "material accessed timestamp", mut: func(c *ProjectDraftContent) { c.Materials[0].AccessedAt = "tomorrow" }, want: "accessed_at"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := validProjectDraftContent()
			tt.mut(&content)
			err := content.ValidateStructure()
			if tt.want == "" && err != nil {
				t.Fatalf("valid content rejected: %v", err)
			}
			if tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Fatalf("expected %q error, got %v", tt.want, err)
			}
		})
	}
}

func TestProjectDraftPreviewProjectsMaterialsByAssignee(t *testing.T) {
	owner := "10000000-0000-4000-8000-000000000001"
	peer := "10000000-0000-4000-8000-000000000002"
	firstItem := "30000000-0000-4000-8000-000000000001"
	secondItem := "30000000-0000-4000-8000-000000000002"
	draft := ProjectDraft{ProjectDraftContent: ProjectDraftContent{
		Name: "预览项目", Goal: "按员工检查范围",
		Items: []ProjectDraftItem{
			{ID: firstItem, Title: "甲任务", Requirements: "甲要求", Criteria: "甲标准", AssigneeUserID: owner, DueDate: "2026-12-01T18:00:00+08:00"},
			{ID: secondItem, Title: "乙任务", Requirements: "乙要求", Criteria: "乙标准", AssigneeUserID: peer, DueDate: "2026-12-02T18:00:00+08:00"},
		},
		Participants: []ProjectDraftParticipant{{UserID: owner}, {UserID: peer}},
		Materials: []ProjectDraftMaterial{
			{ID: "20000000-0000-4000-8000-000000000001", Kind: "document", ProcessedStatus: "ready", TaskIDs: []string{firstItem}},
			{ID: "20000000-0000-4000-8000-000000000002", Kind: "document", ProcessedStatus: "ready", TaskIDs: []string{secondItem}},
			{ID: "20000000-0000-4000-8000-000000000003", Kind: "document", ProcessedStatus: "ready"},
		},
	}}
	preview := draft.Preview()
	if len(preview.EmployeeViews) != 2 {
		t.Fatalf("expected two employee views, got %#v", preview.EmployeeViews)
	}
	if preview.EmployeeViews[0].UserID != owner || len(preview.EmployeeViews[0].Items) != 1 || len(preview.EmployeeViews[0].Materials) != 2 {
		t.Fatalf("owner projection leaked or omitted scope: %#v", preview.EmployeeViews[0])
	}
	if preview.EmployeeViews[1].UserID != peer || len(preview.EmployeeViews[1].Items) != 1 || len(preview.EmployeeViews[1].Materials) != 2 {
		t.Fatalf("peer projection leaked or omitted scope: %#v", preview.EmployeeViews[1])
	}
	if preview.EmployeeViews[0].Items[0].ID == preview.EmployeeViews[1].Items[0].ID || preview.EmployeeViews[0].Materials[0].ID == preview.EmployeeViews[1].Materials[0].ID {
		t.Fatalf("task-limited scope crossed employee views: %#v", preview.EmployeeViews)
	}
}

func TestProjectDraftPreviewPreservesWebMaterialVerificationMetadata(t *testing.T) {
	draft := ProjectDraft{ProjectDraftContent: ProjectDraftContent{
		Name: "公共资料", Goal: "验证来源元数据",
		Items:        []ProjectDraftItem{{ID: "30000000-0000-4000-8000-000000000001", Title: "核验", Requirements: "核验", Criteria: "通过", AssigneeUserID: "10000000-0000-4000-8000-000000000001"}},
		Participants: []ProjectDraftParticipant{{UserID: "10000000-0000-4000-8000-000000000001"}},
		Materials: []ProjectDraftMaterial{{
			ID: "20000000-0000-4000-8000-000000000001", Kind: "document", SourceType: "web",
			SourceLocation: "https://example.com/source", SourceVersion: "2026-09-24T08:00:00Z",
			AccessedAt: "2026-09-24T08:00:00Z", ReadStatus: "unverified", ProcessedStatus: "unverified",
			TaskIDs: []string{"30000000-0000-4000-8000-000000000001"},
		}},
	}}
	got := draft.Preview().Materials[0]
	if got.AccessedAt != "2026-09-24T08:00:00Z" || got.ReadStatus != "unverified" {
		t.Fatalf("web verification metadata was not preserved: %#v", got)
	}
}
