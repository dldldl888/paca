package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	draftdom "github.com/Paca-AI/api/internal/domain/draft"
	notificationdom "github.com/Paca-AI/api/internal/domain/notification"
	taskdom "github.com/Paca-AI/api/internal/domain/task"
	"github.com/Paca-AI/api/internal/platform/authz"
	"github.com/Paca-AI/api/internal/platform/database"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"sort"
	"strings"
	"time"
)

type DraftRepository struct{ db *sqlx.DB }

func NewDraftRepository(db *sqlx.DB) *DraftRepository { return &DraftRepository{db} }
func draftAuthority(ctx context.Context, tx *sqlx.Tx, pid uuid.UUID) (taskdom.AccessScope, error) {
	scope, err := lockTaskProjectAccess(ctx, tx, pid)
	if err != nil {
		return scope, err
	}
	if !scope.CanCreate() {
		return scope, taskdom.ErrTaskForbidden
	}
	return scope, nil
}
func (r *DraftRepository) Create(ctx context.Context, pid uuid.UUID, content draftdom.Content) (*draftdom.Draft, error) {
	var d *draftdom.Draft
	err := WithTx(ctx, r.db, func(tx *sqlx.Tx) error {
		scope, e := draftAuthority(ctx, tx, pid)
		if e != nil {
			return e
		}
		now := time.Now()
		content.Normalize()
		d = &draftdom.Draft{Content: content, ID: uuid.New(), ProjectID: pid, OwnerUserID: scope.UserID, Version: 1, State: "editable", PublishedTasks: []draftdom.PublishedTask{}, CreatedAt: now, UpdatedAt: now}
		b, e := json.Marshal(d)
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, "INSERT INTO task_drafts(id,project_id,owner_user_id,version,state,content,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)", d.ID, pid, d.OwnerUserID, d.Version, d.State, b, now, now)
		return e
	})
	return d, err
}

// Locked keeps owner, live authorization and version checks in the mutation transaction.
func (r *DraftRepository) Locked(ctx context.Context, pid, id uuid.UUID, fn func(context.Context, *sqlx.Tx, *draftdom.Draft, taskdom.AccessScope) error) (*draftdom.Draft, error) {
	var d draftdom.Draft
	err := WithTx(ctx, r.db, func(tx *sqlx.Tx) error {
		ctx = database.WithTransaction(ctx, tx)
		user, ok := authz.ActorIDFromContext(ctx)
		if !ok {
			return draftdom.ErrNotFound
		}
		var b []byte
		e := tx.GetContext(ctx, &b, "SELECT content FROM task_drafts WHERE id=$1 AND project_id=$2 AND owner_user_id=$3 FOR UPDATE", id, pid, user)
		if errors.Is(e, sql.ErrNoRows) {
			return draftdom.ErrNotFound
		}
		if e != nil {
			return e
		}
		if e = json.Unmarshal(b, &d); e != nil {
			return e
		}
		scope, e := draftAuthority(ctx, tx, pid)
		if e != nil {
			return e
		}
		if scope.UserID != d.OwnerUserID {
			return draftdom.ErrNotFound
		}
		return fn(ctx, tx, &d, scope)
	})
	if err != nil {
		return nil, err
	}
	return &d, nil
}
func (r *DraftRepository) Get(ctx context.Context, pid, id uuid.UUID) (*draftdom.Draft, error) {
	return r.Locked(ctx, pid, id, func(context.Context, *sqlx.Tx, *draftdom.Draft, taskdom.AccessScope) error { return nil })
}
func (r *DraftRepository) List(ctx context.Context, pid uuid.UUID) ([]*draftdom.Draft, error) {
	result := []*draftdom.Draft{}
	err := WithTx(ctx, r.db, func(tx *sqlx.Tx) error {
		scope, e := draftAuthority(ctx, tx, pid)
		if e != nil {
			return e
		}
		// ponytail: newest 100 private drafts; add a cursor when projects need older browsing.
		var rows [][]byte
		if e = tx.SelectContext(ctx, &rows, "SELECT content FROM task_drafts WHERE project_id=$1 AND owner_user_id=$2 ORDER BY updated_at DESC,id DESC LIMIT 100", pid, scope.UserID); e != nil {
			return e
		}
		for _, b := range rows {
			var d draftdom.Draft
			if e = json.Unmarshal(b, &d); e != nil {
				return e
			}
			result = append(result, &d)
		}
		return nil
	})
	return result, err
}
func SaveDraft(ctx context.Context, tx *sqlx.Tx, d *draftdom.Draft) error {
	d.UpdatedAt = time.Now()
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE task_drafts SET version=$2,state=$3,content=$4,publication_id=$5,updated_at=$6 WHERE id=$1", d.ID, d.Version, d.State, b, d.PublicationID, d.UpdatedAt)
	if constraint, duplicate := uniqueViolationConstraint(err); duplicate && constraint == "task_drafts_publication_id_key" {
		return draftdom.ErrConflict
	}
	return err
}
func CheckDraftVersion(d *draftdom.Draft, expected int) error {
	if d.State != "editable" || expected != d.Version {
		return draftdom.ErrConflict
	}
	return nil
}
func (r *DraftRepository) Update(ctx context.Context, pid, id uuid.UUID, expected int, c draftdom.Content) (*draftdom.Draft, error) {
	return r.Locked(ctx, pid, id, func(ctx context.Context, tx *sqlx.Tx, d *draftdom.Draft, _ taskdom.AccessScope) error {
		if e := CheckDraftVersion(d, expected); e != nil {
			return e
		}
		c.Normalize()
		d.Content = c
		d.Version++
		return SaveDraft(ctx, tx, d)
	})
}

