package config

import (
	"strings"
	"testing"
)

func TestValidatePortRange(t *testing.T) {
	cases := []struct {
		name       string
		start, end int
		wantErr    bool
	}{
		{"both zero disables the feature", 0, 0, false},
		{"valid range", 2200, 2299, false},
		{"start equals end", 2200, 2200, false},
		{"only start set", 2200, 0, true},
		{"only end set", 0, 2299, true},
		{"end before start", 2299, 2200, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePortRange("SSH_BASTION_PORT_RANGE", tc.start, tc.end)
			if tc.wantErr && err == nil {
				t.Errorf("validatePortRange(%d, %d) = nil, want error", tc.start, tc.end)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validatePortRange(%d, %d) = %v, want nil", tc.start, tc.end, err)
			}
		})
	}
}

func TestLoad_RequiresStableInstanceID(t *testing.T) {
	for _, tc := range []struct{ value, want string }{
		{"", ""},
		{"not-a-uuid", ""},
		{"00000000-0000-0000-0000-000000000000", ""},
		{"11111111-2222-4333-8444-555555555555", "11111111-2222-4333-8444-555555555555"},
		{"AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE", "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("PACA_INSTANCE_ID", tc.value)
			t.Setenv("INTERNAL_API_KEY", "synthetic-test-key")
			t.Setenv("DATABASE_URL", "postgres://test")
			t.Setenv("VALKEY_URL", "redis://localhost:6379")
			t.Setenv("ENCRYPTION_KEY", strings.Repeat("a", 64))
			t.Setenv("AGENT_SERVER_IMAGE", "synthetic-test")
			t.Setenv("AGENT_RUNNER_ALLOWED_AGENT_IDS", "*")
			cfg, err := Load()
			if tc.want != "" {
				if err != nil {
					t.Fatal(err)
				}
				if cfg.InstanceID != tc.want {
					t.Fatalf("instance = %q, want canonical %q", cfg.InstanceID, tc.want)
				}
			} else if err == nil || !strings.Contains(err.Error(), "PACA_INSTANCE_ID") {
				t.Fatalf("invalid instance must fail before startup: %v", err)
			}
		})
	}
}
