/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type GitHubConfig struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=253
	Owner string `json:"owner"`
	// Repository is required: v1 supports repo-level polling only. An empty
	// value would build an invalid org-level URL (/repos/owner//actions/runs).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Repository string `json:"repository"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=64
	// +listType=atomic
	// +kubebuilder:validation:items:MaxLength=64
	Labels []string `json:"labels"`
	Auth   AuthRef  `json:"auth"`
}

type AuthRef struct {
	SecretRef corev1.SecretKeySelector `json:"secretRef"`
}

type ArcTarget struct {
	RunnerSet RefNS `json:"runnerSet"`
}

type GarmTarget struct {
	Pool RefNS `json:"pool"`
}

type RefNS struct {
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// +kubebuilder:validation:MaxLength=253
	Namespace string `json:"namespace"`
}

// Exactly one of Arc or Garm MUST be set. Validated by the controller and at admission.
// +kubebuilder:validation:XValidation:rule="(has(self.arc) ? 1 : 0) + (has(self.garm) ? 1 : 0) == 1",message="exactly one of target.arc or target.garm must be set"
type Target struct {
	Arc  *ArcTarget  `json:"arc,omitempty"`
	Garm *GarmTarget `json:"garm,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.min <= self.max",message="min must be <= max"
type FloorRange struct {
	// +kubebuilder:validation:Minimum=0
	Min int32 `json:"min"`
	// +kubebuilder:validation:Minimum=0
	Max int32 `json:"max"`
}

// +kubebuilder:validation:XValidation:rule="self.from < self.to",message="from must be earlier than to"
type ScheduleWindow struct {
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=7
	// +listType=atomic
	// +kubebuilder:validation:items:MaxLength=16
	Days []string `json:"days"`
	// +kubebuilder:validation:Pattern=`^([01][0-9]|2[0-3]):[0-5][0-9]$`
	// +kubebuilder:validation:MaxLength=5
	From string `json:"from"` // "HH:MM"
	// +kubebuilder:validation:Pattern=`^([01][0-9]|2[0-3]):[0-5][0-9]$`
	// +kubebuilder:validation:MaxLength=5
	To string `json:"to"` // "HH:MM"
	// +kubebuilder:validation:Pattern=`^[A-Za-z]+(?:/[A-Za-z0-9_+\-]+){0,2}$`
	// +kubebuilder:validation:MaxLength=64
	TZ string `json:"tz"` // IANA name, e.g. "UTC", "Europe/London"
	// +kubebuilder:validation:Minimum=0
	Base int32 `json:"base"`
}

type QueueRule struct {
	// +kubebuilder:default="30s"
	PollInterval metav1.Duration `json:"pollInterval"`
	// +kubebuilder:default="2m"
	Cooldown metav1.Duration `json:"cooldown"`
}

// PredictorConfig configures the codebase-aware Predictor (v0.2.0).
type PredictorConfig struct {
	// +kubebuilder:default=true
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// +kubebuilder:default="5m"
	// +optional
	WorkflowRefreshInterval metav1.Duration `json:"workflowRefreshInterval,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=500
	// +kubebuilder:default=50
	// +optional
	MaxRunsPerPoll int32 `json:"maxRunsPerPoll,omitempty"`
}

// PredictedLabelSet is one entry in Status.PredictedLabelSets.
type PredictedLabelSet struct {
	Labels []string `json:"labels"`
	Count  int32    `json:"count"`
}

// ActivityConfig configures the activity-driven warm-floor signal (v0.3.0).
type ActivityConfig struct {
	// +kubebuilder:default=true
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// WindowSeconds is the rolling window over which recent non-bot
	// workflow_runs are counted. Defaults to 900 (15 minutes); see spec §3.4.
	//
	// +kubebuilder:validation:Minimum=60
	// +kubebuilder:validation:Maximum=7200
	// +kubebuilder:default=900
	// +optional
	WindowSeconds int32 `json:"windowSeconds,omitempty"`

	// BotLoginDenylist is appended to the built-in denylist (dependabot[bot],
	// renovate[bot], github-actions[bot], mergify[bot], codecov[bot],
	// copilot-pull-request-reviewer[bot], self-hosted-renovate[bot]). Use
	// for PAT-driven machine users that lack the [bot] suffix (e.g.
	// "snyk-bot", "getsentry-bot", in-house "*-ci" / "*-deploy" accounts).
	// Entries match workflow_run.actor.login exactly (case-sensitive).
	//
	// +kubebuilder:validation:MaxItems=64
	// +listType=set
	// +optional
	BotLoginDenylist []string `json:"botLoginDenylist,omitempty"`
}

