package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	attachmentdom "github.com/Paca-AI/api/internal/domain/attachment"
	docdom "github.com/Paca-AI/api/internal/domain/doc"
	draftdom "github.com/Paca-AI/api/internal/domain/draft"
	"github.com/Paca-AI/api/internal/platform/authz"
	"github.com/Paca-AI/api/internal/platform/database"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

type projectDraftFileRow struct {
	ID               uuid.UUID `db:"id"`
	DraftID          uuid.UUID `db:"draft_id"`
	FileID           uuid.UUID `db:"file_id"`
	FileName         string    `db:"file_name"`
	ContentType      string    `db:"content_type"`
	FileSize         int64     `db:"file_size"`
	UploadStatus     string    `db:"upload_status"`
	ProcessingStatus string    `db:"processing_status"`
	ProcessingError  *string   `db:"processing_error"`
	CreatedAt        time.Time `db:"created_at"`
	UpdatedAt        time.Time `db:"updated_at"`
}

func projectDraftFileFromRow(r projectDraftFileRow) *draftdom.ProjectDraftFile {
	f := &draftdom.ProjectDraftFile{ID: r.ID, DraftID: r.DraftID, FileID: r.FileID, FileName: r.FileName, ContentType: r.ContentType, FileSize: r.FileSize, UploadStatus: r.UploadStatus, ProcessingStatus: r.ProcessingStatus, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
	if r.ProcessingError != nil {
		f.ProcessingError = *r.ProcessingError
	}
	return f
}

type projectDraftRow struct {
	ID                 uuid.UUID  `db:"id"`
	OwnerUserID        uuid.UUID  `db:"owner_user_id"`
	ChatSessionID      uuid.UUID  `db:"chat_session_id"`
	Version            int        `db:"version"`
	State              string     `db:"state"`
	Content            []byte     `db:"content"`
	Generation         []byte     `db:"generation"`
	CreatedAt          time.Time  `db:"created_at"`
	UpdatedAt          time.Time  `db:"updated_at"`
	PublicationID      *uuid.UUID `db:"publication_id"`
	PublishedProjectID *uuid.UUID `db:"published_project_id"`
	PublicationReceipt []byte     `db:"publication_receipt"`
}

func decodeProjectDraft(row projectDraftRow) (*draftdom.ProjectDraft, error) {
	var content draftdom.ProjectDraftContent
	if err := json.Unmarshal(row.Content, &content); err != nil {
		return nil, err
	}
	content.Normalize()
	var generation *draftdom.ProjectDraftGeneration
	if len(row.Generation) > 0 {
		generation = &draftdom.ProjectDraftGeneration{}
		if err := json.Unmarshal(row.Generation, generation); err != nil {
			return nil, err
		}
	}
	d := &draftdom.ProjectDraft{
		ProjectDraftContent: content,
		ID:                  row.ID,
		OwnerUserID:         row.OwnerUserID,
		ChatSessionID:       row.ChatSessionID,
		Version:             row.Version,
		State:               row.State,
		Generation:          generation,
		CreatedAt:           row.CreatedAt,
		UpdatedAt:           row.UpdatedAt,
		PublicationID:       row.PublicationID,
		PublishedProjectID:  row.PublishedProjectID,
	}
	if len(row.PublicationReceipt) > 0 {
		var receipt draftdom.ProjectDraftPublication
		if err := json.Unmarshal(row.PublicationReceipt, &receipt); err != nil {
			return nil, err
		}
		d.Publication = &receipt
	}
	return d, nil
}

func encodeProjectDraftGeneration(g *draftdom.ProjectDraftGeneration) ([]byte, error) {
	if g == nil {
		return nil, nil
	}
	return json.Marshal(g)
}

const projectDraftSelect = `SELECT id,owner_user_id,chat_session_id,version,state,content,generation,created_at,updated_at,publication_id,published_project_id,publication_receipt FROM project_drafts`

func encodeProjectDraft(content draftdom.ProjectDraftContent) ([]byte, error) {
	content.Normalize()
	return json.Marshal(content)
}

func projectDraftActor(ctx context.Context) (uuid.UUID, error) {
	user, ok := authz.ActorIDFromContext(ctx)
	if !ok || user == uuid.Nil {
		return uuid.Nil, draftdom.ErrNotFound
	}
	return user, nil
}

func (r *DraftRepository) CreateProjectDraft(ctx context.Context, content draftdom.ProjectDraftContent, chatSessionID uuid.UUID) (*draftdom.ProjectDraft, error) {
	user, err := projectDraftActor(ctx)
	if err != nil {
		return nil, err
	}
	if chatSessionID == uuid.Nil {
		return nil, draftdom.ErrNotFound
	}
	content.Normalize()
	b, err := encodeProjectDraft(content)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	d := &draftdom.ProjectDraft{
		ProjectDraftContent: content,
		ID:                  uuid.New(),
		OwnerUserID:         user,
		ChatSessionID:       chatSessionID,
		Version:             1,
		State:               "editable",
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	err = WithTx(ctx, r.db, func(tx *sqlx.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO project_drafts(id,owner_user_id,chat_session_id,version,state,content,generation,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, d.ID, user, chatSessionID, d.Version, d.State, b, nil, now, now)
		return err
	})
	if err != nil {
		return nil, err
	}
	return d, nil
}

func (r *DraftRepository) GetProjectDraft(ctx context.Context, id uuid.UUID) (*draftdom.ProjectDraft, error) {
	user, err := projectDraftActor(ctx)
	if err != nil {
		return nil, err
	}
	var row projectDraftRow
	err = WithTx(ctx, r.db, func(tx *sqlx.Tx) error {
		return tx.GetContext(ctx, &row, projectDraftSelect+` WHERE id=$1 AND owner_user_id=$2`, id, user)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, draftdom.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return decodeProjectDraft(row)
}

func (r *DraftRepository) ListProjectDrafts(ctx context.Context) ([]*draftdom.ProjectDraft, error) {
	user, err := projectDraftActor(ctx)
	if err != nil {
		return nil, err
	}
	var rows []projectDraftRow
	err = WithTx(ctx, r.db, func(tx *sqlx.Tx) error {
		return tx.SelectContext(ctx, &rows, projectDraftSelect+` WHERE owner_user_id=$1 ORDER BY updated_at DESC,id DESC LIMIT 100`, user)
	})
	if err != nil {
		return nil, err
	}
	result := make([]*draftdom.ProjectDraft, 0, len(rows))
	for _, row := range rows {
		d, err := decodeProjectDraft(row)
		if err != nil {
			return nil, err
		}
		result = append(result, d)
	}
	return result, nil
}

// ValidateProjectDraftUsers rechecks the live account state used by a draft
// preview. Publication repeats the same checks inside its write transaction;
// preview only reports the current state so the owner can correct it before
// confirming.
func (r *DraftRepository) ValidateProjectDraftUsers(ctx context.Context, content draftdom.ProjectDraftContent) ([]draftdom.ProjectDraftPreviewIssue, error) {
	type reference struct {
		field string
		id    uuid.UUID
	}
	references := make([]reference, 0, len(content.Participants)+len(content.Items)+1)
	add := func(field, rawID string) {
		id, err := uuid.Parse(rawID)
		if err == nil && id != uuid.Nil {
			references = append(references, reference{field: field, id: id})
		}
	}
	for i, participant := range content.Participants {
		add(fmt.Sprintf("participants[%d].user_id", i), participant.UserID)
	}
	if content.ManagerUserID != "" {
		add("manager_user_id", content.ManagerUserID)
	}
	for i, item := range content.Items {
		add(fmt.Sprintf("items[%d].assignee_user_id", i), item.AssigneeUserID)
	}
	if len(references) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(references))
	args := make([]any, len(references))
	for i, ref := range references {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = ref.id.String()
	}
	var activeIDs []uuid.UUID
	query := `SELECT id FROM users WHERE id IN (` + strings.Join(placeholders, ",") + `) AND deleted_at IS NULL`
	if err := DeliveryQuery(ctx, r.db).SelectContext(ctx, &activeIDs, query, args...); err != nil {
		return nil, err
	}
	active := make(map[uuid.UUID]struct{}, len(activeIDs))
	for _, id := range activeIDs {
		active[id] = struct{}{}
	}
	issues := make([]draftdom.ProjectDraftPreviewIssue, 0)
	for _, ref := range references {
		if _, ok := active[ref.id]; !ok {
			issues = append(issues, draftdom.ProjectDraftPreviewIssue{Field: ref.field, Message: "人员已停用或不存在，请重新选择在职账号"})
		}
	}
	return issues, nil
}

func (r *DraftRepository) CreateProjectDraftFile(ctx context.Context, draftID uuid.UUID, f *attachmentdom.File) (*draftdom.ProjectDraftFile, error) {
	user, err := projectDraftActor(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var out *draftdom.ProjectDraftFile
	err = WithTx(ctx, r.db, func(tx *sqlx.Tx) error {
		var state string
		if err := tx.GetContext(ctx, &state, `SELECT state FROM project_drafts WHERE id=$1 AND owner_user_id=$2 FOR UPDATE`, draftID, user); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return draftdom.ErrNotFound
			}
			return err
		}
		if state != "editable" {
			return draftdom.ErrConflict
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO files(id,storage_key,bucket,file_name,content_type,file_size,upload_status,multipart_upload_id,uploaded_by,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, f.ID, f.StorageKey, f.Bucket, f.FileName, f.ContentType, f.FileSize, f.UploadStatus, f.MultipartUploadID, user, f.CreatedAt, f.UpdatedAt); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO project_draft_files(id,draft_id,file_id,owner_user_id,processing_status,created_at,updated_at) VALUES($1,$2,$3,$4,'pending',$5,$6)`, uuid.New(), draftID, f.ID, user, now, now); err != nil {
			return err
		}
		out = &draftdom.ProjectDraftFile{DraftID: draftID, FileID: f.ID, FileName: f.FileName, ContentType: f.ContentType, FileSize: f.FileSize, UploadStatus: string(f.UploadStatus), ProcessingStatus: "pending", CreatedAt: now, UpdatedAt: now}
		return nil
	})
	return out, err
}

const projectDraftFileSelect = `SELECT p.id,p.draft_id,p.file_id,f.file_name,f.content_type,f.file_size,f.upload_status,p.processing_status,p.processing_error,p.created_at,p.updated_at FROM project_draft_files p JOIN files f ON f.id=p.file_id`

func (r *DraftRepository) GetProjectDraftFile(ctx context.Context, draftID, fileID uuid.UUID) (*draftdom.ProjectDraftFile, error) {
	user, err := projectDraftActor(ctx)
	if err != nil {
		return nil, err
	}
	var row projectDraftFileRow
	err = WithTx(ctx, r.db, func(tx *sqlx.Tx) error {
		return tx.GetContext(ctx, &row, projectDraftFileSelect+` WHERE p.draft_id=$1 AND p.file_id=$2 AND p.owner_user_id=$3`, draftID, fileID, user)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, draftdom.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return projectDraftFileFromRow(row), nil
}

func (r *DraftRepository) GetProjectDraftFileRecord(ctx context.Context, draftID, fileID uuid.UUID) (*attachmentdom.File, error) {
	user, err := projectDraftActor(ctx)
	if err != nil {
		return nil, err
	}
	var row fileRecord
	err = WithTx(ctx, r.db, func(tx *sqlx.Tx) error {
		return tx.GetContext(ctx, &row, `SELECT f.id,f.storage_key,f.bucket,f.file_name,f.content_type,f.file_size,f.upload_status,f.multipart_upload_id,f.uploaded_by,f.created_at,f.updated_at FROM files f JOIN project_draft_files p ON p.file_id=f.id WHERE p.draft_id=$1 AND p.file_id=$2 AND p.owner_user_id=$3`, draftID, fileID, user)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, draftdom.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return fileRecordToEntity(&row), nil
}

// ResetProjectDraftFile returns a durable upload session to pending while
// holding the draft/file rows. A ready file is immutable; callers must upload
// a new file instead of silently replacing parsed content.
func (r *DraftRepository) ResetProjectDraftFile(ctx context.Context, draftID, fileID uuid.UUID) (*attachmentdom.File, error) {
	user, err := projectDraftActor(ctx)
	if err != nil {
		return nil, err
	}
	var out *attachmentdom.File
	err = WithTx(ctx, r.db, func(tx *sqlx.Tx) error {
		var state string
		if err := tx.GetContext(ctx, &state, `SELECT state FROM project_drafts WHERE id=$1 AND owner_user_id=$2 FOR UPDATE`, draftID, user); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return draftdom.ErrNotFound
			}
			return err
		}
		if state != "editable" {
			return draftdom.ErrConflict
		}
		var row struct {
			ID                string    `db:"id"`
			StorageKey        string    `db:"storage_key"`
			Bucket            string    `db:"bucket"`
			FileName          string    `db:"file_name"`
			ContentType       string    `db:"content_type"`
			FileSize          int64     `db:"file_size"`
			UploadStatus      string    `db:"upload_status"`
			MultipartUploadID *string   `db:"multipart_upload_id"`
			UploadedBy        *string   `db:"uploaded_by"`
			CreatedAt         time.Time `db:"created_at"`
			UpdatedAt         time.Time `db:"updated_at"`
			ProcessingStatus  string    `db:"processing_status"`
		}
		if err := tx.GetContext(ctx, &row, `SELECT f.id,f.storage_key,f.bucket,f.file_name,f.content_type,f.file_size,f.upload_status,f.multipart_upload_id,f.uploaded_by,f.created_at,f.updated_at,p.processing_status FROM files f JOIN project_draft_files p ON p.file_id=f.id WHERE p.draft_id=$1 AND p.file_id=$2 AND p.owner_user_id=$3 FOR UPDATE`, draftID, fileID, user); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return draftdom.ErrNotFound
			}
			return err
		}
		if row.UploadStatus == string(attachmentdom.UploadStatusUploaded) && row.ProcessingStatus == "ready" {
			return draftdom.ErrConflict
		}
		now := time.Now()
		if _, err := tx.ExecContext(ctx, `UPDATE files SET upload_status=$2,multipart_upload_id=NULL,updated_at=$3 WHERE id=$1`, fileID, attachmentdom.UploadStatusPending, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE project_draft_files SET processing_status='pending',processing_error=NULL,extracted_text=NULL,updated_at=$3 WHERE draft_id=$1 AND file_id=$2 AND owner_user_id=$4`, draftID, fileID, now, user); err != nil {
			return err
		}
		out = &attachmentdom.File{
			ID: mustParseUUID(row.ID), StorageKey: row.StorageKey, Bucket: row.Bucket, FileName: row.FileName,
			ContentType: row.ContentType, FileSize: row.FileSize, UploadStatus: attachmentdom.UploadStatusPending,
			MultipartUploadID: nil, UploadedBy: stringPtrToUUIDPtr(row.UploadedBy), CreatedAt: row.CreatedAt, UpdatedAt: now,
		}
		return nil
	})
	return out, err
}

func (r *DraftRepository) ListProjectDraftFiles(ctx context.Context, draftID uuid.UUID) ([]*draftdom.ProjectDraftFile, error) {
	user, err := projectDraftActor(ctx)
	if err != nil {
		return nil, err
	}
	var rows []projectDraftFileRow
	err = WithTx(ctx, r.db, func(tx *sqlx.Tx) error {
		return tx.SelectContext(ctx, &rows, projectDraftFileSelect+` WHERE p.draft_id=$1 AND p.owner_user_id=$2 ORDER BY p.created_at ASC,p.id ASC`, draftID, user)
	})
	if err != nil {
		return nil, err
	}
	out := make([]*draftdom.ProjectDraftFile, 0, len(rows))
	for _, row := range rows {
		out = append(out, projectDraftFileFromRow(row))
	}
	return out, nil
}

// PublishProjectDraft materializes the validated private draft in one database
// transaction. Materials are intentionally blocked until their source-sharing
// checks are implemented; silently dropping them would publish an incomplete plan.
func (r *DraftRepository) PublishProjectDraft(ctx context.Context, id uuid.UUID, expected int, publication uuid.UUID) (*draftdom.ProjectDraftPublication, error) {
	user, err := projectDraftActor(ctx)
	if err != nil {
		return nil, err
	}
	if publication == uuid.Nil {
		return nil, draftdom.ErrConflict
	}
	var result *draftdom.ProjectDraftPublication
	err = WithTx(ctx, r.db, func(tx *sqlx.Tx) error {
		var row projectDraftRow
		if err := tx.GetContext(ctx, &row, projectDraftSelect+` WHERE id=$1 AND owner_user_id=$2 FOR UPDATE`, id, user); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return draftdom.ErrNotFound
			}
			return err
		}
		if row.PublicationID != nil {
			if row.Version != expected {
				return draftdom.ErrConflict
			}
			if *row.PublicationID != publication {
				return draftdom.ErrConflict
			}
			if row.PublishedProjectID == nil {
				return draftdom.ErrConflict
			}
			d, err := decodeProjectDraft(row)
			if err != nil {
				return err
			}
			if d.Publication == nil {
				return draftdom.ErrConflict
			}
			result = d.Publication
			return nil
		}
		if row.Version != expected {
			return draftdom.ErrConflict
		}
		d, err := decodeProjectDraft(row)
		if err != nil {
			return err
		}
		preview := d.Preview()
		if !preview.CanPublish {
			return &draftdom.ProjectDraftValidationError{Message: "draft preview contains unresolved issues"}
		}
		now := time.Now()
		projectID := uuid.New()
		settings, _ := json.Marshal(map[string]any{})
		if _, err := tx.ExecContext(ctx, `INSERT INTO projects(id,name,description,task_id_prefix,is_public,settings,created_by,created_at) VALUES($1,$2,$3,$4,false,$5,$6,$7)`, projectID, d.Name, d.Goal, "PRJ", settings, user, now); err != nil {
			return err
		}
		adminRole, editorRole := uuid.New(), uuid.New()
		adminPerms, _ := json.Marshal(map[string]bool{
			"projects.read": true, "projects.write": true,
			"project.members.read": true, "project.members.write": true,
			"project.roles.read": true, "project.roles.write": true,
			"tasks.read": true, "tasks.read_all": true, "tasks.write": true, "tasks.manage": true,
			"sprints.read": true, "sprints.write": true,
			"docs.read": true, "docs.write": true,
			"agents.read": true, "agents.write": true,
			"conversations.read": true, "conversations.write": true,
			"workflows.read": true, "workflows.write": true,
			"environments.read": true, "environments.write": true, "environments.connect": true,
			"annotations.read": true, "annotations.write": true, "annotations.resolve": true,
		})
		editorPerms, _ := json.Marshal(map[string]bool{"projects.read": true, "tasks.read": true, "tasks.write": true, "docs.read": true})
		for _, role := range []struct {
			id          uuid.UUID
			name        string
			permissions []byte
		}{{adminRole, "Admin", adminPerms}, {editorRole, "Editor", editorPerms}} {
			if _, err := tx.ExecContext(ctx, `INSERT INTO project_roles(id,project_id,role_name,permissions,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$5)`, role.id, projectID, role.name, role.permissions, now); err != nil {
				return err
			}
		}
		members := map[uuid.UUID]uuid.UUID{user: uuid.New()}
		adminUsers := map[uuid.UUID]bool{user: true}
		if d.ManagerUserID != "" {
			managerID, err := uuid.Parse(d.ManagerUserID)
			if err != nil {
				return &draftdom.ProjectDraftValidationError{Message: "manager user is invalid"}
			}
			var active bool
			if err := tx.GetContext(ctx, &active, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1 AND deleted_at IS NULL)`, managerID); err != nil {
				return err
			}
			if !active {
				return &draftdom.ProjectDraftValidationError{Message: "manager user is no longer active"}
			}
			if _, selected := members[managerID]; !selected {
				return &draftdom.ProjectDraftValidationError{Message: "manager user must be a selected participant"}
			}
			adminUsers[managerID] = true
		}
		for _, participant := range d.Participants {
			participantID, err := uuid.Parse(participant.UserID)
			if err != nil {
				return &draftdom.ProjectDraftValidationError{Message: "participant user is invalid"}
			}
			var active bool
			if err := tx.GetContext(ctx, &active, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1 AND deleted_at IS NULL)`, participantID); err != nil {
				return err
			}
			if !active {
				return &draftdom.ProjectDraftValidationError{Message: "participant user is no longer active"}
			}
			if _, exists := members[participantID]; !exists {
				members[participantID] = uuid.New()
			}
			role := strings.ToLower(strings.TrimSpace(participant.Role))
			if role == "负责人" || role == "项目负责人" || role == "manager" || role == "owner" {
				adminUsers[participantID] = true
			}
		}
		for memberUser, memberID := range members {
			roleID := editorRole
			if adminUsers[memberUser] {
				roleID = adminRole
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO project_members(id,project_id,user_id,project_role_id,member_type,created_at) VALUES($1,$2,$3,$4,'human',$5)`, memberID, projectID, memberUser, roleID, now); err != nil {
				return err
			}
		}
		taskTypeID, taskStatusID := uuid.New(), uuid.New()
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_types(id,project_id,name,is_default,is_system,created_at,updated_at) VALUES($1,$2,'Task',true,false,$3,$3)`, taskTypeID, projectID, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_statuses(id,project_id,name,position,category,is_default,created_at,updated_at) VALUES($1,$2,'Backlog',1,'backlog',true,$3,$3)`, taskStatusID, projectID, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_counters(project_id,last_value) VALUES($1,0)`, projectID); err != nil {
			return err
		}
		taskIDs := make(map[string]uuid.UUID, len(d.Items))
		for _, item := range d.Items {
			assignee, err := uuid.Parse(item.AssigneeUserID)
			if err != nil || members[assignee] == uuid.Nil {
				return &draftdom.ProjectDraftValidationError{Message: "task assignee must be a selected participant"}
			}
			due, _ := time.Parse(time.RFC3339, item.DueDate)
			var start *time.Time
			if item.StartDate != "" {
				parsed, _ := time.Parse(time.RFC3339, item.StartDate)
				start = &parsed
			}
			description, _ := json.Marshal([]map[string]any{
				{"type": "paragraph", "content": []map[string]any{{"type": "text", "text": item.Requirements, "styles": map[string]any{}}}},
				{"type": "paragraph", "content": []map[string]any{{"type": "text", "text": item.Criteria, "styles": map[string]any{}}}},
			})
			customFields, _ := json.Marshal(map[string]string{
				"draft_due_at":   item.DueDate,
				"draft_start_at": item.StartDate,
			})
			taskID := uuid.New()
			taskIDs[item.ID] = taskID
			var number int64
			if err := tx.GetContext(ctx, &number, `UPDATE task_counters SET last_value=last_value+1 WHERE project_id=$1 RETURNING last_value`, projectID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO tasks(id,project_id,task_number,task_type_id,status_id,title,description,importance,reporter_id,custom_fields,start_date,due_date,tags,created_at,updated_at,creator_user_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'[]',$13,$13,$14)`, taskID, projectID, number, taskTypeID, taskStatusID, item.Title, description, item.Importance, members[user], customFields, start, due, now, user); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO task_assignees(task_id,member_id) VALUES($1,$2)`, taskID, members[assignee]); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO task_activities(id,task_id,actor_id,activity_type,content,created_at,updated_at) VALUES($1,$2,$3,'task.created','{}',$4,$4)`, uuid.New(), taskID, members[user], now); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO notifications(id,recipient_user_id,actor_member_id,type,task_id,project_id,created_at,source_event_id) VALUES($1,$2,$3,'assigned',$4,$5,$6,$7)`, uuid.New(), assignee, members[user], taskID, projectID, now, publication.String()+":"+taskID.String()); err != nil {
				return err
			}
		}
		for _, item := range d.Items {
			target := taskIDs[item.ID]
			for _, dependency := range item.Dependencies {
				if source := taskIDs[dependency]; source != uuid.Nil {
					if _, err := tx.ExecContext(ctx, `INSERT INTO task_links(id,source_task_id,target_task_id,link_type,created_by,created_at) VALUES($1,$2,$3,'blocks',$4,$5)`, uuid.New(), source, target, user, now); err != nil {
						return err
					}
				}
			}
		}
		if err := r.createProjectDraftMaterialReferences(ctx, tx, d, projectID, taskIDs, members, user, now); err != nil {
			return err
		}
		result = &draftdom.ProjectDraftPublication{PublicationID: publication, ProjectID: projectID, TaskIDs: taskIDs}
		receipt, err := json.Marshal(result)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE project_drafts SET state='published',publication_id=$2,published_project_id=$3,publication_receipt=$4,updated_at=$5 WHERE id=$1`, id, publication, projectID, receipt, now); err != nil {
			return err
		}
		return nil
	})
	return result, err
}

func (r *DraftRepository) createProjectDraftMaterialReferences(ctx context.Context, tx *sqlx.Tx, d *draftdom.ProjectDraft, projectID uuid.UUID, taskIDs map[string]uuid.UUID, members map[uuid.UUID]uuid.UUID, owner uuid.UUID, now time.Time) error {
	txctx := database.WithTransaction(ctx, tx)
	for _, material := range d.Materials {
		if material.Kind != "document" || material.SourceType != "internal" {
			return &draftdom.ProjectDraftValidationError{Message: "only internal documents can be published as references"}
		}
		sourceID, err := uuid.Parse(material.ID)
		if err != nil {
			return &draftdom.ProjectDraftValidationError{Message: "material source is invalid"}
		}
		users := map[uuid.UUID]struct{}{}
		if len(material.TaskIDs) == 0 {
			for _, participant := range d.Participants {
				participantID, parseErr := uuid.Parse(participant.UserID)
				if parseErr == nil {
					users[participantID] = struct{}{}
				}
			}
		} else {
			for _, item := range d.Items {
				for _, taskID := range material.TaskIDs {
					if taskID == item.ID {
						participantID, parseErr := uuid.Parse(item.AssigneeUserID)
						if parseErr == nil {
							users[participantID] = struct{}{}
						}
					}
				}
			}
		}
		users[owner] = struct{}{}
		var title string
		for user := range users {
			actorCtx := authz.WithActorID(txctx, user)
			args := []any{sourceID}
			predicate, predicateErr := DocumentVisibilityPredicate(actorCtx, r.db, "documents", &args, docdom.AccessRead)
			if predicateErr != nil {
				return predicateErr
			}
			var source struct {
				ID    uuid.UUID `db:"id"`
				Title string    `db:"title"`
			}
			if err := tx.GetContext(actorCtx, &source, `SELECT id,title FROM documents WHERE id=$1 AND source_document_id IS NULL AND deleted_at IS NULL AND `+predicate, args...); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return &draftdom.ProjectDraftValidationError{Message: "material source is no longer readable"}
				}
				return err
			}
			title = source.Title
		}
		visibility := "project"
		if len(material.TaskIDs) > 0 {
			visibility = "task"
		}
		referenceID := uuid.New()
		memberID := members[owner]
		if _, err := tx.ExecContext(txctx, `INSERT INTO documents(id,project_id,title,content,visibility,owner_user_id,source_document_id,access_version,created_by,updated_by,created_at,updated_at) VALUES($1,$2,$3,NULL,$4,$5,$6,1,$7,$7,$8,$8)`, referenceID, projectID, title, visibility, owner, sourceID, memberID, now); err != nil {
			return err
		}
		for _, itemID := range material.TaskIDs {
			taskID := taskIDs[itemID]
			if taskID == uuid.Nil {
				return &draftdom.ProjectDraftValidationError{Message: "material task association is invalid"}
			}
			if _, err := tx.ExecContext(txctx, `INSERT INTO document_tasks(document_id,task_id) VALUES($1,$2)`, referenceID, taskID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *DraftRepository) UpdateProjectDraftFile(ctx context.Context, draftID, fileID uuid.UUID, uploadStatus, processingStatus, processingError string, extractedText *string) (*draftdom.ProjectDraftFile, error) {
	user, err := projectDraftActor(ctx)
	if err != nil {
		return nil, err
	}
	var out *draftdom.ProjectDraftFile
	err = WithTx(ctx, r.db, func(tx *sqlx.Tx) error {
		now := time.Now()
		var state string
		if err := tx.GetContext(ctx, &state, `SELECT state FROM project_drafts WHERE id=$1 AND owner_user_id=$2 FOR UPDATE`, draftID, user); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return draftdom.ErrNotFound
			}
			return err
		}
		if state != "editable" {
			return draftdom.ErrConflict
		}
		if _, err := tx.ExecContext(ctx, `UPDATE files f SET upload_status=COALESCE(NULLIF($3,''),f.upload_status),multipart_upload_id=NULL,updated_at=$4 FROM project_draft_files p WHERE p.file_id=f.id AND p.draft_id=$1 AND p.file_id=$2 AND p.owner_user_id=$5`, draftID, fileID, uploadStatus, now, user); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE project_draft_files SET processing_status=$3,processing_error=NULLIF($4,''),extracted_text=$5,updated_at=$6 WHERE draft_id=$1 AND file_id=$2 AND owner_user_id=$7`, draftID, fileID, processingStatus, processingError, extractedText, now, user); err != nil {
			return err
		}
		var row projectDraftFileRow
		if err := tx.GetContext(ctx, &row, projectDraftFileSelect+` WHERE p.draft_id=$1 AND p.file_id=$2 AND p.owner_user_id=$3`, draftID, fileID, user); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return draftdom.ErrNotFound
			}
			return err
		}
		out = projectDraftFileFromRow(row)
		return nil
	})
	return out, err
}

