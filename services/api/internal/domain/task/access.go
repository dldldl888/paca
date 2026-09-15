package taskdom

import (
	"context"
	"github.com/google/uuid"
	"slices"
)

// AccessAction separates execution progress from management of formal tasks.
type AccessAction uint8

const (
	AccessRead AccessAction = iota
	AccessProgress
	AccessManage
)

// AccessScope is resolved from current identity and project qualification per use
// case. Never persist or reuse it across requests, queueing or event delivery.
type AccessScope struct {
	ProjectID uuid.UUID
	UserID    uuid.UUID
	MemberID  uuid.UUID
	ReadAll   bool
	Write     bool
	Manage    bool
	TaskIDs   []uuid.UUID // non-nil for a bounded backend grant; empty grants deny all
}

type AccessPolicy interface {
	ResolveTaskAccess(context.Context, uuid.UUID) (AccessScope, error)
}

type AccessChecker interface {
	CheckTaskAccess(context.Context, uuid.UUID, uuid.UUID, AccessAction) (*Task, error)
}

func (s AccessScope) IsAssignee(t *Task) bool {
	return s.MemberID != uuid.Nil && slices.Contains(t.AssigneeIDs, s.MemberID)
}

func (s AccessScope) CanRead(t *Task) bool {
	if t == nil || s.ProjectID == uuid.Nil || t.ProjectID != s.ProjectID {
		return false
	}
	if s.TaskIDs != nil && !slices.Contains(s.TaskIDs, t.ID) {
		return false
	}
	return s.ReadAll || (s.UserID != uuid.Nil && t.CreatorUserID != nil && *t.CreatorUserID == s.UserID) || s.IsAssignee(t)
}

func (s AccessScope) Require(t *Task, action AccessAction) error {
	if !s.CanRead(t) {
		return ErrTaskNotFound
	}
	switch action {
	case AccessRead:
		return nil
	case AccessProgress:
		if s.Write && (s.Manage || s.IsAssignee(t) || len(s.TaskIDs) > 0) {
			return nil
		}
	case AccessManage:
		if s.Write && s.Manage {
			return nil
		}
	}
	return ErrTaskForbidden
}

// CanCreate requires an authenticated human manager; bounded object grants cannot create tasks.
func (s AccessScope) CanCreate() bool {
	return s.ProjectID != uuid.Nil && s.UserID != uuid.Nil && s.Write && s.Manage && s.TaskIDs == nil
}
