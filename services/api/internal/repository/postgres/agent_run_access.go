package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	agentdom "github.com/Paca-AI/api/internal/domain/agent"
	docdom "github.com/Paca-AI/api/internal/domain/doc"
	taskdom "github.com/Paca-AI/api/internal/domain/task"
	"github.com/Paca-AI/api/internal/platform/database"
	"github.com/google/uuid"
	"strings"
)

// CurrentChatActor resolves current human membership without accepting actor attribution.
// A zero member selects the user's current membership (global chat has no member).
func (r *AgentRepository) CurrentChatActor(ctx context.Context, project, member, user uuid.UUID) (uuid.UUID, error) {
	var owner uuid.UUID
	var err error
	if project == uuid.Nil {
		if member != uuid.Nil || user == uuid.Nil {
			return owner, agentdom.ErrConversationNotFound
		}
		err = DeliveryQuery(ctx, r.db).GetContext(ctx, &owner, `SELECT id FROM users WHERE id=$1 AND deleted_at IS NULL`, user)
	} else {
		err = DeliveryQuery(ctx, r.db).GetContext(ctx, &owner, `SELECT u.id FROM project_members pm
 JOIN users u ON u.id=pm.user_id AND u.deleted_at IS NULL
 JOIN projects p ON p.id=pm.project_id AND p.deleted_at IS NULL
 WHERE pm.project_id=$1 AND pm.member_type='human' AND pm.deleted_at IS NULL
 AND ($2::uuid='00000000-0000-0000-0000-000000000000' OR pm.id=$2)
 AND ($3::uuid='00000000-0000-0000-0000-000000000000' OR u.id=$3)`, project, member, user)
	}
	if errors.Is(err, sql.ErrNoRows) {
		err = agentdom.ErrConversationNotFound
	}
	return owner, err
}

// MergeConversationContext uses the row's latest value under PostgreSQL's UPDATE
// lock. A stale caller cannot drop references or certify legacy NULL metadata.
func (r *AgentRepository) MergeConversationContext(ctx context.Context, id uuid.UUID, refs []agentdom.ContextItemRef) error {
	if refs == nil {
		refs = []agentdom.ContextItemRef{}
	}
	b, err := json.Marshal(refs)
	if err != nil {
		return err
	}
	result, err := DeliveryQuery(ctx, r.db).ExecContext(ctx, `UPDATE agent_conversations SET context_items=
 (SELECT COALESCE(jsonb_agg(DISTINCT ref),'[]'::jsonb) FROM jsonb_array_elements(context_items || $2::jsonb) ref),
 updated_at=now() WHERE id=$1 AND context_items IS NOT NULL`, id, b)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return agentdom.ErrConversationNotFound
	}
	return err
}

// BeginConversationTurn serializes the conservative prior-turn latch. The caller
// has authorized current scope; Task5 will fence permission writers around it.
func (r *AgentRepository) BeginConversationTurn(ctx context.Context, id, agentID uuid.UUID, begin bool) (bool, error) {
	tx := database.Transaction(ctx)
	own := tx == nil
	var err error
	if own {
		tx, err = r.db.BeginTxx(ctx, nil)
	}
	if err != nil {
		return false, err
	}
	if own {
		defer tx.Rollback()
	}
	var prior bool
	err = tx.GetContext(ctx, &prior, `SELECT started_at IS NOT NULL OR EXISTS
 (SELECT 1 FROM agent_conversation_events WHERE conversation_id=c.id)
 FROM agent_conversations c WHERE id=$1 AND agent_id=$2 AND (NOT $3 OR status='running') FOR UPDATE`, id, agentID, begin)
	if errors.Is(err, sql.ErrNoRows) {
		err = agentdom.ErrConversationNotFound
	}
	if err != nil {
		return false, err
	}
	if begin {
		if _, err = tx.ExecContext(ctx, `UPDATE agent_conversations SET started_at=COALESCE(started_at,now()) WHERE id=$1`, id); err != nil {
			return false, err
		}
	}
	if own {
		return prior, tx.Commit()
	}
	return prior, nil
}