func (r *DraftRepository) UpdateProjectDraft(ctx context.Context, id uuid.UUID, expected int, content draftdom.ProjectDraftContent) (*draftdom.ProjectDraft, error) {
	user, err := projectDraftActor(ctx)
	if err != nil {
		return nil, err
	}
	content.Normalize()
	if err := content.ValidateStructure(); err != nil {
		return nil, err
	}
	var result *draftdom.ProjectDraft
	err = WithTx(ctx, r.db, func(tx *sqlx.Tx) error {
		var row projectDraftRow
		if err := tx.GetContext(ctx, &row, projectDraftSelect+` WHERE id=$1 AND owner_user_id=$2 FOR UPDATE`, id, user); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return draftdom.ErrNotFound
			}
			return err
		}
		if row.State != "editable" || row.Version != expected {
			return draftdom.ErrConflict
		}
		b, err := encodeProjectDraft(content)
		if err != nil {
			return err
		}
		generation := row.Generation
		if len(generation) > 0 {
			var current draftdom.ProjectDraftGeneration
			if err := json.Unmarshal(generation, &current); err != nil {
				return err
			}
			if current.Status == "starting" || current.Status == "running" {
				current.Status = "failed"
				current.Error = "草稿已被人工修改，本轮生成结果已失效"
				generation, err = encodeProjectDraftGeneration(&current)
				if err != nil {
					return err
				}
			}
		}
		now := time.Now()
		if _, err = tx.ExecContext(ctx, `UPDATE project_drafts SET version=version+1,content=$2,generation=$3,updated_at=$4 WHERE id=$1 AND owner_user_id=$5`, id, b, generation, now, user); err != nil {
			return err
		}
		row.Version++
		row.Content, row.Generation, row.UpdatedAt = b, generation, now
		result, err = decodeProjectDraft(row)
		return err
	})
	return result, err
}

