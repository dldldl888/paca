// Package postgres — sqlx implementation of sprintdom.SprintRepository.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	sprintdom "github.com/Paca-AI/api/internal/domain/sprint"
	taskdom "github.com/Paca-AI/api/internal/domain/task"
)

// --- sqlx model -------------------------------------------------------------

type sprintRecord struct {
	ID        string     `db:"id"`
	ProjectID string     `db:"project_id"`
	Name      string     `db:"name"`
	StartDate *time.Time `db:"start_date"`
	EndDate   *time.Time `db:"end_date"`
	Goal      *string    `db:"goal"`
	Status    string     `db:"status"`
	CreatedAt time.Time  `db:"created_at"`
	UpdatedAt time.Time  `db:"updated_at"`
}

// --- Repository struct -------------------------------------------------------

// SprintRepository is the sqlx implementation of sprintdom.SprintRepository.
type SprintRepository struct {
	db *sqlx.DB
}

// NewSprintRepository returns a new SprintRepository.
func NewSprintRepository(db *sqlx.DB) *SprintRepository {
	return &SprintRepository{db: db}
}

const sprintSelectCols = `id, project_id, name, start_date, end_date, goal, status, created_at, updated_at`

// --- Methods ----------------------------------------------------------------

// ListSprints returns all sprints for a project ordered by creation time.
func (r *SprintRepository) ListSprints(ctx context.Context, projectID uuid.UUID) ([]*sprintdom.Sprint, error) {
	var records []sprintRecord
	if err := r.db.SelectContext(ctx, &records, `SELECT `+sprintSelectCols+` FROM sprints WHERE project_id = $1 ORDER BY created_at ASC`, projectID.String()); err != nil {
		return nil, fmt.Errorf("sprint repo: list: %w", err)
	}
	out := make([]*sprintdom.Sprint, 0, len(records))
	for i := range records {
		out = append(out, toSprintEntity(&records[i]))
	}
	return out, nil
}

// FindSprintByID returns the sprint with the given ID.
func (r *SprintRepository) FindSprintByID(ctx context.Context, id uuid.UUID) (*sprintdom.Sprint, error) {
	var rec sprintRecord
	err := r.db.GetContext(ctx, &rec, `SELECT `+sprintSelectCols+` FROM sprints WHERE id = $1`, id.String())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sprintdom.ErrSprintNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("sprint repo: find by id: %w", err)
	}
	return toSprintEntity(&rec), nil
}

// CreateSprint persists a new sprint.
func (r *SprintRepository) CreateSprint(ctx context.Context, s *sprintdom.Sprint) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO sprints (id, project_id, name, start_date, end_date, goal, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		s.ID.String(), s.ProjectID.String(), s.Name, s.StartDate, s.EndDate,
		s.Goal, string(s.Status), s.CreatedAt, s.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("sprint repo: create: %w", err)
	}
	return nil
}

// UpdateSprint persists changes to an existing sprint.
func (r *SprintRepository) UpdateSprint(ctx context.Context, s *sprintdom.Sprint) error {
	return WithTx(ctx, r.db, func(tx *sqlx.Tx) error {
		if err := lockSprintForMutation(ctx, tx, s.ID); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `
		UPDATE sprints SET name = $1, start_date = $2, end_date = $3, goal = $4, status = $5, updated_at = $6
		WHERE id = $7`,
			s.Name, s.StartDate, s.EndDate, s.Goal, string(s.Status), s.UpdatedAt, s.ID.String(),
		)
		if err != nil {
			return fmt.Errorf("sprint repo: update: %w", err)
		}
		n, _ := result.RowsAffected()
		if n == 0 {
			return sprintdom.ErrSprintNotFound
		}
		return nil
	})
}

