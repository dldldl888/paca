package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	attachmentdom "github.com/Paca-AI/api/internal/domain/attachment"
	draftdom "github.com/Paca-AI/api/internal/domain/draft"
	"github.com/Paca-AI/api/internal/platform/authz"
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
	ID            uuid.UUID `db:"id"`
	OwnerUserID   uuid.UUID `db:"owner_user_id"`
	ChatSessionID uuid.UUID `db:"chat_session_id"`
	Version       int       `db:"version"`
	State         string    `db:"state"`
	Content       []byte    `db:"content"`
	Generation    []byte    `db:"generation"`
	CreatedAt     time.Time `db:"created_at"`
	UpdatedAt     time.Time `db:"updated_at"`
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
	return &draftdom.ProjectDraft{
		ProjectDraftContent: content,
		ID:                  row.ID,
		OwnerUserID:         row.OwnerUserID,
		ChatSessionID:       row.ChatSessionID,
		Version:             row.Version,
		State:               row.State,
		Generation:          generation,
		CreatedAt:           row.CreatedAt,
		UpdatedAt:           row.UpdatedAt,
	}, nil
}

func encodeProjectDraftGeneration(g *draftdom.ProjectDraftGeneration) ([]byte, error) {
	if g == nil {
		return nil, nil
	}
	return json.Marshal(g)
}

const projectDraftSelect = `SELECT id,owner_user_id,chat_session_id,version,state,content,generation,created_at,updated_at FROM project_drafts`

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

func (r *DraftRepository) CreateProjectDraftFile(ctx context.Context, draftID uuid.UUID, f *attachmentdom.File) (*draftdom.ProjectDraftFile, error) {
	user, err := projectDraftActor(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var out *draftdom.ProjectDraftFile
	err = WithTx(ctx, r.db, func(tx *sqlx.Tx) error {
		var exists uuid.UUID
		if err := tx.GetContext(ctx, &exists, `SELECT id FROM project_drafts WHERE id=$1 AND owner_user_id=$2`, draftID, user); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return draftdom.ErrNotFound
			}
			return err
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

func (r *DraftRepository) UpdateProjectDraftFile(ctx context.Context, draftID, fileID uuid.UUID, uploadStatus, processingStatus, processingError string, extractedText *string) (*draftdom.ProjectDraftFile, error) {
	user, err := projectDraftActor(ctx)
	if err != nil {
		return nil, err
	}
	var out *draftdom.ProjectDraftFile
	err = WithTx(ctx, r.db, func(tx *sqlx.Tx) error {
		now := time.Now()
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
