// internal/activity/activity_test.go
package activity

import "testing"

func TestIsBotActor(t *testing.T) {
	// A defensive denylist entry without [bot] suffix — none exist in
	// BuiltinDenylist today but the helper must still attribute correctly.
	customDeny := []string{"snyk-bot", "internal-ci"}

	cases := []struct {
		name           string
		actorType      string
		triggeringType string
		login          string
		denylist       []string
		wantIsBot      bool
		wantReason     string
	}{
		{
			name:      "human user, empty denylist",
			actorType: "User", triggeringType: "User",
			login: "alice", denylist: nil,
			wantIsBot: false, wantReason: "",
		},
		{
			name:      "actor type Bot wins",
			actorType: "Bot", triggeringType: "User",
			login: "somebot", denylist: nil,
			wantIsBot: true, wantReason: "bot_type",
		},
		{
			name:      "triggering actor Bot when actor User",
			actorType: "User", triggeringType: "Bot",
			login: "alice", denylist: nil,
			wantIsBot: true, wantReason: "trigger_bot_type",
		},
		{
			name:      "dependabot[bot] login matches suffix before denylist",
			actorType: "User", triggeringType: "User",
			login: "dependabot[bot]", denylist: BuiltinDenylist,
			wantIsBot: true, wantReason: "bot_suffix",
		},
		{
			name:      "novel [bot] suffix not in builtin",
			actorType: "User", triggeringType: "User",
			login: "custom-thing[bot]", denylist: BuiltinDenylist,
			wantIsBot: true, wantReason: "bot_suffix",
		},
		{
			name:      "user-supplied denylist match (no suffix)",
			actorType: "User", triggeringType: "User",
			login: "snyk-bot", denylist: customDeny,
			wantIsBot: true, wantReason: "denylist",
		},
		{
			name:      "defensive: builtin-shape login without suffix in denylist",
			actorType: "User", triggeringType: "User",
			login: "internal-ci", denylist: customDeny,
			wantIsBot: true, wantReason: "denylist",
		},
		{
			name:      "empty login never classifies as bot",
			actorType: "Bot", triggeringType: "Bot",
			login: "", denylist: BuiltinDenylist,
			wantIsBot: false, wantReason: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isBot, reason := IsBotActor(tc.actorType, tc.triggeringType, tc.login, tc.denylist)
			if isBot != tc.wantIsBot || reason != tc.wantReason {
				t.Fatalf("IsBotActor(%q,%q,%q,%v) = (%v,%q), want (%v,%q)",
					tc.actorType, tc.triggeringType, tc.login, tc.denylist,
					isBot, reason, tc.wantIsBot, tc.wantReason)
			}
		})
	}
}

func TestBuiltinDenylistShape(t *testing.T) {
	// Spec §3.5 enumerates the seven builtin entries. Catching a typo or an
	// accidental removal here is cheaper than tracking it down via a missed
	// bot filter in a manual kind run.
	want := []string{
		"dependabot[bot]",
		"renovate[bot]",
		"github-actions[bot]",
		"mergify[bot]",
		"codecov[bot]",
		"copilot-pull-request-reviewer[bot]",
		"self-hosted-renovate[bot]",
	}
	if len(BuiltinDenylist) != len(want) {
		t.Fatalf("BuiltinDenylist len = %d, want %d (%v)", len(BuiltinDenylist), len(want), BuiltinDenylist)
	}
	for i, w := range want {
		if BuiltinDenylist[i] != w {
			t.Errorf("BuiltinDenylist[%d] = %q, want %q", i, BuiltinDenylist[i], w)
		}
	}
}