// StartProjectDraftGeneration reserves one generation for the current draft
// version. It does not change the draft version, so human edits can continue;
// import will reject the result if an edit wins the version race first.
func (r *DraftRepository) StartProjectDraftGeneration(ctx context.Context, id uuid.UUID, expected int, agentID uuid.UUID, baseEventIndex int) (*draftdom.ProjectDraft, error) {
	user, err := projectDraftActor(ctx)
	if err != nil {
		return nil, err
	}
	var result *draftdom.ProjectDraft
	err = WithTx(ctx, r.db, func(tx *sqlx.Tx) error {
		var row projectDraftRow
		if err := tx.GetContext(ctx, &row, projectDraftSelect+` WHERE id=$1 AND owner_user_id=$2 FOR UPDATE`, id, user); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return draftdom.ErrNotFound
			}
			return err
		}
		if row.State != "editable" || row.Version != expected {
			return draftdom.ErrConflict
		}
		if len(row.Generation) > 0 {
			var current draftdom.ProjectDraftGeneration
			if err := json.Unmarshal(row.Generation, &current); err != nil {
				return err
			}
			if current.Status == "starting" || current.Status == "running" {
				return draftdom.ErrConflict
			}
		}
		generation := &draftdom.ProjectDraftGeneration{ID: uuid.New(), Version: expected, AgentID: agentID, Status: "starting", BaseEventIndex: baseEventIndex}
		b, err := encodeProjectDraftGeneration(generation)
		if err != nil {
			return err
		}
		now := time.Now()
		if _, err = tx.ExecContext(ctx, `UPDATE project_drafts SET generation=$2,updated_at=$3 WHERE id=$1 AND owner_user_id=$4`, id, b, now, user); err != nil {
			return err
		}
		row.Generation, row.UpdatedAt = b, now
		result, err = decodeProjectDraft(row)
		return err
	})
	return result, err
}

