package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	taskdom "github.com/Paca-AI/api/internal/domain/task"
	"strings"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/Paca-AI/api/internal/platform/authz"
)

// AuthzPermissionStore resolves effective permissions from persisted roles.
type AuthzPermissionStore struct {
	db sqlx.QueryerContext
}

// NewAuthzPermissionStore returns a new permission store.
func NewAuthzPermissionStore(db sqlx.QueryerContext) *AuthzPermissionStore {
	return &AuthzPermissionStore{db: db}
}

// ListGlobalPermissions returns permissions granted by the user's global role (via users.role_id).
func (s *AuthzPermissionStore) ListGlobalPermissions(ctx context.Context, userID uuid.UUID) ([]authz.Permission, error) {
	var rows []struct {
		Permissions []byte `db:"permissions"`
	}
	err := sqlx.SelectContext(ctx, s.db, &rows, `
		SELECT gr.permissions
		FROM global_roles gr
		JOIN users u ON u.role_id = gr.id
		WHERE u.id = $1 AND u.deleted_at IS NULL`, userID.String())
	if err != nil {
		return nil, fmt.Errorf("authz store: list global permissions: %w", err)
	}

	return collectPermissions(rows), nil
}

// ListProjectPermissions returns permissions from project role memberships for
// the provided project.
func (s *AuthzPermissionStore) ListProjectPermissions(ctx context.Context, userID, projectID uuid.UUID) ([]authz.Permission, error) {
	var rows []struct {
		Permissions []byte `db:"permissions"`
	}
	err := sqlx.SelectContext(ctx, s.db, &rows, `
		SELECT pr.permissions
		FROM project_roles pr
		JOIN project_members pm ON pm.project_role_id = pr.id
		JOIN users u ON u.id = pm.user_id AND u.deleted_at IS NULL
        JOIN projects p ON p.id = pm.project_id AND p.deleted_at IS NULL
        WHERE pm.user_id = $1 AND pm.project_id = $2 AND pm.deleted_at IS NULL AND pr.project_id = pm.project_id`, userID.String(), projectID.String())
	if err != nil {
		return nil, fmt.Errorf("authz store: list project permissions: %w", err)
	}

	return collectPermissions(rows), nil
}

func collectPermissions(rows []struct {
	Permissions []byte `db:"permissions"`
}) []authz.Permission {
	seen := map[authz.Permission]struct{}{}
	out := make([]authz.Permission, 0)
	for _, row := range rows {
		for _, p := range permissionsFromJSON(row.Permissions) {
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			out = append(out, p)
		}
	}
	return out
}

func permissionsFromJSON(raw []byte) []authz.Permission {
	if len(raw) == 0 {
		return nil
	}

	var payload any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}

	seen := map[authz.Permission]struct{}{}
	out := make([]authz.Permission, 0)

	add := func(k string) {
		k = strings.TrimSpace(k)
		if k == "" {
			return
		}
		p := authz.Permission(k)
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}

	switch v := payload.(type) {
	case map[string]any:
		for key, enabled := range v {
			switch e := enabled.(type) {
			case bool:
				if e {
					add(key)
				}
			case float64:
				if e != 0 {
					add(key)
				}
			case string:
				if strings.EqualFold(e, "true") {
					add(key)
				}
			}
		}
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				add(s)
			}
		}
	}

	return out
}

// FindTaskActorAccess reads live identity and qualification, independent of JWT role claims.
func (s *AuthzPermissionStore) FindTaskActorAccess(ctx context.Context, userID, projectID uuid.UUID) (authz.TaskActorAccess, error) {
	var row struct {
		UserID   uuid.UUID  `db:"user_id"`
		MemberID *uuid.UUID `db:"member_id"`
		Global   []byte     `db:"global_permissions"`
		Project  []byte     `db:"project_permissions"`
	}
	err := sqlx.GetContext(ctx, s.db, &row, `SELECT u.id AS user_id, pm.id AS member_id,
 gr.permissions AS global_permissions, COALESCE(pr.permissions, '{}'::jsonb) AS project_permissions
 FROM users u JOIN global_roles gr ON gr.id = u.role_id
 JOIN projects p ON p.id = $2 AND p.deleted_at IS NULL
 LEFT JOIN project_members pm ON pm.project_id = p.id AND pm.user_id = u.id
 AND pm.member_type = 'human' AND pm.deleted_at IS NULL
 LEFT JOIN project_roles pr ON pr.id = pm.project_role_id AND pr.project_id = p.id
 WHERE u.id = $1 AND u.deleted_at IS NULL`, userID, projectID)
	if errors.Is(err, sql.ErrNoRows) {
		return authz.TaskActorAccess{}, taskdom.ErrTaskNotFound
	}
	if err != nil {
		return authz.TaskActorAccess{}, fmt.Errorf("authz store: task access: %w", err)
	}
	return authz.TaskActorAccess{UserID: row.UserID, MemberID: row.MemberID, GlobalPermissions: permissionsFromJSON(row.Global), ProjectPermissions: permissionsFromJSON(row.Project)}, nil
}
