package worker

import (
	"context"
	automationdom "github.com/Paca-AI/api/internal/domain/automation"
	sprintdom "github.com/Paca-AI/api/internal/domain/sprint"
	"github.com/google/uuid"
	"testing"
)

func TestReviewResumeRejectsTopologyDeletion(t *testing.T) {
	c, repo, _, _ := newWaitPauseFixture(t)
	var delay *automationdom.PendingDelay
	for _, d := range repo.pendingDelays {
		delay = d
	}
	// Deleting the current outgoing edge leaves no surviving creation timestamp.
	repo.edges = nil
	repo.automation.GraphRevision++ // mirrors the production deletion trigger
	if err := c.resumeAfterDelay(context.Background(), delay); err == nil {
		t.Fatal("changed graph accepted: scheduler would delete pending delay and finalize run")
	}
}
func TestReviewResumeRejectsDifferentExistingWaitNode(t *testing.T) {
	c, repo, updater, _ := newWaitPauseFixture(t)
	var delay *automationdom.PendingDelay
	for _, d := range repo.pendingDelays {
		delay = d
	}
	original := repo.nodes[0]
	alternate := *original
	alternate.ID = uuid.New()
	// Another already-existing wait in this automation was not visited by this run.
	repo.nodes = append(repo.nodes, &alternate)
	repo.edges = append(repo.edges, &automationdom.Edge{ID: uuid.New(), AutomationID: repo.automation.ID, SourceNodeID: alternate.ID, TargetNodeID: repo.nodes[1].ID})
	delay.NodeID = alternate.ID
	if err := c.resumeAfterDelay(context.Background(), delay); err == nil {
		t.Fatalf("unvisited pause node accepted; downstream task writes=%d", updater.calls)
	}
}

func TestReviewTaskResumeRejectsInjectedSprint(t *testing.T) {
	c, repo, updater, _ := newWaitPauseFixture(t)
	var delay *automationdom.PendingDelay
	for _, d := range repo.pendingDelays {
		delay = d
	}
	// Task-triggered walks persist no explicit SprintID; the task has no sprint.
	injected := &sprintdom.Sprint{ID: uuid.New(), ProjectID: delay.ProjectID}
	c.sprintRepo = &fakeSprintReader{byID: map[uuid.UUID]*sprintdom.Sprint{injected.ID: injected}}
	delay.Context.SprintID = &injected.ID
	if err := c.resumeAfterDelay(context.Background(), delay); err == nil {
		t.Fatalf("unbound same-project sprint accepted; downstream task writes=%d", updater.calls)
	}
}
