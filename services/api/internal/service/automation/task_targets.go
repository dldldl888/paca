package automationsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	automationdom "github.com/Paca-AI/api/internal/domain/automation"
	taskdom "github.com/Paca-AI/api/internal/domain/task"
	"github.com/Paca-AI/api/internal/platform/authz"
	"github.com/google/uuid"
	"slices"
)

// GrantTasks validates live project/object links and grants only this operation's IDs.
func GrantTasks(ctx context.Context, repo taskLookup, checker taskdom.AccessChecker, projectID uuid.UUID, action taskdom.AccessAction, ids ...uuid.UUID) (context.Context, []*taskdom.Task, error) {
	if checker == nil || repo == nil {
		return nil, nil, taskdom.ErrTaskForbidden
	}
	readIDs := append([]uuid.UUID(nil), ids...)
	for _, id := range ids {
		t, err := repo.FindTaskByID(ctx, id)
		if err != nil {
			return nil, nil, err
		}
		if t == nil || t.ID != id || t.ProjectID != projectID {
			return nil, nil, taskdom.ErrTaskNotFound
		}
		// The common checker redacts parent IDs without their own read authority.
		// Validate that immediate reference before retaining it for graph traversal.
		if t.ParentTaskID != nil {
			parent, err := repo.FindTaskByID(ctx, *t.ParentTaskID)
			if err != nil {
				return nil, nil, err
			}
			if parent == nil || parent.ID != *t.ParentTaskID || parent.ProjectID != projectID {
				return nil, nil, taskdom.ErrTaskNotFound
			}
			if !slices.Contains(readIDs, parent.ID) {
				readIDs = append(readIDs, parent.ID)
			}
		}
	}
	readCtx, err := authz.WithTaskServiceGrant(ctx, authz.TaskServiceGrant{Name: "automation", ProjectID: projectID, TaskIDs: readIDs, Action: taskdom.AccessRead})
	if err != nil {
		return nil, nil, err
	}
	for _, id := range readIDs {
		if _, err := checker.CheckTaskAccess(readCtx, projectID, id, taskdom.AccessRead); err != nil {
			return nil, nil, err
		}
	}
	granted, err := authz.WithTaskServiceGrant(ctx, authz.TaskServiceGrant{Name: "automation", ProjectID: projectID, TaskIDs: ids, Action: action})
	if err != nil {
		return nil, nil, err
	}
	tasks := make([]*taskdom.Task, 0, len(ids))
	for _, id := range ids {
		if _, err := checker.CheckTaskAccess(granted, projectID, id, action); err != nil {
			return nil, nil, err
		}
		t, err := checker.CheckTaskAccess(readCtx, projectID, id, taskdom.AccessRead)
		if err != nil {
			return nil, nil, err
		}
		tasks = append(tasks, t)
	}
	return granted, tasks, nil
}

