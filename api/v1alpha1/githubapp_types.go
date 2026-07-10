package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:validation:Enum=ingress;tunnel
type GitHubAppIngressMode string

const (
	IngressModeIngress GitHubAppIngressMode = "ingress"
	IngressModeTunnel  GitHubAppIngressMode = "tunnel"
)

type SecretKeyRef struct {
	// +kubebuilder:validation:Required
	Name string `json:"name"`
	// +kubebuilder:validation:Required
	Key string `json:"key"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

type TunnelSpec struct {
	// +kubebuilder:validation:Pattern=`^wss://.+`
	RelayURL string `json:"relayURL"`
}

type GitHubAppIngress struct {
	// +kubebuilder:default=ingress
	Mode GitHubAppIngressMode `json:"mode"`
	// +optional
	Hostname string `json:"hostname,omitempty"`
	// +optional
	Tunnel *TunnelSpec `json:"tunnel,omitempty"`
}

type GitHubAppSpec struct {
	// +kubebuilder:validation:Minimum=1
	AppID int64 `json:"appID"`
	// +kubebuilder:validation:Required
	PrivateKeyRef SecretKeyRef `json:"privateKeyRef"`
	// +kubebuilder:validation:Required
	WebhookSecretRef SecretKeyRef `json:"webhookSecretRef"`
	// +kubebuilder:validation:Required
	Ingress GitHubAppIngress `json:"ingress"`
}

type Installation struct {
	ID           int64  `json:"id"`
	Account      string `json:"account"`
	Repositories int32  `json:"repositories"`
}

type GitHubAppStatus struct {
	// +optional
	Installations []Installation `json:"installations,omitempty"`
	// +optional
	WebhookHealthy bool `json:"webhookHealthy,omitempty"`
	// +optional
	LastDelivery *metav1.Time `json:"lastDelivery,omitempty"`
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,path=githubapps,singular=githubapp,shortName=gha,categories={warmrunners}
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="App",type=integer,JSONPath=`.spec.appID`
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.ingress.mode`
// +kubebuilder:printcolumn:name="Healthy",type=boolean,JSONPath=`.status.webhookHealthy`
// +kubebuilder:printcolumn:name="LastDelivery",type=date,JSONPath=`.status.lastDelivery`
type GitHubApp struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              GitHubAppSpec   `json:"spec,omitempty"`
	Status            GitHubAppStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type GitHubAppList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GitHubApp `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GitHubApp{}, &GitHubAppList{})
}
