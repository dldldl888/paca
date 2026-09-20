package docdom

import (
	"context"
	"github.com/google/uuid"
)

type AccessAction uint8

const (
	AccessRead AccessAction = iota
	AccessWrite
)

type AccessChecker interface {
	CheckDocumentAccess(context.Context, uuid.UUID, uuid.UUID, AccessAction) (*Document, error)
}
type UpdateAccessInput struct {
	ExpectedAccessVersion int64       `json:"expected_access_version"`
	Visibility            string      `json:"visibility"`
	TaskIDs               []uuid.UUID `json:"task_ids"`
}
type AccessRepository interface {
	AccessChecker
	UpdateDocumentAccess(context.Context, uuid.UUID, uuid.UUID, UpdateAccessInput) (*Document, error)
}
