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
	"net/http"
	"net/http/httptest"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	warmrunnersv1alpha1 "github.com/sarataha/warmrunners/api/v1alpha1"
	"github.com/sarataha/warmrunners/internal/demand"
	"github.com/sarataha/warmrunners/internal/scheduler"
)

var _ = Describe("WarmRunnerPolicy full-loop integration", func() {
	Context("end-to-end reconcile with real GitHubRESTPoller", func() {
		const resourceName = "integration-arc"

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: "default",
		}

		var srv *httptest.Server

		BeforeEach(func() {
			By("starting GitHub stub HTTP server")
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"total_count": 0, "workflow_runs": []}`))
			}))

			By("creating the WarmRunnerPolicy resource")
			resource := &warmrunnersv1alpha1.WarmRunnerPolicy{
				ObjectMeta: metav1.ObjectMeta{
					Name:      resourceName,
					Namespace: "default",
				},
				Spec: warmrunnersv1alpha1.WarmRunnerPolicySpec{
					GitHub: warmrunnersv1alpha1.GitHubConfig{
						Owner:      "org",
						Repository: "repo",
						Labels:     []string{"self-hosted"},
					},
					Target: warmrunnersv1alpha1.Target{
						Arc: &warmrunnersv1alpha1.ArcTarget{
							RunnerSet: warmrunnersv1alpha1.RefNS{Name: "prod-runners", Namespace: "arc-system"},
						},
					},
					Floor: warmrunnersv1alpha1.FloorRange{Min: 2, Max: 10},
					QueueRule: warmrunnersv1alpha1.QueueRule{
						PollInterval: metav1.Duration{Duration: time.Second},
						Cooldown:     metav1.Duration{Duration: time.Minute},
					},
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())
		})

		AfterEach(func() {
			By("deleting the WarmRunnerPolicy resource")
			resource := &warmrunnersv1alpha1.WarmRunnerPolicy{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			By("closing the GitHub stub HTTP server")
			srv.Close()
		})

		It("sets status.DesiredFloor to floor.Min when queue is empty", func() {
			By("reconciling with a real GitHubRESTPoller pointed at the stub")
			reconciler := &WarmRunnerPolicyReconciler{
				Client:    k8sClient,
				Scheme:    k8sClient.Scheme(),
				Scheduler: scheduler.NewHeuristic(),
				Demand:    demand.NewGitHubRESTPoller(srv.URL, ""),
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("re-fetching the policy and asserting DesiredFloor == 2")
			var got warmrunnersv1alpha1.WarmRunnerPolicy
			Expect(k8sClient.Get(ctx, typeNamespacedName, &got)).To(Succeed())
			Expect(got.Status.DesiredFloor).To(Equal(int32(2)))
			Expect(meta.IsStatusConditionTrue(got.Status.Conditions, "DemandSourceAvailable")).To(BeTrue())
		})
	})
})
