package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Paca-AI/api/internal/apierr"
	docdom "github.com/Paca-AI/api/internal/domain/doc"
	taskdom "github.com/Paca-AI/api/internal/domain/task"
	"github.com/Paca-AI/api/internal/platform/authz"
	"github.com/Paca-AI/api/internal/platform/database"
	tasksvc "github.com/Paca-AI/api/internal/service/task"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// DocumentVisibilityPredicate is the shared current-object SQL policy. Callers
// must apply it before ORDER/LIMIT, including company context and activity unions.
// It checks one canonical source using the very same object predicate.
func DocumentVisibilityPredicate(ctx context.Context, db *sqlx.DB, alias string, args *[]any, action docdom.AccessAction) (string, error) {
	object, err := documentObjectPredicate(ctx, db, args, action)
	if err != nil {
		return "", err
	}
	source := object
	if action == docdom.AccessWrite {
		source, err = documentObjectPredicate(ctx, db, args, docdom.AccessRead)
		if err != nil {
			return "", err
		}
	}
	qualify := func(s, a string) string { return strings.ReplaceAll(s, "%s", a) }
	return "(" + alias + ".deleted_at IS NULL AND (" + qualify(object, alias) + ") AND (" + alias + ".source_document_id IS NULL OR EXISTS (SELECT 1 FROM documents canonical WHERE canonical.id=" + alias + ".source_document_id AND canonical.deleted_at IS NULL AND canonical.source_document_id IS NULL AND (" + qualify(source, "canonical") + "))))", nil
}
func documentObjectPredicate(ctx context.Context, db *sqlx.DB, args *[]any, action docdom.AccessAction) (string, error) {
	store := NewAuthzPermissionStore(db)
	authorizer := authz.NewAuthorizer(store).WithAgentRoleResolver(store)
	user, authenticated := authz.ActorIDFromContext(ctx)
	agent, isAgent := authz.AgentIDFromContext(ctx)
	if isAgent {
		var active bool
		if err := DeliveryQuery(ctx, db).GetContext(ctx, &active, `SELECT EXISTS(SELECT 1 FROM agents WHERE id=$1 AND deleted_at IS NULL)`, agent); err != nil {
			return "", err
		}
		if !active {
			return "FALSE", nil
		}
	}
	human := authenticated && !isAgent && !authz.IsAgentCredential(ctx)
	var projects []struct {
		ID     uuid.UUID `db:"id"`
		Public bool      `db:"is_public"`
	}
	// ponytail: policy resolution is O(projects); restrict to target/source project IDs if measured scale requires it.
	if err := DeliveryQuery(ctx, db).SelectContext(ctx, &projects, `SELECT id,is_public FROM projects WHERE deleted_at IS NULL`); err != nil {
		return "", err
	}
	bind := func(v any) string { *args = append(*args, v); return fmt.Sprintf("$%d", len(*args)) }
	var branches []string
	for _, p := range projects {
		legacy, read, write, globalAll := false, false, false, false
		member := false
		if authenticated || isAgent {
			var err error
			if isAgent {
				read, err = authorizer.HasPermissionsForAgent(ctx, agent, p.ID, authz.PermissionDocsRead)
				if err != nil {
					return "", err
				}
				write, err = authorizer.HasPermissionsForAgent(ctx, agent, p.ID, authz.PermissionDocsWrite)
				if err != nil {
					return "", err
				}
				globalRead, e := authorizer.HasGlobalPermissionsForAgent(ctx, agent, authz.PermissionProjectsRead)
				if e != nil {
					return "", e
				}
				legacy = read || globalRead
				member = read
			} else {
				actor, err := store.FindTaskActorAccess(ctx, user, p.ID)
				if err != nil {
					if errors.Is(err, taskdom.ErrTaskNotFound) {
						continue
					}
					return "", err
				}
				member = actor.MemberID != nil
				globalAll, err = authorizer.HasPermissions(ctx, user, nil, "", authz.PermissionAll)
				if err != nil {
					return "", err
				}
				read, err = authorizer.HasPermissions(ctx, user, &p.ID, "", authz.PermissionDocsRead)
				if err != nil {
					return "", err
				}
				write, err = authorizer.HasPermissions(ctx, user, &p.ID, "", authz.PermissionDocsWrite)
				if err != nil {
					return "", err
				}
				globalRead, e := authorizer.HasPermissions(ctx, user, nil, "", authz.PermissionProjectsRead)
				if e != nil {
					return "", e
				}
				legacy = read || globalRead
			}
		} else {
			legacy = p.Public
		}
		if action == docdom.AccessWrite {
			legacy = write
			read = read && write
		}
		var clauses []string
		if legacy {
			clauses = append(clauses, "%s.visibility='legacy_project'")
		}
		if read && ((human && (member || globalAll)) || (isAgent && member)) {
			clauses = append(clauses, "%s.visibility='project'")
			if globalAll && human {
				clauses = append(clauses, "%s.visibility='private'")
			} else if human {
				clauses = append(clauses, "(%s.visibility='private' AND %s.owner_user_id="+bind(user)+")")
			}
			scope, e := authz.NewTaskAccessPolicy(store).ResolveTaskAccess(ctx, p.ID)
			if e == nil {
				taskPred := taskVisibilitySQL(scope, "dtask", args)
				clauses = append(clauses, "(%s.visibility='task' AND EXISTS (SELECT 1 FROM document_tasks link JOIN tasks dtask ON dtask.id=link.task_id WHERE link.document_id=%s.id AND dtask.deleted_at IS NULL AND "+taskPred+"))")
				// The same qualified human manager who may repair an empty task scope
				// must be able to discover it and obtain its current access version.
				if human && write && scope.ReadAll && scope.Manage && scope.Write && scope.TaskIDs == nil {
					clauses = append(clauses, "(%s.visibility='task' AND NOT EXISTS (SELECT 1 FROM document_tasks empty_link JOIN tasks empty_task ON empty_task.id=empty_link.task_id WHERE empty_link.document_id=%s.id AND empty_task.deleted_at IS NULL))")
				}
			} else if !errors.Is(e, taskdom.ErrTaskNotFound) {
				return "", e
			}
		}
		if len(clauses) > 0 {
			branches = append(branches, "(%s.project_id="+bind(p.ID)+" AND ("+strings.Join(clauses, " OR ")+"))")
		}
	}
	object := strings.Join(branches, " OR ")
	if object == "" {
		return "FALSE", nil
	}
	return object, nil
}

