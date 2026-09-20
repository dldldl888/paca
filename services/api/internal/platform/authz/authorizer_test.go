package authz_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Paca-AI/api/internal/platform/authz"
)

type stubPermissionStore struct {
	globalPerms  []authz.Permission
	projectPerms []authz.Permission
	globalErr    error
}

func (s *stubPermissionStore) ListGlobalPermissions(context.Context, uuid.UUID) ([]authz.Permission, error) {
	return s.globalPerms, s.globalErr
}

func (s *stubPermissionStore) ListProjectPermissions(context.Context, uuid.UUID, uuid.UUID) ([]authz.Permission, error) {
	return s.projectPerms, nil
}

func TestAuthorizerExplicitPermissionsOverrideLegacyName(t *testing.T) {
	ctx := context.Background()
	userID := uuid.New()

	for _, tc := range []struct {
		name  string
		store authz.PermissionStore
		role  string
	}{
		{name: "ADMIN with empty persisted permissions", store: &stubPermissionStore{}, role: "ADMIN"},
		{name: "ADMIN with only users.read", store: &stubPermissionStore{globalPerms: []authz.Permission{authz.PermissionUsersRead}}, role: "ADMIN"},
		{name: "SUPER_ADMIN claim with empty persisted permissions", store: &stubPermissionStore{}, role: "SUPER_ADMIN"},
		{name: "ADMIN without permission store", role: "ADMIN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			allowed, err := authz.NewAuthorizer(tc.store).HasPermissions(ctx, userID, nil, tc.role, authz.PermissionSettingsWrite)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if allowed {
				t.Fatal("expected role name alone to be denied")
			}
		})
	}

	t.Run("explicit wildcard allows", func(t *testing.T) {
		store := &stubPermissionStore{globalPerms: []authz.Permission{authz.PermissionAll}}
		allowed, err := authz.NewAuthorizer(store).HasPermissions(ctx, userID, nil, "USER", authz.PermissionSettingsWrite)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !allowed {
			t.Fatal("expected persisted wildcard permission to allow")
		}
	})

	t.Run("revocation affects the next check", func(t *testing.T) {
		store := &stubPermissionStore{globalPerms: []authz.Permission{authz.PermissionSettingsWrite}}
		a := authz.NewAuthorizer(store)

		allowed, err := a.HasPermissions(ctx, userID, nil, "ADMIN", authz.PermissionSettingsWrite)
		if err != nil || !allowed {
			t.Fatalf("expected initial persisted grant to allow, allowed=%v err=%v", allowed, err)
		}

		store.globalPerms = nil
		allowed, err = a.HasPermissions(ctx, userID, nil, "ADMIN", authz.PermissionSettingsWrite)
		if err != nil {
			t.Fatalf("unexpected error after revocation: %v", err)
		}
		if allowed {
			t.Fatal("expected revoked permission to deny the next check")
		}
	})

	t.Run("store error does not fall back to role name", func(t *testing.T) {
		wantErr := errors.New("permission store failed")
		store := &stubPermissionStore{globalErr: wantErr}
		allowed, err := authz.NewAuthorizer(store).HasPermissions(ctx, userID, nil, "ADMIN", authz.PermissionSettingsWrite)
		if allowed {
			t.Fatal("expected store failure to deny")
		}
		if !errors.Is(err, wantErr) {
			t.Fatalf("expected store error, got %v", err)
		}
	})
}

func TestAuthorizer_GlobalAndProjectPermissions(t *testing.T) {
	projectID := uuid.New()
	a := authz.NewAuthorizer(&stubPermissionStore{
		globalPerms:  []authz.Permission{authz.PermissionGlobalRolesRead},
		projectPerms: []authz.Permission{authz.PermissionTasksWrite},
	})

	ok, err := a.HasPermissions(context.Background(), uuid.New(), &projectID, "USER", authz.PermissionTasksWrite)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected project permission to authorize")
	}
}

func TestAuthorizer_WildcardMatch(t *testing.T) {
	a := authz.NewAuthorizer(&stubPermissionStore{globalPerms: []authz.Permission{authz.PermissionTasksAll}})
	ok, err := a.HasPermissions(context.Background(), uuid.New(), nil, "USER", authz.PermissionTasksWrite)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected tasks.* to authorize tasks.write")
	}
}
