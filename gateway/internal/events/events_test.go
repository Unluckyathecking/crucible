package events

import "testing"

// TestEventTypeConstants locks the wire string for every event type. A rename
// of any constant's value is a breaking change for registered webhook
// consumers, so this test must fail if one changes.
func TestEventTypeConstants(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"SubscriptionUpdated", SubscriptionUpdated, "subscription.updated"},
		{"SubscriptionDeleted", SubscriptionDeleted, "subscription.deleted"},
		{"QuotaExceeded", QuotaExceeded, "quota.exceeded"},
		{"APIKeyRotated", APIKeyRotated, "api_key.rotated"},
		{"APIKeyRevoked", APIKeyRevoked, "api_key.revoked"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
			}
		})
	}
}

func TestAllEventTypesMatchesConstants(t *testing.T) {
	want := map[string]bool{
		SubscriptionUpdated: true,
		SubscriptionDeleted: true,
		QuotaExceeded:       true,
		APIKeyRotated:       true,
		APIKeyRevoked:       true,
	}
	if len(AllEventTypes) != len(want) {
		t.Fatalf("AllEventTypes has %d entries, want %d", len(AllEventTypes), len(want))
	}
	seen := make(map[string]bool, len(AllEventTypes))
	for _, et := range AllEventTypes {
		if !want[et] {
			t.Errorf("AllEventTypes contains unexpected event type %q", et)
		}
		if seen[et] {
			t.Errorf("AllEventTypes contains duplicate event type %q", et)
		}
		seen[et] = true
	}
}
