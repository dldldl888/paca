// Package sprintsvc implements sprint view application services.
package sprintsvc

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	sprintdom "github.com/Paca-AI/api/internal/domain/sprint"
	taskdom "github.com/Paca-AI/api/internal/domain/task"
	"github.com/Paca-AI/api/internal/events"
	"github.com/Paca-AI/api/internal/platform/messaging"
)

// ViewService is the concrete implementation of sprintdom.ViewService.
type ViewService struct {
	repo       sprintdom.ViewRepository
	sprintRepo sprintdom.SprintRepository
	taskRepo   taskdom.TaskRepository
	publisher  *messaging.Publisher
	access     taskdom.AccessChecker
}

// NewViewService returns a configured ViewService. sprintRepo and taskRepo
// are used to verify that a sprint-context view's sprint, and a task
// position's task, belong to the project the caller was authorized against.
// publisher may be nil; real-time events are then skipped silently.
func NewViewService(repo sprintdom.ViewRepository, sprintRepo sprintdom.SprintRepository, taskRepo taskdom.TaskRepository, publisher *messaging.Publisher) *ViewService {
	return &ViewService{repo: repo, sprintRepo: sprintRepo, taskRepo: taskRepo, publisher: publisher}
}

func (s *ViewService) WithTaskAccessChecker(checker taskdom.AccessChecker) *ViewService {
	s.access = checker
	return s
}

func (s *ViewService) checkTask(ctx context.Context, projectID, taskID uuid.UUID, action taskdom.AccessAction) error {
	if s.access == nil {
		return taskdom.ErrTaskNotFound
	}
	_, err := s.access.CheckTaskAccess(ctx, projectID, taskID, action)
	return err
}

// sprintInProject returns nil when sprintID resolves to a sprint that
// belongs to projectID, and sprintdom.ErrSprintNotFound otherwise (including
// when the sprint does not exist at all).
func (s *ViewService) sprintInProject(ctx context.Context, projectID, sprintID uuid.UUID) error {
	sp, err := s.sprintRepo.FindSprintByID(ctx, sprintID)
	if err != nil {
		return err
	}
	if sp.ProjectID != projectID {
		return sprintdom.ErrSprintNotFound
	}
	return nil
}

// publish sends a real-time pub/sub notification for a view change. Errors
// are silently swallowed so a messaging failure never blocks the primary
// HTTP response — this is how the frontend learns to refresh its sprint/
// backlog/timeline view list instead of relying on query staleTime.
func (s *ViewService) publish(ctx context.Context, topic string, payload map[string]any) {
	if s.publisher == nil {
		return
	}
	_ = s.publisher.Publish(ctx, events.ChannelRealtime, map[string]any{
		"type":    topic,
		"payload": payload,
	})
}

// viewPayload builds the common event payload fields shared by all view
// events: project_id, view_id, view_context, and — for sprint-context views
// — sprint_id.
func viewPayload(v *sprintdom.SprintView) map[string]any {
	payload := map[string]any{
		"project_id":   v.ProjectID.String(),
		"view_id":      v.ID.String(),
		"view_context": string(v.ViewContext),
	}
	if v.SprintID != nil {
		payload["sprint_id"] = v.SprintID.String()
	}
	return payload
}

// hasPluginConfig reports whether cfg carries the plugin binding required for
// a "plugin" view_type. Non-plugin view types are always valid here — this
// only guards against a "plugin" view being persisted without the
// PluginID/PluginComponent pair the frontend needs to resolve its extension
// point (see apps/web's InteractionLayout, which falls back to a "Plugin not
// available" empty state when either is missing or doesn't match a
// registered plugin). Enforced server-side so no caller — MCP client, stale
// tool version, or otherwise — can persist a broken plugin view even when it
// skips (or predates) the equivalent client-side check.
func hasPluginConfig(vt sprintdom.ViewType, cfg *sprintdom.ViewConfig) bool {
	if vt != sprintdom.ViewTypePlugin {
		return true
	}
	if cfg == nil {
		return false
	}
	return strings.TrimSpace(cfg.PluginID) != "" && strings.TrimSpace(cfg.PluginComponent) != ""
}

// ListViews returns all views for a sprint, verifying the sprint belongs to projectID.
func (s *ViewService) ListViews(ctx context.Context, projectID, sprintID uuid.UUID) ([]*sprintdom.SprintView, error) {
	if err := s.sprintInProject(ctx, projectID, sprintID); err != nil {
		return nil, err
	}
	return s.repo.ListViews(ctx, sprintID)
}

// ListProjectViews returns all views for a project filtered by viewCtx.
func (s *ViewService) ListProjectViews(ctx context.Context, projectID uuid.UUID, viewCtx sprintdom.ViewContext) ([]*sprintdom.SprintView, error) {
	return s.repo.ListProjectViews(ctx, projectID, viewCtx)
}