func (r *DraftRepository) BindProjectDraftGeneration(ctx context.Context, id, generationID, conversationID uuid.UUID) (*draftdom.ProjectDraft, error) {
	user, err := projectDraftActor(ctx)
	if err != nil {
		return nil, err
	}
	var result *draftdom.ProjectDraft
	err = WithTx(ctx, r.db, func(tx *sqlx.Tx) error {
		var row projectDraftRow
		if err := tx.GetContext(ctx, &row, projectDraftSelect+` WHERE id=$1 AND owner_user_id=$2 FOR UPDATE`, id, user); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return draftdom.ErrNotFound
			}
			return err
		}
		var generation draftdom.ProjectDraftGeneration
		if len(row.Generation) == 0 || json.Unmarshal(row.Generation, &generation) != nil || generation.ID != generationID || generation.Status != "starting" {
			return draftdom.ErrConflict
		}
		generation.ConversationID = &conversationID
		generation.Status = "running"
		b, err := encodeProjectDraftGeneration(&generation)
		if err != nil {
			return err
		}
		now := time.Now()
		if _, err = tx.ExecContext(ctx, `UPDATE project_drafts SET generation=$2,updated_at=$3 WHERE id=$1 AND owner_user_id=$4`, id, b, now, user); err != nil {
			return err
		}
		row.Generation, row.UpdatedAt = b, now
		result, err = decodeProjectDraft(row)
		return err
	})
	return result, err
}