func (r *AgentRepository) ListDeliveryUsers(ctx context.Context) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	err := DeliveryQuery(ctx, r.db).SelectContext(ctx, &ids, `SELECT id FROM users WHERE deleted_at IS NULL`)
	return ids, err
}
func (r *AgentRepository) ListPrivateRunIDs(ctx context.Context) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	err := DeliveryQuery(ctx, r.db).SelectContext(ctx, &ids, `SELECT id FROM agent_conversations WHERE audience='owner_private' AND status IN ('running','queued','paused')`)
	return ids, err
}
func (r *AgentRepository) FailRevokedRun(ctx context.Context, id uuid.UUID) error {
	_, err := DeliveryQuery(ctx, r.db).ExecContext(ctx, `UPDATE agent_conversations SET status='failed',error_message='Authorization revoked',finished_at=now(),updated_at=now() WHERE id=$1 AND status IN ('running','queued','paused')`, id)
	if err != nil {
		return err
	}
	_, err = DeliveryQuery(ctx, r.db).ExecContext(ctx, `DELETE FROM agent_pending_triggers WHERE conversation_id=$1`, id)
	return err
}
func (r *AgentRepository) DeliveryActivityVisible(ctx context.Context, kind string, content []byte, scope taskdom.AccessScope) (bool, error) {
	b := &queryBuilder{idx: 3, args: []any{kind, string(content)}}
	applyTaskVisibility(b, []taskdom.AccessScope{scope})
	visible := strings.Join(b.whereClauses, " AND ")
	var ok bool
	err := DeliveryQuery(ctx, r.db).GetContext(ctx, &ok, `SELECT EXISTS(SELECT 1 FROM (SELECT $1::text AS activity_type,$2::jsonb AS content) ta WHERE true`+taskActivityReferencesVisible(visible)+`)`, b.args...)
	return ok, err
}

func (r *AgentRepository) PrivateHistoryOwner(ctx context.Context, id uuid.UUID) (uuid.UUID, error) {
	var owner uuid.UUID
	err := DeliveryQuery(ctx, r.db).GetContext(ctx, &owner, `SELECT u.id FROM agent_conversations c
 LEFT JOIN agent_chat_sessions s ON s.id=c.chat_session_id AND s.agent_id=c.agent_id AND s.project_id IS NOT DISTINCT FROM c.project_id
 LEFT JOIN project_members pm ON pm.id=s.member_id AND pm.project_id=c.project_id AND pm.member_type='human'
 JOIN users u ON u.id=CASE WHEN c.project_id IS NULL AND c.actor_user_id IS NOT NULL THEN c.actor_user_id ELSE pm.user_id END AND u.deleted_at IS NULL
 WHERE c.id=$1 AND c.audience='owner_private' AND c.status='failed' AND c.error_message='Authorization revoked'`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, agentdom.ErrConversationNotFound
	}
	return owner, err
}

// Selection is intentionally limited to private projects with human membership.
// The service filters docs.read before applying any document count/content limit.
func (r *AgentRepository) ListCompanyProjects(ctx context.Context, owner uuid.UUID) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	err := DeliveryQuery(ctx, r.db).SelectContext(ctx, &ids, `SELECT DISTINCT p.id FROM projects p
 JOIN project_members pm ON pm.project_id=p.id AND pm.member_type='human' AND pm.deleted_at IS NULL
 JOIN users u ON u.id=pm.user_id AND u.deleted_at IS NULL
 JOIN doc_folders f ON f.project_id=p.id AND f.parent_id IS NULL AND f.name='公司资料'
 WHERE p.deleted_at IS NULL AND p.is_public=false AND u.id=$1 ORDER BY p.id`, owner)
	return ids, err
}
func (r *AgentRepository) ListCompanyDocuments(ctx context.Context, projects []uuid.UUID, limit int) ([]*docdom.Document, error) {
	out := []*docdom.Document{}
	if len(projects) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(projects)+1)
	placeholders := make([]string, len(projects))
	for i, p := range projects {
		args = append(args, p)
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}
	args = append(args, limit)
	var records []documentRecord
	err := DeliveryQuery(ctx, r.db).SelectContext(ctx, &records, `SELECT `+documentCols+` FROM documents WHERE deleted_at IS NULL
 AND project_id IN (`+strings.Join(placeholders, ",")+`) AND folder_id IN
 (SELECT id FROM doc_folders WHERE project_id=documents.project_id AND parent_id IS NULL AND name='公司资料')
 ORDER BY updated_at DESC,id ASC LIMIT `+fmt.Sprintf("$%d", len(args)), args...)
	if err != nil {
		return nil, err
	}
	for _, rec := range records {
		out = append(out, documentFromRecord(rec))
	}
	return out, nil
}
