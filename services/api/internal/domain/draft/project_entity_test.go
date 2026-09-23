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
		{name: "material kind", mut: func(c *ProjectDraftContent) { c.Materials[0].Kind = "copy" }, want: "kind"},
		{name: "material id", mut: func(c *ProjectDraftContent) { c.Materials[0].ID = uuid.NewString() + "-extra" }, want: "material"},
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
