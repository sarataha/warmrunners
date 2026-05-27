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
	Owner      string `json:"owner"`
	Repository string `json:"repository,omitempty"`
	// +kubebuilder:validation:MinItems=1
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
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// Exactly one of Arc or Garm MUST be set. Validated by the controller and at admission.
// +kubebuilder:validation:XValidation:rule="(has(self.arc) ? 1 : 0) + (has(self.garm) ? 1 : 0) == 1",message="exactly one of target.arc or target.garm must be set"
type Target struct {
	Arc  *ArcTarget  `json:"arc,omitempty"`
	Garm *GarmTarget `json:"garm,omitempty"`
}

type FloorRange struct {
	// +kubebuilder:validation:Minimum=0
	Min int32 `json:"min"`
	// +kubebuilder:validation:Minimum=0
	Max int32 `json:"max"`
}

type ScheduleWindow struct {
	// +kubebuilder:validation:MinItems=1
	Days []string `json:"days"`
	From string   `json:"from"` // "HH:MM"
	To   string   `json:"to"`   // "HH:MM"
	TZ   string   `json:"tz"`   // IANA name, e.g. "UTC", "Europe/London"
	// +kubebuilder:validation:Minimum=0
	Base int32 `json:"base"`
}

type HeadroomTier struct {
	// +kubebuilder:validation:Minimum=1
	WhenQueueAtLeast int32 `json:"whenQueueAtLeast"`
	// +kubebuilder:validation:Minimum=0
	AddRunners int32 `json:"addRunners"`
}

type QueueRule struct {
	// +kubebuilder:default="30s"
	PollInterval metav1.Duration `json:"pollInterval"`
	Headroom     []HeadroomTier  `json:"headroom,omitempty"`
	// +kubebuilder:default="2m"
	Cooldown metav1.Duration `json:"cooldown"`
}

type WarmRunnerPolicySpec struct {
	GitHub    GitHubConfig     `json:"github"`
	Target    Target           `json:"target"`
	Floor     FloorRange       `json:"floor"`
	Schedule  []ScheduleWindow `json:"schedule,omitempty"`
	QueueRule QueueRule        `json:"queueRule"`
}

type WarmRunnerPolicyStatus struct {
	DesiredFloor      int32        `json:"desiredFloor,omitempty"`
	AppliedFloor      int32        `json:"appliedFloor,omitempty"`
	LastQueueDepth    int32        `json:"lastQueueDepth,omitempty"`
	LastReconcileTime *metav1.Time `json:"lastReconcileTime,omitempty"`
	// LastDecreaseTime is when the warm-floor was last decreased. It feeds the
	// scheduler's cooldown so decreases are rate-limited independently of the
	// per-poll reconcile time.
	LastDecreaseTime *metav1.Time       `json:"lastDecreaseTime,omitempty"`
	Conditions       []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.status.desiredFloor`
// +kubebuilder:printcolumn:name="Applied",type=integer,JSONPath=`.status.appliedFloor`
// +kubebuilder:printcolumn:name="Queue",type=integer,JSONPath=`.status.lastQueueDepth`
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
