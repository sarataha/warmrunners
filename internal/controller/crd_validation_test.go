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

package controller

import (
	"os"
	"path/filepath"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	warmrunnersv1alpha1 "github.com/sarataha/warmrunners/api/v1alpha1"
	"sigs.k8s.io/yaml"
)

// validBase returns a syntactically-valid WarmRunnerPolicy that each test can
// then mutate in a single way to trigger one validation failure.
func validBase(name string) *warmrunnersv1alpha1.WarmRunnerPolicy {
	return &warmrunnersv1alpha1.WarmRunnerPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: warmrunnersv1alpha1.WarmRunnerPolicySpec{
			GitHub: warmrunnersv1alpha1.GitHubConfig{
				Owner:      "my-org",
				Repository: "my-repo",
				Labels:     []string{"self-hosted", "linux", "x64"},
				Auth: warmrunnersv1alpha1.AuthRef{
					SecretRef: corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "gh-token"},
						Key:                  "token",
					},
				},
			},
			Target: warmrunnersv1alpha1.Target{
				Arc: &warmrunnersv1alpha1.ArcTarget{
					RunnerSet: warmrunnersv1alpha1.RefNS{Name: "prod", Namespace: "arc-system"},
				},
			},
			Floor: warmrunnersv1alpha1.FloorRange{Min: 0, Max: 5},
			Schedule: []warmrunnersv1alpha1.ScheduleWindow{
				{
					Days: []string{"Mon"},
					From: "08:00",
					To:   "19:00",
					TZ:   "UTC",
					Base: 1,
				},
			},
			QueueRule: warmrunnersv1alpha1.QueueRule{
				PollInterval: metav1.Duration{Duration: 30000000000},
				Cooldown:     metav1.Duration{Duration: 120000000000},
			},
		},
	}
}

var _ = Describe("WarmRunnerPolicy CRD validation", func() {
	It("rejects floor.min > floor.max", func() {
		p := validBase("invalid-floor")
		p.Spec.Floor = warmrunnersv1alpha1.FloorRange{Min: 10, Max: 5}
		err := k8sClient.Create(ctx, p)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("min must be <= max"))
	})

	It("rejects schedule.from >= schedule.to", func() {
		p := validBase("invalid-schedule")
		p.Spec.Schedule[0].From = "19:00"
		p.Spec.Schedule[0].To = "08:00"
		err := k8sClient.Create(ctx, p)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("from must be earlier than to"))
	})

	It("rejects malformed HH:MM (24:00)", func() {
		p := validBase("invalid-hhmm-24")
		p.Spec.Schedule[0].From = "24:00"
		err := k8sClient.Create(ctx, p)
		Expect(err).To(HaveOccurred())
	})

	It("rejects malformed HH:MM (7:5)", func() {
		p := validBase("invalid-hhmm-short")
		p.Spec.Schedule[0].From = "7:5"
		err := k8sClient.Create(ctx, p)
		Expect(err).To(HaveOccurred())
	})

	It("rejects invalid IANA timezone", func() {
		p := validBase("invalid-tz")
		// Pattern is structural, not a real IANA lookup, but "Mars/Phobos" still
		// matches our shape; pick something that breaks the regex instead.
		p.Spec.Schedule[0].TZ = "Mars/Phobos/Deimos/Extra"
		err := k8sClient.Create(ctx, p)
		Expect(err).To(HaveOccurred())
	})

	It("accepts the example arc policy", func() {
		raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "policy-arc.yaml"))
		Expect(err).NotTo(HaveOccurred())
		obj := &warmrunnersv1alpha1.WarmRunnerPolicy{}
		Expect(yaml.Unmarshal(raw, obj)).To(Succeed())
		obj.Name = "example-arc-accepted"
		obj.ResourceVersion = ""
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
	})

	It("accepts the example garm policy", func() {
		raw, err := os.ReadFile(filepath.Join("..", "..", "examples", "policy-garm.yaml"))
		Expect(err).NotTo(HaveOccurred())
		obj := &warmrunnersv1alpha1.WarmRunnerPolicy{}
		Expect(yaml.Unmarshal(raw, obj)).To(Succeed())
		obj.Name = "example-garm-accepted"
		obj.ResourceVersion = ""
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
	})
})