// Check assignee rows again after taking deterministic user/member locks. Snapshot
// joins alone can miss a concurrent deletion or a user reassignment while waiting.
func draftMembers(ctx context.Context, tx *sqlx.Tx, pid uuid.UUID, items []draftdom.Item) (map[string]uuid.UUID, []draftdom.Issue, error) {
	members := map[string]uuid.UUID{}
	issues := []draftdom.Issue{}
	ids := []string{}
	for _, it := range items {
		if _, e := uuid.Parse(it.AssigneeMemberID); e == nil {
			ids = append(ids, it.AssigneeMemberID)
		}
	}
	sort.Strings(ids)
	users := []string{}
	for _, id := range ids {
		var uid uuid.UUID
		e := tx.GetContext(ctx, &uid, "SELECT user_id FROM project_members WHERE id=$1 AND project_id=$2 AND member_type='human' AND deleted_at IS NULL", id, pid)
		if errors.Is(e, sql.ErrNoRows) {
			continue
		}
		if e != nil {
			return nil, nil, e
		}
		users = append(users, uid.String())
	}
	sort.Strings(users)
	for _, uid := range users {
		var locked uuid.UUID
		e := tx.GetContext(ctx, &locked, "SELECT id FROM users WHERE id=$1 FOR SHARE", uid)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return nil, nil, e
		}
	}
	for _, id := range ids {
		var uid uuid.UUID
		e := tx.GetContext(ctx, &uid, "SELECT user_id FROM project_members WHERE id=$1 AND project_id=$2 AND member_type='human' AND deleted_at IS NULL FOR SHARE", id, pid)
		if errors.Is(e, sql.ErrNoRows) {
			continue
		}
		if e != nil {
			return nil, nil, e
		}
		// A newly reassigned user not in the locked set is rejected for a clean retry.
		j := sort.SearchStrings(users, uid.String())
		if j == len(users) || users[j] != uid.String() {
			continue
		}
		var active bool
		if e = tx.GetContext(ctx, &active, "SELECT EXISTS(SELECT 1 FROM users WHERE id=$1 AND deleted_at IS NULL)", uid); e != nil {
			return nil, nil, e
		}
		if active {
			members[id] = uid
		}
	}
	for i, it := range items {
		if _, ok := members[it.AssigneeMemberID]; !ok {
			issues = append(issues, draftdom.Issue{Field: fmt.Sprintf("items[%d].assignee_member_id", i), Message: "负责人必须是本项目有效人类成员"})
		}
	}
	return members, issues, nil
}
func (r *DraftRepository) Preview(ctx context.Context, pid, id uuid.UUID, expected int) (*draftdom.Preview, error) {
	var p draftdom.Preview
	_, err := r.Locked(ctx, pid, id, func(ctx context.Context, tx *sqlx.Tx, d *draftdom.Draft, _ taskdom.AccessScope) error {
		if e := CheckDraftVersion(d, expected); e != nil {
			return e
		}
		p = d.Project()
		_, issues, e := draftMembers(ctx, tx, pid, p.Items)
		if e != nil {
			return e
		}
		p.Issues = append(p.Issues, issues...)
		p.CanPublish = len(p.Issues) == 0
		return nil
	})
	return &p, err
}
func (r *DraftRepository) Publish(ctx context.Context, pid, id uuid.UUID, expected int, publication uuid.UUID) (*draftdom.Draft, error) {
	return r.Locked(ctx, pid, id, func(ctx context.Context, tx *sqlx.Tx, d *draftdom.Draft, scope taskdom.AccessScope) error {
		if d.State == "published" && d.PublicationID != nil && *d.PublicationID == publication && d.PublishedVersion == expected {
			return nil
		}
		if e := CheckDraftVersion(d, expected); e != nil {
			return e
		}
		// Claim the UUID before any formal writes; the unique index serializes reuse across drafts.
		d.PublicationID = &publication
		if e := SaveDraft(ctx, tx, d); e != nil {
			return e
		}
		p := d.Project()
		members, issues, e := draftMembers(ctx, tx, pid, p.Items)
		if e != nil {
			return e
		}
		p.Issues = append(p.Issues, issues...)
		if len(p.Issues) > 0 {
			return &DraftValidationError{p.Issues}
		}
		var typeID, statusID *uuid.UUID
		if e = tx.GetContext(ctx, &typeID, "SELECT id FROM task_types WHERE project_id=$1 AND is_default=true LIMIT 1 FOR SHARE", pid); e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if e = tx.GetContext(ctx, &statusID, "SELECT id FROM task_statuses WHERE project_id=$1 AND is_default=true LIMIT 1 FOR SHARE", pid); e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		repo := NewTaskRepository(r.db)
		activities := NewTaskActivityRepository(r.db)
		notifications := NewNotificationRepository(r.db)
		now := time.Now()
		mapping := map[string]uuid.UUID{}
		d.PublishedTasks = []draftdom.PublishedTask{}
		var actor *uuid.UUID
		if scope.MemberID != uuid.Nil {
			actor = &scope.MemberID
		}
		for _, it := range p.Items {
			due, _ := time.Parse(time.RFC3339, it.DueDate)
			var start *time.Time
			if it.StartDate != "" {
				v, _ := time.Parse(time.RFC3339, it.StartDate)
				start = &v
			}
			task := &taskdom.Task{ID: uuid.New(), ProjectID: pid, TaskTypeID: typeID, StatusID: statusID, Title: strings.TrimSpace(it.Title), Description: it.Description(), Importance: []int{0, 10, 35, 75, 150}[it.Importance], AssigneeIDs: []uuid.UUID{uuid.MustParse(it.AssigneeMemberID)}, CustomFields: map[string]any{}, Tags: []string{}, StartDate: start, DueDate: &due, CreatedAt: now, UpdatedAt: now}
			if e = repo.CreateTaskAuthorized(ctx, task, scope); e != nil {
				return e
			}
			mapping[it.ID] = task.ID
			d.PublishedTasks = append(d.PublishedTasks, draftdom.PublishedTask{ItemID: it.ID, TaskID: task.ID, TaskNumber: task.TaskNumber})
			if e = activities.CreateActivity(ctx, &taskdom.Activity{ID: uuid.New(), TaskID: task.ID, ActorID: actor, ActivityType: taskdom.ActivityTypeTaskCreated, Content: json.RawMessage("{}"), CreatedAt: now, UpdatedAt: now}); e != nil {
				return e
			}
			event := "draft:" + publication.String() + ":" + it.ID
			if e = notifications.Create(ctx, &notificationdom.Notification{ID: uuid.New(), RecipientUserID: members[it.AssigneeMemberID], ActorMemberID: actor, Type: notificationdom.NotificationTypeAssigned, TaskID: &task.ID, ProjectID: pid, SourceEventID: &event, CreatedAt: now}); e != nil {
				return e
			}
		}
		links := NewTaskLinkRepository(r.db)
		for _, it := range p.Items {
			for _, dep := range it.Dependencies {
				if _, e = links.CreateTaskLinkIfNotExists(ctx, &taskdom.TaskLink{ID: uuid.New(), SourceTaskID: mapping[dep], TargetTaskID: mapping[it.ID], LinkType: taskdom.LinkTypeBlocks, CreatedBy: &scope.UserID, CreatedAt: now}); e != nil {
					return e
				}
			}
		}
		d.State = "published"
		d.PublishedVersion = expected
		d.PublishedAt = &now
		return SaveDraft(ctx, tx, d)
	})
}

type DraftValidationError struct{ Issues []draftdom.Issue }

func (e *DraftValidationError) Error() string { return "draft has validation issues" }