// DeleteSprint removes a sprint by ID.
func (r *SprintRepository) DeleteSprint(ctx context.Context, id uuid.UUID) error {
	return WithTx(ctx, r.db, func(tx *sqlx.Tx) error {
		if err := lockSprintForMutation(ctx, tx, id); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM sprints WHERE id = $1`, id.String())
		if err != nil {
			return fmt.Errorf("sprint repo: delete: %w", err)
		}
		n, _ := result.RowsAffected()
		if n == 0 {
			return sprintdom.ErrSprintNotFound
		}
		return nil
	})
}

// --- Entity converter -------------------------------------------------------

func toSprintEntity(r *sprintRecord) *sprintdom.Sprint {
	id, _ := uuid.Parse(r.ID)
	pid, _ := uuid.Parse(r.ProjectID)
	return &sprintdom.Sprint{
		ID:        id,
		ProjectID: pid,
		Name:      r.Name,
		StartDate: r.StartDate,
		EndDate:   r.EndDate,
		Goal:      r.Goal,
		Status:    sprintdom.SprintStatus(r.Status),
		CreatedAt: r.CreatedAt,
		UpdatedAt: r.UpdatedAt,
	}
}

// Lock the referenced sprint before checking legacy rows, so a concurrent
// reference cannot appear between the check and a delete/configuration change.
func lockSprintForMutation(ctx context.Context, tx *sqlx.Tx, id uuid.UUID) error {
	var projectID uuid.UUID
	if err := tx.GetContext(ctx, &projectID, `SELECT project_id FROM sprints WHERE id=$1 FOR UPDATE`, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sprintdom.ErrSprintNotFound
		}
		return err
	}
	var foreign bool
	if err := tx.GetContext(ctx, &foreign, `SELECT EXISTS(SELECT 1 FROM tasks WHERE sprint_id=$1 AND project_id<>$2)`, id, projectID); err != nil {
		return err
	}
	if foreign {
		return taskdom.ErrTaskForbidden
	}
	return nil
}

// CompleteSprintAuthorized rechecks live authority and the complete task set
// under locks, then commits task moves and sprint status together.
func (r *SprintRepository) CompleteSprintAuthorized(ctx context.Context, projectID, sprintID uuid.UUID, targetID *uuid.UUID, expectedIDs []uuid.UUID) (*sprintdom.Sprint, error) {
	var result *sprintdom.Sprint
	err := WithTx(ctx, r.db, func(tx *sqlx.Tx) error {
		scope, err := lockTaskProjectAccess(ctx, tx, projectID)
		if err != nil {
			return err
		}
		if !scope.Write || !scope.Manage {
			return taskdom.ErrTaskForbidden
		}
		// FOR UPDATE also blocks a new task's foreign-key reference until commit.
		var sprints []sprintRecord
		if err := tx.SelectContext(ctx, &sprints, `SELECT `+sprintSelectCols+` FROM sprints WHERE (id=$1 OR id=$2) AND project_id=$3 ORDER BY id FOR UPDATE`, sprintID, uuidPtrToStrPtr(targetID), projectID); err != nil {
			return err
		}
		var source *sprintdom.Sprint
		targetExists := targetID == nil
		for i := range sprints {
			if sprints[i].ID == sprintID.String() {
				source = toSprintEntity(&sprints[i])
			}
			if targetID != nil && sprints[i].ID == targetID.String() {
				targetExists = true
			}
		}
		if source == nil || !targetExists {
			return sprintdom.ErrSprintNotFound
		}
		if err := lockSprintForMutation(ctx, tx, sprintID); err != nil {
			return err
		}
		if source.Status == sprintdom.SprintStatusCompleted {
			return sprintdom.ErrSprintAlreadyComplete
		}
		if targetID != nil && *targetID == sprintID {
			return taskdom.ErrTaskForbidden
		}
		var statuses []uuid.UUID
		if err := tx.SelectContext(ctx, &statuses, `SELECT id FROM task_statuses WHERE project_id=$1 ORDER BY id FOR SHARE`, projectID); err != nil {
			return err
		}
		var ids []uuid.UUID
		if err := tx.SelectContext(ctx, &ids, `SELECT id FROM tasks WHERE project_id=$1 AND sprint_id=$2 AND deleted_at IS NULL AND (status_id IS NULL OR status_id NOT IN (SELECT id FROM task_statuses WHERE project_id=$1 AND category='done')) ORDER BY id FOR UPDATE`, projectID, sprintID); err != nil {
			return err
		}
		expected := append([]uuid.UUID(nil), expectedIDs...)
		sort.Slice(expected, func(i, j int) bool { return expected[i].String() < expected[j].String() })
		if !slices.Equal(ids, expected) {
			return taskdom.ErrTaskForbidden
		}
		if len(ids) == 0 && !scope.CanCreate() {
			return taskdom.ErrTaskForbidden
		}
		for _, id := range ids {
			if _, err := lockAuthorizedTask(ctx, tx, projectID, id, scope, taskdom.AccessManage); err != nil {
				return err
			}
		}
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `UPDATE tasks SET sprint_id=$1,updated_at=$2 WHERE id=$3 AND project_id=$4 AND sprint_id=$5`, uuidPtrToStrPtr(targetID), time.Now(), id, projectID, sprintID); err != nil {
				return err
			}
		}
		var updated sprintRecord
		if err := tx.GetContext(ctx, &updated, `UPDATE sprints SET status='completed',updated_at=$1 WHERE id=$2 AND project_id=$3 RETURNING `+sprintSelectCols, time.Now(), sprintID, projectID); err != nil {
			return err
		}
		result = toSprintEntity(&updated)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// DeleteSprintAuthorized protects every task affected by ON DELETE SET NULL,
// including done tasks, with current project-wide management authority.
func (r *SprintRepository) DeleteSprintAuthorized(ctx context.Context, projectID, id uuid.UUID) error {
	return WithTx(ctx, r.db, func(tx *sqlx.Tx) error {
		scope, err := lockTaskProjectAccess(ctx, tx, projectID)
		if err != nil {
			return err
		}
		if !scope.ReadAll || !scope.CanCreate() {
			return taskdom.ErrTaskForbidden
		}
		if err := lockSprintForMutation(ctx, tx, id); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM sprints WHERE id=$1 AND project_id=$2`, id, projectID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return sprintdom.ErrSprintNotFound
		}
		return nil
	})
}