func (r *DraftRepository) FailProjectDraftGeneration(ctx context.Context, id, generationID uuid.UUID, message string) (*draftdom.ProjectDraft, error) {
	user, err := projectDraftActor(ctx)
	if err != nil {
		return nil, err
	}
	var result *draftdom.ProjectDraft
	err = WithTx(ctx, r.db, func(tx *sqlx.Tx) error {
		var row projectDraftRow
		if err := tx.GetContext(ctx, &row, projectDraftSelect+` WHERE id=$1 AND owner_user_id=$2 FOR UPDATE`, id, user); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return draftdom.ErrNotFound
			}
			return err
		}
		var generation draftdom.ProjectDraftGeneration
		if len(row.Generation) == 0 || json.Unmarshal(row.Generation, &generation) != nil || generation.ID != generationID || (generation.Status != "starting" && generation.Status != "running") {
			return draftdom.ErrConflict
		}
		generation.Status, generation.Error = "failed", message
		b, err := encodeProjectDraftGeneration(&generation)
		if err != nil {
			return err
		}
		now := time.Now()
		if _, err = tx.ExecContext(ctx, `UPDATE project_drafts SET generation=$2,updated_at=$3 WHERE id=$1 AND owner_user_id=$4`, id, b, now, user); err != nil {
			return err
		}
		row.Generation, row.UpdatedAt = b, now
		result, err = decodeProjectDraft(row)
		return err
	})
	return result, err
}