// GetView returns the view with the given ID, verifying it belongs to projectID.
func (s *ViewService) GetView(ctx context.Context, projectID, id uuid.UUID) (*sprintdom.SprintView, error) {
	v, err := s.repo.FindViewByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if v.ProjectID != projectID {
		return nil, sprintdom.ErrViewNotFound
	}
	return v, nil
}

// CreateView creates a new view for the given sprint. When in.SprintID is
// set (sprint-context view), verifies the sprint belongs to in.ProjectID.
func (s *ViewService) CreateView(ctx context.Context, in sprintdom.CreateViewInput) (*sprintdom.SprintView, error) {
	if in.SprintID != nil {
		if err := s.sprintInProject(ctx, in.ProjectID, *in.SprintID); err != nil {
			return nil, err
		}
	}

	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, sprintdom.ErrViewNameInvalid
	}

	vt := in.ViewType
	if vt == "" {
		vt = sprintdom.ViewTypeTable
	}
	if !sprintdom.ValidViewTypes[vt] {
		return nil, sprintdom.ErrViewTypeInvalid
	}
	if !hasPluginConfig(vt, &in.Config) {
		return nil, sprintdom.ErrViewPluginConfigRequired
	}

	now := time.Now()
	v := &sprintdom.SprintView{
		ID:          uuid.New(),
		SprintID:    in.SprintID,
		ProjectID:   in.ProjectID,
		Name:        name,
		ViewType:    vt,
		Config:      in.Config,
		Position:    in.Position,
		ViewContext: in.ViewContext,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	if err := s.repo.CreateView(ctx, v); err != nil {
		return nil, err
	}
	s.publish(ctx, events.TopicViewCreated, viewPayload(v))
	return v, nil
}

// UpdateView updates the mutable fields of an existing view,
// verifying it belongs to projectID.
func (s *ViewService) UpdateView(ctx context.Context, projectID, id uuid.UUID, in sprintdom.UpdateViewInput) (*sprintdom.SprintView, error) {
	v, err := s.repo.FindViewByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if v.ProjectID != projectID {
		return nil, sprintdom.ErrViewNotFound
	}

	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if name == "" {
			return nil, sprintdom.ErrViewNameInvalid
		}
		v.Name = name
	}
	if in.ViewType != nil {
		if !sprintdom.ValidViewTypes[*in.ViewType] {
			return nil, sprintdom.ErrViewTypeInvalid
		}
		v.ViewType = *in.ViewType
	}
	if in.Config != nil {
		v.Config = *in.Config
	}
	if in.Position != nil {
		v.Position = *in.Position
	}
	if !hasPluginConfig(v.ViewType, &v.Config) {
		return nil, sprintdom.ErrViewPluginConfigRequired
	}
	v.UpdatedAt = time.Now()

	if err := s.repo.UpdateView(ctx, v); err != nil {
		return nil, err
	}
	s.publish(ctx, events.TopicViewUpdated, viewPayload(v))
	return v, nil
}

// DeleteView removes a view by ID.  Deletion of the last remaining view is
// rejected with ErrViewIsLastView.  Verifies the view belongs to projectID.
func (s *ViewService) DeleteView(ctx context.Context, projectID, id uuid.UUID) error {
	v, err := s.repo.FindViewByID(ctx, id)
	if err != nil {
		return err
	}
	if v.ProjectID != projectID {
		return sprintdom.ErrViewNotFound
	}

	var count int
	if v.SprintID != nil {
		count, err = s.repo.CountViews(ctx, *v.SprintID)
	} else {
		count, err = s.repo.CountProjectViews(ctx, v.ProjectID, v.ViewContext)
	}
	if err != nil {
		return err
	}
	if count <= 1 {
		return sprintdom.ErrViewIsLastView
	}

	if err := s.repo.DeleteView(ctx, id); err != nil {
		return err
	}
	s.publish(ctx, events.TopicViewDeleted, viewPayload(v))
	return nil
}

// MoveTask updates the manual position of a task within a view,
// verifying the view and the task both belong to projectID.
func (s *ViewService) MoveTask(ctx context.Context, projectID, viewID uuid.UUID, in sprintdom.MoveTaskInput) error {
	v, err := s.repo.FindViewByID(ctx, viewID)
	if err != nil {
		return err
	}
	if v.ProjectID != projectID {
		return sprintdom.ErrViewNotFound
	}
	if err := s.checkTask(ctx, projectID, in.TaskID, taskdom.AccessManage); err != nil {
		return err
	}
	pos := &sprintdom.ViewTaskPosition{
		ID:       uuid.New(),
		ViewID:   viewID,
		TaskID:   in.TaskID,
		Position: in.Position,
		GroupKey: in.GroupKey,
	}
	return s.repo.UpsertTaskPosition(ctx, pos)
}

