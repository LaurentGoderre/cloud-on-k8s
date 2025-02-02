// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package driver

import (
	"sort"

	"github.com/elastic/cloud-on-k8s/pkg/apis/elasticsearch/v1beta1"
	"github.com/elastic/cloud-on-k8s/pkg/controller/common/expectations"
	"github.com/elastic/cloud-on-k8s/pkg/controller/elasticsearch/label"
	"github.com/elastic/cloud-on-k8s/pkg/controller/elasticsearch/sset"
	"github.com/elastic/cloud-on-k8s/pkg/utils/k8s"
	"github.com/elastic/cloud-on-k8s/pkg/utils/stringsutil"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Delete runs through a list of potential candidates and select the ones that can be deleted.
// Do not run this function unless driver expectations are met.
func (ctx *rollingUpgradeCtx) Delete() ([]corev1.Pod, error) {
	if len(ctx.podsToUpgrade) == 0 {
		return nil, nil
	}

	// Get allowed deletions and check if maxUnavailable has been reached.
	allowedDeletions, maxUnavailableReached := ctx.getAllowedDeletions()

	// Step 1. Sort the Pods to get the ones with the higher priority
	candidates := make([]corev1.Pod, len(ctx.podsToUpgrade)) // work on a copy in order to have no side effect
	copy(candidates, ctx.podsToUpgrade)
	sortCandidates(candidates)

	// Step 2: Apply predicates
	predicateContext := NewPredicateContext(
		ctx.esState,
		ctx.shardLister,
		ctx.healthyPods,
		ctx.podsToUpgrade,
		ctx.expectedMasters,
		ctx.actualMasters,
	)
	log.V(1).Info("Applying predicates",
		"maxUnavailableReached", maxUnavailableReached,
		"allowedDeletions", allowedDeletions,
	)
	podsToDelete, err := applyPredicates(predicateContext, candidates, maxUnavailableReached, allowedDeletions)
	if err != nil {
		return podsToDelete, err
	}

	if len(podsToDelete) == 0 {
		log.V(1).Info(
			"No pod deleted during rolling upgrade",
			"es_name", ctx.ES.Name,
			"namespace", ctx.ES.Namespace,
		)
		return podsToDelete, nil
	}

	// Disable shard allocation
	if err := ctx.prepareClusterForNodeRestart(ctx.esClient, ctx.esState); err != nil {
		return podsToDelete, err
	}
	// TODO: If master is changed into a data node (or the opposite) it must be excluded or we should update m_m_n
	deletedPods := []corev1.Pod{}
	for _, podToDelete := range podsToDelete {
		if err := ctx.handleMasterScaleChange(podToDelete); err != nil {
			return deletedPods, err
		}
		if err := deletePod(ctx.client, ctx.ES, podToDelete, ctx.expectations); err != nil {
			return deletedPods, err
		}
		deletedPods = append(deletedPods, podToDelete)
	}
	return deletedPods, nil
}

// getAllowedDeletions returns the number of deletions that can be done and if maxUnavailable has been reached.
func (ctx *rollingUpgradeCtx) getAllowedDeletions() (int, bool) {
	// Check if we are not over disruption budget
	// Upscale is done, we should have the required number of Pods
	actualPods := ctx.statefulSets.PodNames()
	unhealthyPods := len(actualPods) - len(ctx.healthyPods)

	maxUnavailable := ctx.ES.Spec.UpdateStrategy.ChangeBudget.GetMaxUnavailableOrDefault()
	if maxUnavailable == nil {
		// maxUnavailable is unbounded, we allow removing all pods
		return len(actualPods), false
	}

	allowedDeletions := int(*maxUnavailable) - unhealthyPods
	// If maxUnavailable is reached the deletion driver still allows one unhealthy Pod to be restarted.
	maxUnavailableReached := allowedDeletions <= 0
	return allowedDeletions, maxUnavailableReached
}

// sortCandidates is the default sort function, masters have lower priority as
// we want to update the data nodes first.
// If 2 Pods are of the same type then use the reverse ordinal order.
// TODO: Add some priority to unhealthy (bootlooping) Pods
func sortCandidates(allPods []corev1.Pod) {
	sort.Slice(allPods, func(i, j int) bool {
		pod1 := allPods[i]
		pod2 := allPods[j]
		if (label.IsMasterNode(pod1) && label.IsMasterNode(pod2)) ||
			(!label.IsMasterNode(pod1) && !label.IsMasterNode(pod2)) { // same type, use the reverse name function
			ssetName1, ord1, err := sset.StatefulSetName(pod1.Name)
			if err != nil {
				return false
			}
			ssetName2, ord2, err := sset.StatefulSetName(pod2.Name)
			if err != nil {
				return false
			}
			if ssetName1 == ssetName2 {
				// same name, compare ordinal, higher first
				return ord1 > ord2
			}
			return ssetName1 < ssetName2
		}
		if label.IsMasterNode(pod1) && !label.IsMasterNode(pod2) {
			// pod2 has higher priority since it is a data node
			return false
		}
		return true
	})
}

// handleMasterScaleChange handles Zen updates when a type change results in the addition or the removal of a master:
// In case of a master scale down it shares the same logic that a "traditional" scale down:
// * We proactively set m_m_n to the value of 1 if there are 2 Zen1 masters left
// * We exclude the master for Zen2
// In case of a master scale up there's nothing else to do:
// * If there are Zen1 nodes m_m_n is updated prior the update of the StatefulSet in HandleUpscaleAndSpecChanges
// * Because of the design of Zen2 there's nothing else to do for it.
func (ctx *rollingUpgradeCtx) handleMasterScaleChange(pod corev1.Pod) error {
	masterScaleDown := label.IsMasterNode(pod) && !stringsutil.StringInSlice(pod.Name, ctx.expectedMasters)
	if masterScaleDown {
		if err := updateZenSettingsForDownscale(
			ctx.client,
			ctx.esClient,
			ctx.ES,
			ctx.reconcileState,
			ctx.statefulSets,
			pod.Name,
		); err != nil {
			return err
		}
	}
	return nil
}

func deletePod(k8sClient k8s.Client, es v1beta1.Elasticsearch, pod corev1.Pod, expectations *expectations.Expectations) error {
	log.Info("Deleting pod for rolling upgrade", "es_name", es.Name, "namespace", es.Namespace, "pod_name", pod.Name, "pod_uid", pod.UID)
	// The name of the Pod we want to delete is not enough as it may have been already deleted/recreated.
	// The uid of the Pod we want to delete is used as a precondition to check that we actually delete the right one.
	// If not it means that we are running with a stale cache.
	opt := client.Preconditions{
		UID: &pod.UID,
	}
	// expect the pod to not be there in the cache at next reconciliation
	expectations.ExpectDeletion(pod)
	err := k8sClient.Delete(&pod, opt)
	if err != nil {
		// cancel our expectation since the Pod may not be deleted
		expectations.CancelExpectedDeletion(pod)
	}
	return err
}

func runPredicates(
	ctx PredicateContext,
	candidate corev1.Pod,
	deletedPods []corev1.Pod,
	maxUnavailableReached bool,
) (bool, error) {
	for _, predicate := range predicates {
		canDelete, err := predicate.fn(ctx, candidate, deletedPods, maxUnavailableReached)
		if err != nil {
			return false, err
		}
		if !canDelete {
			log.V(1).Info("Predicate failed", "pod_name", candidate.Name, "predicate_name", predicate.name)
			// Skip this Pod, it can't be deleted for the moment
			return false, nil
		}
	}
	// All predicates passed!
	return true, nil
}
