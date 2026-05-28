// Package activity samples recent CI activity in a repository and produces a
// per-label-set demand snapshot. It is the third warm-floor signal in
// warmrunners v0.3.0, sitting alongside the schedule-window (`internal/scheduler`)
// and codebase-aware predictor (`internal/predictor`) signals. The reconciler
// folds all three into a single `max(...)` floor.
//
// Unlike the predictor — which walks the needs: DAG of currently-active runs
// to size imminent demand — the activity sampler answers a simpler question:
// "did a human just push something CI-relevant in the last N minutes?". A
// non-empty answer keeps the floor up at the magnitude of the triggered
// workflows' fanout; an empty answer lets the floor drain via cooldown.
//
// # Bot filter
//
// `IsBotActor` and `BuiltinDenylist` implement spec §3.5. The builtin list
// captures the well-known GitHub-App bot logins; the user-supplied
// `spec.activity.botLoginDenylist` is *appended* (merge, not replace) so
// upgrading warmrunners never silently re-admits a previously-filtered bot.
//
// # Concurrency
//
// The Activity interface itself makes no concurrency promise; individual
// implementations document their own. Sample.PerLabelSet is a plain map and
// must not be mutated concurrently with reads.
package activity

import (
	"context"
	"strings"
	"time"
)

// Activity samples recent CI activity in a repo and returns per-label-set
// fanout demand. Implementations must honor ctx.Done() and never block past
// the caller's timeout. token is the per-policy GitHub credential; window
// bounds how far back the implementation looks; denylist is the merged
// built-in + user-supplied bot login list.
type Activity interface {
	Sample(ctx context.Context, owner, repository, token string,
		window time.Duration, denylist []string) (Sample, error)
}

// Sample is the per-poll output. PerLabelSet keys are predictor.LabelSetKey
// of the runs-on labels; values are the max-fanout count derived from the
// triggered workflow's YAML at head_sha.
type Sample struct {
	PerLabelSet map[string]int
}

// BuiltinDenylist is the compiled-in set of well-known bot actor logins.
// User-supplied entries from spec.activity.botLoginDenylist are appended
// to (not replacing) this list before being passed to Sample.
var BuiltinDenylist = []string{
	"dependabot[bot]",
	"renovate[bot]",
	"github-actions[bot]",
	"mergify[bot]",
	"codecov[bot]",
	"copilot-pull-request-reviewer[bot]",
	"self-hosted-renovate[bot]",
}

// IsBotActor decides whether a workflow_run is bot-driven and should be
// excluded from the activity signal.
//
// Precedence (first match wins; the reason is what the metrics layer in PR 2
// reports as the `reason` label on `warmrunners_activity_bot_filtered_total`):
//
//  1. actorType == "Bot" → ("bot_type")
//  2. triggeringActorType == "Bot" → ("trigger_bot_type")
//  3. actorLogin ends with "[bot]" → ("bot_suffix")
//  4. actorLogin appears in denylist → ("denylist")
//
// Empty actorLogin returns (false, "") so a malformed run never silently
// classifies as a bot — drop those at the source rather than in this helper.
//
// The suffix check intentionally runs BEFORE the denylist check: most builtin
// denylist entries (`dependabot[bot]`, etc.) carry the `[bot]` suffix, and we
// want the generic-bot reason ("bot_suffix") to win over the named-entry
// reason ("denylist") so a future bot we forgot to list still attributes
// correctly in metrics.
func IsBotActor(actorType, triggeringActorType, actorLogin string, denylist []string) (bool, string) {
	if actorLogin == "" {
		return false, ""
	}
	if actorType == "Bot" {
		return true, "bot_type"
	}
	if triggeringActorType == "Bot" {
		return true, "trigger_bot_type"
	}
	if strings.HasSuffix(actorLogin, "[bot]") {
		return true, "bot_suffix"
	}
	for _, d := range denylist {
		if d == actorLogin {
			return true, "denylist"
		}
	}
	return false, ""
}