// BulkMoveTasks updates the manual positions of multiple tasks within a view
// in a single database round-trip.  Verifies the view and every task belong
// to projectID.
func (s *ViewService) BulkMoveTasks(ctx context.Context, projectID, viewID uuid.UUID, items []sprintdom.MoveTaskInput) error {
	v, err := s.repo.FindViewByID(ctx, viewID)
	if err != nil {
		return err
	}
	if v.ProjectID != projectID {
		return sprintdom.ErrViewNotFound
	}
	for _, in := range items {
		if err := s.checkTask(ctx, projectID, in.TaskID, taskdom.AccessManage); err != nil {
			return err
		}
	}
	positions := make([]*sprintdom.ViewTaskPosition, 0, len(items))
	for _, in := range items {
		positions = append(positions, &sprintdom.ViewTaskPosition{
			ID:       uuid.New(),
			ViewID:   viewID,
			TaskID:   in.TaskID,
			Position: in.Position,
			GroupKey: in.GroupKey,
		})
	}
	return s.repo.BulkUpsertTaskPositions(ctx, positions)
}

// ListTaskPositions returns the manual ordering for all tasks in a view,
// verifying the view belongs to projectID.
func (s *ViewService) ListTaskPositions(ctx context.Context, projectID, viewID uuid.UUID) ([]*sprintdom.ViewTaskPosition, error) {
	v, err := s.repo.FindViewByID(ctx, viewID)
	if err != nil {
		return nil, err
	}
	if v.ProjectID != projectID {
		return nil, sprintdom.ErrViewNotFound
	}
	policy, ok := s.access.(taskdom.AccessPolicy)
	if !ok {
		return nil, taskdom.ErrTaskNotFound
	}
	scope, err := policy.ResolveTaskAccess(ctx, projectID)
	if err != nil {
		return nil, err
	}
	repo, ok := s.repo.(interface {
		ListTaskPositionsVisible(context.Context, uuid.UUID, taskdom.AccessScope) ([]*sprintdom.ViewTaskPosition, error)
	})
	if !ok {
		return nil, taskdom.ErrTaskNotFound
	}
	return repo.ListTaskPositionsVisible(ctx, viewID, scope)
}

// ReorderViews reorders all views belonging to a sprint, verifying the
// sprint belongs to projectID.  viewIDs must contain exactly the IDs of all
// views for that sprint in the desired display order.
func (s *ViewService) ReorderViews(ctx context.Context, projectID, sprintID uuid.UUID, viewIDs []uuid.UUID) error {
	if err := s.sprintInProject(ctx, projectID, sprintID); err != nil {
		return err
	}
	existing, err := s.repo.ListViews(ctx, sprintID)
	if err != nil {
		return err
	}
	if err := s.validateAndReorder(ctx, existing, viewIDs); err != nil {
		return err
	}
	if len(existing) > 0 {
		payload := map[string]any{
			"project_id":   existing[0].ProjectID.String(),
			"view_context": string(existing[0].ViewContext),
			"sprint_id":    sprintID.String(),
		}
		s.publish(ctx, events.TopicViewReordered, payload)
	}
	return nil
}

// ReorderProjectViews reorders all views for a project+context.
// viewIDs must contain exactly the IDs of all views for that project+context in the desired order.
func (s *ViewService) ReorderProjectViews(ctx context.Context, projectID uuid.UUID, viewCtx sprintdom.ViewContext, viewIDs []uuid.UUID) error {
	existing, err := s.repo.ListProjectViews(ctx, projectID, viewCtx)
	if err != nil {
		return err
	}
	if err := s.validateAndReorder(ctx, existing, viewIDs); err != nil {
		return err
	}
	if len(existing) > 0 {
		s.publish(ctx, events.TopicViewReordered, map[string]any{
			"project_id":   projectID.String(),
			"view_context": string(viewCtx),
		})
	}
	return nil
}

// validateAndReorder checks that viewIDs exactly matches the IDs of existing
// views (same count, no unknowns) then persists the new positions.
func (s *ViewService) validateAndReorder(ctx context.Context, existing []*sprintdom.SprintView, viewIDs []uuid.UUID) error {
	if len(viewIDs) != len(existing) {
		return sprintdom.ErrViewReorderInvalid
	}
	existingSet := make(map[uuid.UUID]struct{}, len(existing))
	for _, v := range existing {
		existingSet[v.ID] = struct{}{}
	}
	items := make([]sprintdom.ViewReorderItem, 0, len(viewIDs))
	for i, id := range viewIDs {
		if _, ok := existingSet[id]; !ok {
			return sprintdom.ErrViewReorderInvalid
		}
		items = append(items, sprintdom.ViewReorderItem{ID: id, Position: float64(i)})
	}
	return s.repo.ReorderViews(ctx, items)
}
