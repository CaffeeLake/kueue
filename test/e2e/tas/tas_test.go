/*
Copyright 2024 The Kubernetes Authors.

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

package tase2e

import (
	"fmt"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kueuealpha "sigs.k8s.io/kueue/apis/kueue/v1alpha1"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta1"
	workloadjob "sigs.k8s.io/kueue/pkg/controller/jobs/job"
	"sigs.k8s.io/kueue/pkg/util/testing"
	testingjob "sigs.k8s.io/kueue/pkg/util/testingjobs/job"
	"sigs.k8s.io/kueue/pkg/workload"
	"sigs.k8s.io/kueue/test/util"
)

const (
	instanceType          = "tas-group"
	tasNodeGroupLabel     = "cloud.provider.com/node-group"
	topologyLevelRack     = "cloud.provider.com/topology-rack"
	topologyLevelBlock    = "cloud.provider.com/topology-block"
	topologyLevelHostname = "kubernetes.io/hostname"
	extraResource         = "example.com/gpu"
)

var _ = ginkgo.Describe("TopologyAwareScheduling", func() {
	var ns *corev1.Namespace
	ginkgo.BeforeEach(func() {
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "e2e-tas-",
			},
		}
		gomega.Expect(k8sClient.Create(ctx, ns)).To(gomega.Succeed())
	})
	ginkgo.AfterEach(func() {
		gomega.Expect(util.DeleteNamespace(ctx, k8sClient, ns)).To(gomega.Succeed())
	})

	ginkgo.When("Creating a Job that can't fit in one Rack", func() {
		var (
			topology     *kueuealpha.Topology
			tasFlavor    *kueue.ResourceFlavor
			localQueue   *kueue.LocalQueue
			clusterQueue *kueue.ClusterQueue
		)
		ginkgo.BeforeEach(func() {
			topology = testing.MakeTopology("datacenter").Levels([]string{
				topologyLevelBlock,
				topologyLevelRack,
				topologyLevelHostname,
			}).Obj()
			gomega.Expect(k8sClient.Create(ctx, topology)).Should(gomega.Succeed())

			tasFlavor = testing.MakeResourceFlavor("tas-flavor").
				NodeLabel(tasNodeGroupLabel, instanceType).TopologyName(topology.Name).Obj()
			gomega.Expect(k8sClient.Create(ctx, tasFlavor)).Should(gomega.Succeed())
			clusterQueue = testing.MakeClusterQueue("cluster-queue").
				ResourceGroup(
					*testing.MakeFlavorQuotas("tas-flavor").
						Resource(extraResource, "8").
						Obj(),
				).
				Obj()
			gomega.Expect(k8sClient.Create(ctx, clusterQueue)).Should(gomega.Succeed())
			util.ExpectClusterQueuesToBeActive(ctx, k8sClient, clusterQueue)

			localQueue = testing.MakeLocalQueue("main", ns.Name).ClusterQueue("cluster-queue").Obj()
			gomega.Expect(k8sClient.Create(ctx, localQueue)).Should(gomega.Succeed())
		})
		ginkgo.AfterEach(func() {
			gomega.Expect(util.DeleteAllJobsInNamespace(ctx, k8sClient, ns)).Should(gomega.Succeed())
			// Force remove workloads to be sure that cluster queue can be removed.
			gomega.Expect(util.DeleteWorkloadsInNamespace(ctx, k8sClient, ns)).Should(gomega.Succeed())
			gomega.Expect(util.DeleteObject(ctx, k8sClient, localQueue)).Should(gomega.Succeed())
			gomega.Expect(util.DeleteObject(ctx, k8sClient, topology)).Should(gomega.Succeed())
			util.ExpectObjectToBeDeleted(ctx, k8sClient, clusterQueue, true)
			util.ExpectObjectToBeDeleted(ctx, k8sClient, tasFlavor, true)
		})

		ginkgo.It("Should not admit a Job if Rack required", func() {
			sampleJob := testingjob.MakeJob("test-job", ns.Name).
				Queue(localQueue.Name).
				Parallelism(3).
				Completions(3).
				Request(extraResource, "1").
				Limit(extraResource, "1").
				Obj()
			jobKey := client.ObjectKeyFromObject(sampleJob)
			sampleJob = (&testingjob.JobWrapper{Job: *sampleJob}).
				PodAnnotation(kueuealpha.PodSetRequiredTopologyAnnotation, topologyLevelRack).
				Image(util.E2eTestSleepImage, []string{"100ms"}).
				Obj()
			gomega.Expect(k8sClient.Create(ctx, sampleJob)).Should(gomega.Succeed())

			expectJobWithSuspendedAndNodeSelectors(jobKey, true, nil)
			wlLookupKey := types.NamespacedName{Name: workloadjob.GetWorkloadNameForJob(sampleJob.Name, sampleJob.UID), Namespace: ns.Name}
			ginkgo.By(fmt.Sprintf("workload %q not getting an admission", wlLookupKey), func() {
				createdWorkload := &kueue.Workload{}
				gomega.Eventually(func(g gomega.Gomega) {
					g.Expect(k8sClient.Get(ctx, wlLookupKey, createdWorkload)).Should(gomega.Succeed())
					g.Expect(createdWorkload.Status.Admission).Should(gomega.BeNil())
				}, util.LongTimeout, util.Interval).Should(gomega.Succeed())
			})
		})

		ginkgo.It("should admit a Job to TAS Block if Rack preferred", func() {
			sampleJob := testingjob.MakeJob("test-job", ns.Name).
				Queue(localQueue.Name).
				Parallelism(3).
				Completions(3).
				Request(extraResource, "1").
				Limit(extraResource, "1").
				Obj()
			jobKey := client.ObjectKeyFromObject(sampleJob)
			sampleJob = (&testingjob.JobWrapper{Job: *sampleJob}).
				PodAnnotation(kueuealpha.PodSetPreferredTopologyAnnotation, topologyLevelRack).
				Image(util.E2eTestSleepImage, []string{"100ms"}).
				Obj()
			gomega.Expect(k8sClient.Create(ctx, sampleJob)).Should(gomega.Succeed())

			expectJobWithSuspendedAndNodeSelectors(jobKey, false, map[string]string{
				tasNodeGroupLabel: instanceType,
			})
			wlLookupKey := types.NamespacedName{Name: workloadjob.GetWorkloadNameForJob(sampleJob.Name, sampleJob.UID), Namespace: ns.Name}
			createdWorkload := &kueue.Workload{}
			ginkgo.By(fmt.Sprintf("await for admission of workload %q and verify TopologyAssignment", wlLookupKey), func() {
				gomega.Eventually(func(g gomega.Gomega) {
					g.Expect(k8sClient.Get(ctx, wlLookupKey, createdWorkload)).Should(gomega.Succeed())
					g.Expect(createdWorkload.Status.Admission).ShouldNot(gomega.BeNil())
				}, util.LongTimeout, util.Interval).Should(gomega.Succeed())
				gomega.Expect(createdWorkload.Status.Admission.PodSetAssignments).Should(gomega.HaveLen(1))
				gomega.Expect(createdWorkload.Status.Admission.PodSetAssignments[0].TopologyAssignment.Levels).Should(gomega.BeComparableTo(
					[]string{
						topologyLevelBlock,
						topologyLevelRack,
						topologyLevelHostname,
					},
				))
				podCountPerBlock := map[string]int32{}
				for _, d := range createdWorkload.Status.Admission.PodSetAssignments[0].TopologyAssignment.Domains {
					podCountPerBlock[d.Values[0]] += d.Count
				}
				// both pod assignments are in the same block
				gomega.Expect(podCountPerBlock).Should(gomega.HaveLen(1))
				// pod assignment count equals job parallelism
				for _, pd := range podCountPerBlock {
					gomega.Expect(pd).Should(gomega.Equal(ptr.Deref[int32](sampleJob.Spec.Parallelism, 0)))
				}
			})
			ginkgo.By(fmt.Sprintf("verify the workload %q gets finished", wlLookupKey), func() {
				gomega.Eventually(func(g gomega.Gomega) {
					g.Expect(k8sClient.Get(ctx, wlLookupKey, createdWorkload)).Should(gomega.Succeed())
					g.Expect(workload.HasQuotaReservation(createdWorkload)).Should(gomega.BeTrue())
					g.Expect(createdWorkload.Status.Conditions).Should(testing.HaveConditionStatusTrue(kueue.WorkloadFinished))
				}, util.LongTimeout, util.Interval).Should(gomega.Succeed())
			})
		})

		ginkgo.It("Should admit a Job to TAS Block if Block required", func() {
			sampleJob := testingjob.MakeJob("test-job", ns.Name).
				Queue(localQueue.Name).
				Parallelism(3).
				Completions(3).
				Request(extraResource, "1").
				Limit(extraResource, "1").
				Obj()
			jobKey := client.ObjectKeyFromObject(sampleJob)
			sampleJob = (&testingjob.JobWrapper{Job: *sampleJob}).
				PodAnnotation(kueuealpha.PodSetRequiredTopologyAnnotation, topologyLevelBlock).
				Image(util.E2eTestSleepImage, []string{"100ms"}).
				Obj()
			gomega.Expect(k8sClient.Create(ctx, sampleJob)).Should(gomega.Succeed())

			expectJobWithSuspendedAndNodeSelectors(jobKey, false, map[string]string{
				tasNodeGroupLabel: instanceType,
			})
			wlLookupKey := types.NamespacedName{Name: workloadjob.GetWorkloadNameForJob(sampleJob.Name, sampleJob.UID), Namespace: ns.Name}
			createdWorkload := &kueue.Workload{}
			ginkgo.By(fmt.Sprintf("await for admission of workload %q and verify TopologyAssignment", wlLookupKey), func() {
				gomega.Eventually(func(g gomega.Gomega) {
					g.Expect(k8sClient.Get(ctx, wlLookupKey, createdWorkload)).Should(gomega.Succeed())
					g.Expect(createdWorkload.Status.Admission).ShouldNot(gomega.BeNil())
				}, util.LongTimeout, util.Interval).Should(gomega.Succeed())
				gomega.Expect(createdWorkload.Status.Admission.PodSetAssignments).Should(gomega.HaveLen(1))
				gomega.Expect(createdWorkload.Status.Admission.PodSetAssignments[0].TopologyAssignment.Levels).Should(gomega.BeComparableTo(
					[]string{
						topologyLevelBlock,
						topologyLevelRack,
						topologyLevelHostname,
					},
				))
				podCountPerBlock := map[string]int32{}
				for _, d := range createdWorkload.Status.Admission.PodSetAssignments[0].TopologyAssignment.Domains {
					podCountPerBlock[d.Values[0]] += d.Count
				}
				// both pod assignments are in the same block
				gomega.Expect(podCountPerBlock).Should(gomega.HaveLen(1))
				// pod assignment count equals job parallelism
				for _, pd := range podCountPerBlock {
					gomega.Expect(pd).Should(gomega.Equal(ptr.Deref[int32](sampleJob.Spec.Parallelism, 0)))
				}
			})

			ginkgo.By(fmt.Sprintf("verify the workload %q gets finished", wlLookupKey), func() {
				gomega.Eventually(func(g gomega.Gomega) {
					g.Expect(k8sClient.Get(ctx, wlLookupKey, createdWorkload)).Should(gomega.Succeed())
					g.Expect(workload.HasQuotaReservation(createdWorkload)).Should(gomega.BeTrue())
					g.Expect(createdWorkload.Status.Conditions).Should(testing.HaveConditionStatusTrue(kueue.WorkloadFinished))
				}, util.LongTimeout, util.Interval).Should(gomega.Succeed())
			})
		})

		ginkgo.It("Should allow to run a Job with parallelism < completions", func() {
			sampleJob := testingjob.MakeJob("test-job", ns.Name).
				Queue(localQueue.Name).
				Parallelism(2).
				Completions(3).
				Request(extraResource, "1").
				Limit(extraResource, "1").
				Obj()
			sampleJob = (&testingjob.JobWrapper{Job: *sampleJob}).
				PodAnnotation(kueuealpha.PodSetRequiredTopologyAnnotation, topologyLevelBlock).
				Image(util.E2eTestSleepImage, []string{"10ms"}).
				Obj()
			gomega.Expect(k8sClient.Create(ctx, sampleJob)).Should(gomega.Succeed())

			wlLookupKey := types.NamespacedName{Name: workloadjob.GetWorkloadNameForJob(sampleJob.Name, sampleJob.UID), Namespace: ns.Name}
			createdWorkload := &kueue.Workload{}

			ginkgo.By(fmt.Sprintf("verify the workload %q gets TopologyAssignment becomes finished", wlLookupKey), func() {
				gomega.Eventually(func(g gomega.Gomega) {
					g.Expect(k8sClient.Get(ctx, wlLookupKey, createdWorkload)).Should(gomega.Succeed())
					g.Expect(createdWorkload.Status.Admission.PodSetAssignments[0].TopologyAssignment).ShouldNot(gomega.BeNil())
					g.Expect(createdWorkload.Status.Conditions).Should(testing.HaveConditionStatusTrue(kueue.WorkloadFinished))
				}, util.LongTimeout, util.Interval).Should(gomega.Succeed())
			})
		})
	})
})

func expectJobWithSuspendedAndNodeSelectors(key types.NamespacedName, suspended bool, ns map[string]string) {
	job := &batchv1.Job{}
	gomega.EventuallyWithOffset(1, func(g gomega.Gomega) {
		g.Expect(k8sClient.Get(ctx, key, job)).To(gomega.Succeed())
		g.Expect(job.Spec.Suspend).Should(gomega.Equal(ptr.To(suspended)))
		g.Expect(job.Spec.Template.Spec.NodeSelector).Should(gomega.Equal(ns))
	}, util.Timeout, util.Interval).Should(gomega.Succeed())
}