func (r *DocumentRepository) CheckDocumentAccess(ctx context.Context, project, id uuid.UUID, action docdom.AccessAction) (*docdom.Document, error) {
	args := []any{id}
	predicate, err := DocumentVisibilityPredicate(ctx, r.db, "documents", &args, action)
	if err != nil {
		return nil, err
	}
	query := `SELECT ` + documentCols + ` FROM documents WHERE id=$1 AND ` + predicate
	if project != uuid.Nil {
		args = append(args, project)
		query += fmt.Sprintf(" AND project_id=$%d", len(args))
	}
	var rec documentRecord
	if err = DeliveryQuery(ctx, r.db).GetContext(ctx, &rec, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, docdom.ErrDocNotFound
		}
		return nil, err
	}
	d := documentFromRecord(rec)

	if d.SourceDocumentID != nil {
		var source struct {
			Title     string    `db:"title"`
			ProjectID uuid.UUID `db:"project_id"`
			UpdatedAt time.Time `db:"updated_at"`
			Content   []byte    `db:"content"`
		}
		if err = DeliveryQuery(ctx, r.db).GetContext(ctx, &source, `SELECT title,content,project_id,updated_at FROM documents WHERE id=$1`, d.SourceDocumentID); err != nil {
			return nil, err
		}
		d.SourceTitle = &source.Title
		d.SourceProjectID = &source.ProjectID
		d.SourceUpdatedAt = &source.UpdatedAt
		d.Content = source.Content
	}
	return d, nil
}

type documentWriteKey struct{}

func documentWriteStarted(ctx context.Context) {
	if started, ok := ctx.Value(documentWriteKey{}).(*bool); ok {
		*started = true
	}
}
func documentValidationError(err error) bool {
	var api *apierr.Error
	if errors.As(err, &api) {
		return api.Code == apierr.CodeBadRequest || api.Code == apierr.CodeForbidden || api.Code == apierr.Code("DOCUMENT_ACCESS_CONFLICT")
	}
	return errors.Is(err, docdom.ErrDocNotFound) || errors.Is(err, taskdom.ErrTaskNotFound) || errors.Is(err, taskdom.ErrTaskForbidden)
}