// Reason strings for the ActivityAvailable status condition (v0.3.0).
const (
	ActivityConditionReasonAvailable   = "Available"
	ActivityConditionReasonSampleError = "SampleError"
)

type WarmRunnerPolicySpec struct {
	GitHub GitHubConfig `json:"github"`
	Target Target       `json:"target"`
	Floor  FloorRange   `json:"floor"`
	// +kubebuilder:validation:MaxItems=64
	Schedule  []ScheduleWindow `json:"schedule,omitempty"`
	QueueRule QueueRule        `json:"queueRule"`
	// +optional
	Predictor *PredictorConfig `json:"predictor,omitempty"`
	// +optional
	Activity *ActivityConfig `json:"activity,omitempty"`
	// DryRun stops the controller from patching the backend's warm-floor field
	// while keeping every other signal (demand poll, predictor, activity,
	// scheduler, status, metrics) live. Use to canary a new policy and watch
	// what the controller WOULD apply before letting it act. Default false.
	//
	// +kubebuilder:default=false
	// +optional
	DryRun bool `json:"dryRun,omitempty"`
}

type WarmRunnerPolicyStatus struct {
	DesiredFloor      int32        `json:"desiredFloor,omitempty"`
	AppliedFloor      int32        `json:"appliedFloor,omitempty"`
	LastQueueDepth    int32        `json:"lastQueueDepth,omitempty"`
	LastReconcileTime *metav1.Time `json:"lastReconcileTime,omitempty"`
	// LastDecreaseTime is when the warm-floor was last decreased. It feeds the
	// scheduler's cooldown so decreases are rate-limited independently of the
	// per-poll reconcile time.
	LastDecreaseTime *metav1.Time `json:"lastDecreaseTime,omitempty"`
	// +optional
	PredictedFloor int32 `json:"predictedFloor,omitempty"`

	// +listType=atomic
	// +optional
	PredictedLabelSets []PredictedLabelSet `json:"predictedLabelSets,omitempty"`

	// +optional
	ActivityFloor int32 `json:"activityFloor,omitempty"`

	// +listType=atomic
	// +optional
	ActivityLabelSets []PredictedLabelSet `json:"activityLabelSets,omitempty"`

	// DryRun mirrors spec.dryRun so operators can confirm at a glance that
	// the controller is in observe-only mode.
	// +optional
	DryRun bool `json:"dryRun,omitempty"`

	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// Condition type + reason strings for the dry-run signal (v0.4.0).
const (
	DryRunConditionType           = "DryRun"
	DryRunConditionReasonActive   = "Active"
	DryRunConditionReasonInactive = "Inactive"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=warmrunnerpolicies,singular=warmrunnerpolicy,shortName=wrp,categories={warmrunners}
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.status.desiredFloor`
// +kubebuilder:printcolumn:name="Applied",type=integer,JSONPath=`.status.appliedFloor`
// +kubebuilder:printcolumn:name="Queue",type=integer,JSONPath=`.status.lastQueueDepth`
// +kubebuilder:printcolumn:name="Predicted",type=integer,JSONPath=`.status.predictedFloor`
// +kubebuilder:printcolumn:name="Active",type=integer,JSONPath=`.status.activityFloor`
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=`.metadata.creationTimestamp`
type WarmRunnerPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              WarmRunnerPolicySpec   `json:"spec,omitempty"`
	Status            WarmRunnerPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type WarmRunnerPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WarmRunnerPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&WarmRunnerPolicy{}, &WarmRunnerPolicyList{})
}

// Kind returns "arc" or "garm" when exactly one target is set, "" otherwise.
func (t Target) Kind() string {
	arc, garm := t.Arc != nil, t.Garm != nil
	if arc && !garm {
		return "arc"
	}
	if garm && !arc {
		return "garm"
	}
	return ""
}