// CompleteProjectDraftGeneration atomically imports a validated result. A
// retry of the same request returns the already imported row without writing
// another version.
func (r *DraftRepository) CompleteProjectDraftGeneration(ctx context.Context, id, generationID uuid.UUID, expected int, content draftdom.ProjectDraftContent) (*draftdom.ProjectDraft, error) {
	user, err := projectDraftActor(ctx)
	if err != nil {
		return nil, err
	}
	content.Normalize()
	if err := content.ValidateStructure(); err != nil {
		return nil, err
	}
	bContent, err := encodeProjectDraft(content)
	if err != nil {
		return nil, err
	}
	var result *draftdom.ProjectDraft
	err = WithTx(ctx, r.db, func(tx *sqlx.Tx) error {
		var row projectDraftRow
		if err := tx.GetContext(ctx, &row, projectDraftSelect+` WHERE id=$1 AND owner_user_id=$2 FOR UPDATE`, id, user); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return draftdom.ErrNotFound
			}
			return err
		}
		var generation draftdom.ProjectDraftGeneration
		if len(row.Generation) == 0 || json.Unmarshal(row.Generation, &generation) != nil || generation.ID != generationID || generation.Version != expected {
			return draftdom.ErrConflict
		}
		if generation.Status == "imported" && row.Version == expected+1 {
			result, err = decodeProjectDraft(row)
			return err
		}
		if row.State != "editable" || row.Version != expected || generation.Status != "running" || generation.ConversationID == nil {
			return draftdom.ErrConflict
		}
		generation.Status = "imported"
		bGeneration, err := encodeProjectDraftGeneration(&generation)
		if err != nil {
			return err
		}
		now := time.Now()
		if _, err = tx.ExecContext(ctx, `UPDATE project_drafts SET version=version+1,content=$2,generation=$3,updated_at=$4 WHERE id=$1 AND owner_user_id=$5 AND version=$6`, id, bContent, bGeneration, now, user, expected); err != nil {
			return err
		}
		row.Version++
		row.Content, row.Generation, row.UpdatedAt = bContent, bGeneration, now
		result, err = decodeProjectDraft(row)
		return err
	})
	return result, err
}