// ponytail: document/comment writes share the global barrier; partition only after measured contention.
func (r *DocumentRepository) documentMutation(ctx context.Context, fn func(context.Context) error) error {
	var rejected error
	err := authorityMutation(ctx, r.db, func(ctx context.Context) error {
		return WithTx(ctx, r.db, func(tx *sqlx.Tx) error {
			ctx = database.WithTransaction(ctx, tx)
			if err := authz.RecheckMutation(ctx); err != nil {
				return err
			}
			started := false
			err := fn(context.WithValue(ctx, documentWriteKey{}, &started))
			// Only known validation errors before the first write may commit an empty
			// transaction and reopen the gate. SQL/commit/reconciliation errors escape.
			if !started && documentValidationError(err) {
				rejected = err
				return nil
			}
			return err
		})
	})
	if err != nil {
		return err
	}
	return rejected
}
func documentHuman(ctx context.Context) (uuid.UUID, error) {
	user, ok := authz.ActorIDFromContext(ctx)
	_, agent := authz.AgentIDFromContext(ctx)
	if !ok || agent || authz.IsAgentCredential(ctx) || !authz.IsHumanSession(ctx) {
		return uuid.Nil, apierr.New(apierr.CodeForbidden, "document creation and sharing require a verified human owner; agent run-owner binding is unavailable")
	}
	if authz.HasTaskServiceGrant(ctx) {
		return uuid.Nil, apierr.New(apierr.CodeForbidden, "bounded task grants cannot create documents or change document audiences")
	}
	return user, nil
}
func (r *DocumentRepository) checkDocumentScope(ctx context.Context, project uuid.UUID, visibility string, tasks []uuid.UUID) error {
	if visibility != "private" && visibility != "project" && visibility != "task" {
		return apierr.New(apierr.CodeBadRequest, "invalid document visibility")
	}
	user, err := documentHuman(ctx)
	if err != nil {
		return err
	}
	store := NewAuthzPermissionStore(r.db)
	a := authz.NewAuthorizer(store)
	actor, err := store.FindTaskActorAccess(ctx, user, project)
	if err != nil {
		return docdom.ErrDocNotFound
	}
	all, err := a.HasPermissions(ctx, user, nil, "", authz.PermissionAll)
	if err != nil {
		return err
	}
	if actor.MemberID == nil && !all {
		return docdom.ErrDocNotFound
	}
	allowed, err := a.HasPermissions(ctx, user, &project, "", authz.PermissionDocsRead, authz.PermissionDocsWrite)
	if err != nil {
		return err
	}
	if !allowed {
		return docdom.ErrDocNotFound
	}
	if visibility != "private" {
		allowed, err = a.HasPermissions(ctx, user, &project, "", authz.PermissionTasksManage)
		if err != nil {
			return err
		}
		if !allowed {
			return apierr.New(apierr.CodeForbidden, "document sharing requires docs.write and tasks.manage")
		}
	}
	if visibility == "task" && len(tasks) == 0 {
		return apierr.New(apierr.CodeBadRequest, "task scope requires at least one task; change explicitly to private")
	}
	if visibility != "task" && len(tasks) > 0 {
		return apierr.New(apierr.CodeBadRequest, "task associations require task scope")
	}
	checker := tasksvc.New(NewTaskRepository(r.db), authz.NewTaskAccessPolicy(store))
	for _, id := range tasks {
		if _, err = checker.CheckTaskAccess(ctx, project, id, taskdom.AccessManage); err != nil {
			return err
		}
	}
	return nil
}
func (r *DocumentRepository) setDocumentTasks(ctx context.Context, id uuid.UUID, tasks []uuid.UUID) error {
	if _, err := DeliveryQuery(ctx, r.db).ExecContext(ctx, `DELETE FROM document_tasks WHERE document_id=$1`, id); err != nil {
		return err
	}
	for _, task := range tasks {
		if _, err := DeliveryQuery(ctx, r.db).ExecContext(ctx, `INSERT INTO document_tasks(document_id,task_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, id, task); err != nil {
			return err
		}
	}
	return nil
}
func (r *DocumentRepository) UpdateDocumentAccess(ctx context.Context, project, id uuid.UUID, in docdom.UpdateAccessInput) (*docdom.Document, error) {
	var result *docdom.Document
	err := r.documentMutation(ctx, func(ctx context.Context) error {
		user, err := documentHuman(ctx)
		if err != nil {
			return err
		}
		// Recheck current permissions only after the delivery barrier; then lock.
		store := NewAuthzPermissionStore(r.db)
		a := authz.NewAuthorizer(store)
		allowed, err := a.HasPermissions(ctx, user, &project, "", authz.PermissionDocsWrite, authz.PermissionTasksManage)
		if err != nil {
			return err
		}
		if !allowed {
			return apierr.New(apierr.CodeForbidden, "document sharing requires docs.write and tasks.manage")
		}
		d, err := r.CheckDocumentAccess(ctx, project, id, docdom.AccessRead)
		if err != nil {
			return err
		}
		if d.Visibility == "private" && (d.OwnerUserID == nil || *d.OwnerUserID != user) {
			return docdom.ErrDocNotFound
		}
		if err = r.checkDocumentScope(ctx, project, in.Visibility, in.TaskIDs); err != nil {
			return err
		}
		var version int64
		if err = DeliveryQuery(ctx, r.db).GetContext(ctx, &version, `SELECT access_version FROM documents WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, id); err != nil {
			return err
		}
		if version != in.ExpectedAccessVersion {
			return apierr.New(apierr.Code("DOCUMENT_ACCESS_CONFLICT"), "document access version changed")
		}
		if in.Visibility == "private" && d.OwnerUserID == nil {
			return apierr.New(apierr.CodeBadRequest, "legacy document has no verified human owner")
		}
		documentWriteStarted(ctx)
		if _, err = DeliveryQuery(ctx, r.db).ExecContext(ctx, `UPDATE documents SET visibility=$2,access_version=access_version+1 WHERE id=$1`, id, in.Visibility); err != nil {
			return err
		}
		if err = r.setDocumentTasks(ctx, id, in.TaskIDs); err != nil {
			return err
		}
		result = d
		result.Visibility = in.Visibility
		result.AccessVersion = version + 1
		result.TaskIDs = in.TaskIDs
		return nil
	})
	return result, err
}

func taskVisibilitySQL(scope taskdom.AccessScope, alias string, args *[]any) string {
	b := &queryBuilder{args: *args, idx: len(*args) + 1}
	applyTaskVisibility(b, []taskdom.AccessScope{scope})
	*args = b.args
	return strings.ReplaceAll(strings.Join(b.whereClauses, " AND "), "tasks.", alias+".")
}