// ResolveTargetTasks expands complete same-project targets before any fanout effect.
func ResolveTargetTasks(ctx context.Context, repo taskLookup, checker taskdom.AccessChecker, projectID uuid.UUID, baseTask *taskdom.Task, target *automationdom.TaskTarget) ([]*taskdom.Task, error) {
	if baseTask == nil || baseTask.ProjectID != projectID {
		return nil, taskdom.ErrTaskNotFound
	}
	_, base, err := GrantTasks(ctx, repo, checker, projectID, taskdom.AccessRead, baseTask.ID)
	if err != nil {
		return nil, err
	}
	baseTask = base[0]
	finish := func(tasks []*taskdom.Task) ([]*taskdom.Task, error) {
		if len(tasks) == 0 {
			return nil, nil
		}
		ids := make([]uuid.UUID, len(tasks))
		for i, t := range tasks {
			if t == nil || t.ProjectID != projectID {
				return nil, taskdom.ErrTaskNotFound
			}
			ids[i] = t.ID
		}
		_, fresh, err := GrantTasks(ctx, repo, checker, projectID, taskdom.AccessRead, ids...)
		return fresh, err
	}

	if target == nil || target.Kind == "" || target.Kind == automationdom.TaskTargetSelf {
		return finish([]*taskdom.Task{baseTask})
	}
	switch target.Kind {
	case automationdom.TaskTargetOther:
		if target.OtherTaskID == nil {
			return nil, fmt.Errorf("target: other requires other_task_id")
		}
		t, err := repo.FindTaskByID(ctx, *target.OtherTaskID)
		if err != nil {
			return nil, fmt.Errorf("target: find other task: %w", err)
		}
		return finish([]*taskdom.Task{t})
	case automationdom.TaskTargetParent:
		if baseTask.ParentTaskID == nil {
			return nil, nil
		}
		t, err := repo.FindTaskByID(ctx, *baseTask.ParentTaskID)
		if err != nil {
			return nil, fmt.Errorf("target: find parent task: %w", err)
		}
		return finish([]*taskdom.Task{t})
	case automationdom.TaskTargetChildren:
		reader, ok := repo.(interface {
			ListChildTaskIDs(context.Context, uuid.UUID) ([]uuid.UUID, error)
		})
		if !ok {
			return nil, taskdom.ErrTaskForbidden
		}
		ids, err := reader.ListChildTaskIDs(ctx, baseTask.ID)
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			return nil, nil
		}
		_, children, err := GrantTasks(ctx, repo, checker, projectID, taskdom.AccessRead, ids...)
		if err != nil {
			return nil, err
		}
		for _, child := range children {
			if child.ParentTaskID == nil || *child.ParentTaskID != baseTask.ID {
				return nil, taskdom.ErrTaskNotFound
			}
		}
		return children, nil
	case automationdom.TaskTargetBlocks, automationdom.TaskTargetIsBlockedBy, automationdom.TaskTargetRelatesTo, automationdom.TaskTargetDuplicates, automationdom.TaskTargetIsDuplicatedBy:
		reader, ok := repo.(interface {
			ListTaskLinks(context.Context, uuid.UUID) ([]*taskdom.TaskLink, error)
		})
		if !ok {
			return nil, taskdom.ErrTaskForbidden
		}
		links, err := reader.ListTaskLinks(ctx, baseTask.ID)
		if err != nil {
			return nil, fmt.Errorf("target: list task links: %w", err)
		}
		var out []*taskdom.Task
		for _, link := range links {
			if link.SourceTaskID != baseTask.ID && link.TargetTaskID != baseTask.ID {
				return nil, taskdom.ErrTaskNotFound
			}
			if link.DisplayLinkType != string(target.Kind) {
				continue
			}
			otherID := link.TargetTaskID
			if link.SourceTaskID != baseTask.ID {
				otherID = link.SourceTaskID
			}
			t, err := repo.FindTaskByID(ctx, otherID)
			if err != nil {
				if errors.Is(err, taskdom.ErrTaskNotFound) {
					continue
				}
				return nil, err
			}
			out = append(out, t)
		}
		return finish(out)
	default:
		return nil, fmt.Errorf("target: unknown kind %q", target.Kind)
	}
}

// ValidateGraphTaskTargets preflights every concrete task reference before bookkeeping.
func ValidateGraphTaskTargets(ctx context.Context, repo taskLookup, checker taskdom.AccessChecker, projectID uuid.UUID, graph *automationdom.Graph, task *taskdom.Task) error {
	if repo == nil || checker == nil || graph == nil || projectID == uuid.Nil {
		return taskdom.ErrTaskForbidden
	}
	if task != nil {
		_, _, err := GrantTasks(ctx, repo, checker, projectID, taskdom.AccessRead, task.ID)
		if err != nil {
			return err
		}
	}
	// Preflight all task targets before a run or scheduler bookkeeping exists.
	for _, n := range graph.Nodes {
		var targets []*automationdom.TaskTarget
		switch n.Kind {
		case automationdom.KindAction:
			var cfg automationdom.ActionConfig
			if err := json.Unmarshal(n.Config, &cfg); err != nil {
				return err
			}
			if cfg.Target != nil {
				targets = append(targets, cfg.Target)
			}
		case automationdom.KindCondition:
			var cfg automationdom.ConditionConfig
			if err := json.Unmarshal(n.Config, &cfg); err != nil {
				return err
			}
			for _, branch := range cfg.Branches {
				if branch.Tree != nil && branch.Tree.Target != nil {
					targets = append(targets, branch.Tree.Target)
				}
			}
		}
		for _, target := range targets {
			if _, err := ResolveTargetTasks(ctx, repo, checker, projectID, task, target); err != nil {
				return err
			}
		}
	}
	return nil
}
