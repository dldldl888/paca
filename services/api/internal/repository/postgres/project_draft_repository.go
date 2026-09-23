package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	draftdom "github.com/Paca-AI/api/internal/domain/draft"
	"github.com/Paca-AI/api/internal/platform/authz"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

type projectDraftRow struct {
	ID            uuid.UUID `db:"id"`
	OwnerUserID   uuid.UUID `db:"owner_user_id"`
	ChatSessionID uuid.UUID `db:"chat_session_id"`
	Version       int       `db:"version"`
	State         string    `db:"state"`
	Content       []byte    `db:"content"`
	CreatedAt     time.Time `db:"created_at"`
	UpdatedAt     time.Time `db:"updated_at"`
}

func decodeProjectDraft(row projectDraftRow) (*draftdom.ProjectDraft, error) {
	var content draftdom.ProjectDraftContent
	if err := json.Unmarshal(row.Content, &content); err != nil {
		return nil, err
	}
	content.Normalize()
	return &draftdom.ProjectDraft{
		ProjectDraftContent: content,
		ID:                  row.ID,
		OwnerUserID:         row.OwnerUserID,
		ChatSessionID:       row.ChatSessionID,
		Version:             row.Version,
		State:               row.State,
		CreatedAt:           row.CreatedAt,
		UpdatedAt:           row.UpdatedAt,
	}, nil
}

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
		_, err := tx.ExecContext(ctx, `INSERT INTO project_drafts(id,owner_user_id,chat_session_id,version,state,content,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, d.ID, user, chatSessionID, d.Version, d.State, b, now, now)
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
		return tx.GetContext(ctx, &row, `SELECT id,owner_user_id,chat_session_id,version,state,content,created_at,updated_at FROM project_drafts WHERE id=$1 AND owner_user_id=$2`, id, user)
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
		return tx.SelectContext(ctx, &rows, `SELECT id,owner_user_id,chat_session_id,version,state,content,created_at,updated_at FROM project_drafts WHERE owner_user_id=$1 ORDER BY updated_at DESC,id DESC LIMIT 100`, user)
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
		if err := tx.GetContext(ctx, &row, `SELECT id,owner_user_id,chat_session_id,version,state,content,created_at,updated_at FROM project_drafts WHERE id=$1 AND owner_user_id=$2 FOR UPDATE`, id, user); err != nil {
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
		now := time.Now()
		if _, err = tx.ExecContext(ctx, `UPDATE project_drafts SET version=version+1,content=$2,updated_at=$3 WHERE id=$1 AND owner_user_id=$4`, id, b, now, user); err != nil {
			return err
		}
		row.Version++
		row.Content = b
		row.UpdatedAt = now
		result, err = decodeProjectDraft(row)
		return err
	})
	return result, err
}
