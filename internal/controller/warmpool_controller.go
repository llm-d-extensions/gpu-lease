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
	"context"
	"errors"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	poc "github.com/llm-d-extensions/gpu-lease/api/v1alpha1"
	"github.com/llm-d-extensions/gpu-lease/internal/controller/inventory"
)

// DefaultWarmPoolResyncPeriod is used when WarmPoolReconciler.ResyncPeriod is zero
// (design.md §7: "--warm-pool-resync=15s"). Promotion can be blocked on external state
// (Kueue quota) that produces no watch event on the objects this reconciler watches, so a
// periodic resync is required for the pool to converge on its own.
const DefaultWarmPoolResyncPeriod = 15 * time.Second

// blockedPromotionRequeueAfter is the requeue delay when a promotion is blocked because
// no warm pod sits on a node with a free GPU (design.md §4.6).
const blockedPromotionRequeueAfter = 30 * time.Second

// Event reasons emitted on the managed Deployment. These are the PoC's demo narration
// (task instructions §9): every promote/demote decision and every blocked promotion is
// reported here.
const (
	EventReasonPromoted             = "PromotedToHot"
	EventReasonDemoted              = "DemotedToWarm"
	EventReasonNoFreeGPUOnAnyNode   = poc.ReasonNoFreeGPUOnAnyPodNode
	EventReasonWarmDeficit          = "WarmDeficit"
	EventReasonOrphanLeaseReclaimed = "OrphanLeaseReclaimed"
	// EventReasonFailedLeaseReclaimed is emitted when a Failed GPULease past
	// FailedLeaseRetention is deleted to free its (node, gpuID) pair (see
	// api/v1alpha1.DefaultFailedLeaseRetention's doc comment for why this retention sweep
	// exists at all).
	EventReasonFailedLeaseReclaimed = "FailedLeaseReclaimed"
)

// WarmPoolReconciler watches Deployments annotated gpu-lease.llm-d.ai/managed=true and
// enforces the "always N warm pods" invariant (design.md §2, §4.3-§4.6) by creating and
// deleting GPULease objects. It decides *which* pod to promote or demote; the GPULease
// reconciler (a separate controller in this binary) executes the lease lifecycle that
// results from that decision. WarmPoolReconciler never calls the pod REST API and never
// creates a Kueue Workload directly.
type WarmPoolReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Recorder emits the Events described above. Defaulted by SetupWithManager to
	// mgr.GetEventRecorderFor("warmpool-controller") when nil; tests may inject a
	// record.NewFakeRecorder to assert on emitted events.
	Recorder record.EventRecorder

	// Inventory answers per-node GPU capacity/free-count/allocation questions
	// (internal/controller/inventory). Defaulted by SetupWithManager to
	// inventory.New(mgr.GetClient()) when nil.
	Inventory inventory.Inventory

	// ResyncPeriod is how often a managed Deployment is re-reconciled even without a
	// watch event (design.md §7). Defaults to DefaultWarmPoolResyncPeriod when <= 0.
	ResyncPeriod time.Duration

	// FailedLeaseRetention is how long a GPULease in phase Failed is left in place
	// (as evidence of the failure) before this reconciler deletes it to free its
	// (node, gpuID) pair (see api/v1alpha1.DefaultFailedLeaseRetention's doc comment).
	// Defaults to poc.DefaultFailedLeaseRetention when <= 0; a field (rather than the
	// bare constant) so tests can inject a short retention instead of sleeping.
	FailedLeaseRetention time.Duration
}

// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=poc.llm-d.ai,resources=gpuleases,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile implements the WarmPool reconcile algorithm of design.md §4.6.
func (r *WarmPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var dep appsv1.Deployment
	if err := r.Get(ctx, req.NamespacedName, &dep); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get deployment: %w", err)
	}

	if !poc.IsManaged(dep.Annotations) {
		// Not (or no longer) opted in; the For() predicate normally filters this out,
		// but a Deployment can transition out of management between enqueue and
		// reconcile, and there is nothing for this reconciler to clean up.
		return ctrl.Result{}, nil
	}

	settings := poc.ParseDeploymentSettings(dep.Annotations, dep.Name)

	sel, err := deploymentSelector(&dep)
	if err != nil {
		logger.Error(err, "cannot resolve deployment pod selector; will not retry until the spec changes")
		return ctrl.Result{}, nil
	}

	// Reclaim GPUs held by leases whose claim is no longer valid, and Failed leases past
	// their retention window, *before* computing the promotion/demotion decision, so
	// capacity freed by e.g. a KEDA-deleted hot pod or a reclaimed Failed lease is visible
	// to this same reconcile (this is what makes the system self-heal). failedRequeueAfter
	// is >0 when a Failed lease exists that is not yet old enough to reclaim; it must be
	// folded into this Reconcile's returned RequeueAfter below so reclamation happens
	// promptly rather than waiting on an unrelated watch event or the resync period.
	failedRequeueAfter, err := r.reconcileOrphans(ctx, &dep, sel)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile orphaned/failed leases: %w", err)
	}

	pods, err := listDeploymentPods(ctx, r.Client, &dep)
	if err != nil {
		return ctrl.Result{}, err
	}

	var considered []corev1.Pod
	for i := range pods {
		p := &pods[i]
		if p.Labels == nil || p.Labels[poc.StateLabelKey] == "" {
			if err := initializePodWarmState(ctx, r.Client, p); err != nil {
				return ctrl.Result{}, err
			}
		}
		if isPodConsidered(p) {
			considered = append(considered, *p)
		}
	}

	readyCount := len(considered)
	var hotPods, warmPods []corev1.Pod
	held, hotCount := 0, 0
	for _, p := range considered {
		state := podState(&p)
		if isHeld(state) {
			held++
		}
		switch state {
		case poc.StateHot:
			hotCount++
			hotPods = append(hotPods, p)
		case poc.StateWarm:
			warmPods = append(warmPods, p)
		}
	}

	target := targetHot(readyCount, settings.WarmReplicas, settings.MinHotReplicas)

	PoolHotPods.WithLabelValues(dep.Namespace, dep.Name).Set(float64(hotCount))
	PoolWarmPods.WithLabelValues(dep.Namespace, dep.Name).Set(float64(readyCount - hotCount))
	PoolWarmDesired.WithLabelValues(dep.Namespace, dep.Name).Set(float64(settings.WarmReplicas))

	if int32(readyCount) < settings.MinHotReplicas+settings.WarmReplicas {
		// design.md §4.6/task instructions: hot capacity wins when there are not enough
		// ready replicas to satisfy both floors; surface it rather than staying silent.
		r.event(&dep, corev1.EventTypeWarning, EventReasonWarmDeficit,
			"only %d ready pod(s); min-hot-replicas(%d)+warm-replicas(%d)=%d exceeds that, "+
				"so hot capacity wins and the warm pool runs below its target of %d",
			readyCount, settings.MinHotReplicas, settings.WarmReplicas,
			settings.MinHotReplicas+settings.WarmReplicas, settings.WarmReplicas)
	}

	// Never promote and demote in the same reconcile.
	blocked := false
	switch {
	case held < target:
		need := target - held
		freeByNode, err := r.Inventory.NodeFreeCounts(ctx)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("node free GPU counts: %w", err)
		}
		promoted, err := r.promote(ctx, &dep, settings, warmPods, need, freeByNode)
		if err != nil {
			return ctrl.Result{}, err
		}
		blocked = promoted < need
	case held > target:
		need := held - target
		if err := r.demote(ctx, &dep, hotPods, need); err != nil {
			return ctrl.Result{}, err
		}
	}

	requeueAfter := r.effectiveResyncPeriod()
	if blocked {
		requeueAfter = blockedPromotionRequeueAfter
	}
	requeueAfter = minPositiveDuration(requeueAfter, failedRequeueAfter)

	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// minPositiveDuration returns the smaller of a and b, treating a non-positive value as
// "no requirement" rather than as smaller than everything: if either is <= 0 the other is
// returned unchanged, so a caller can fold an optional requeue-after (e.g.
// failedRequeueAfter, which is 0 when no Failed lease is pending reclaim) into a value
// that already has its own default without special-casing the zero case at each call.
func minPositiveDuration(a, b time.Duration) time.Duration {
	if a <= 0 {
		return b
	}
	if b <= 0 {
		return a
	}
	if a < b {
		return a
	}
	return b
}

// promote attempts to promote up to need warm pods to hot, in candidate order
// (design.md §4.6), and returns how many promotions actually succeeded. A shortfall
// (the return value < need) means no warm pod sits on a node with a free GPU; the caller
// is responsible for the NoFreeGPUOnAnyPodNode event/metric/requeue.
func (r *WarmPoolReconciler) promote(
	ctx context.Context,
	dep *appsv1.Deployment,
	settings poc.DeploymentSettings,
	warmPods []corev1.Pod,
	need int,
	freeByNode map[string]int32,
) (int, error) {
	candidates := sortPromotionCandidates(warmPods, freeByNode)

	promoted := 0
	for i := range candidates {
		if promoted >= need {
			break
		}
		pod := &candidates[i]
		node := pod.Spec.NodeName

		lease, err := r.acquireLeaseOnNode(ctx, dep, settings, pod, node)
		if err != nil {
			if errors.Is(err, inventory.ErrExhausted) {
				// Raced with another allocator, or freeByNode was already stale; this
				// node has no free GPU after all. Try the next candidate.
				continue
			}
			return promoted, err
		}

		if err := patchPodStateLabel(ctx, r.Client, pod, poc.StateActivating); err != nil {
			return promoted, err
		}

		PromotionsTotal.Inc()
		promoted++
		r.event(dep, corev1.EventTypeNormal, EventReasonPromoted,
			"promoting pod %s to hot: acquired GPULease %s (node %s, gpu %d, model %s)",
			pod.Name, lease.Name, node, lease.Spec.GPUID, lease.Spec.GPUModel)
	}

	if promoted < need {
		LeaseAcquireFailuresTotal.WithLabelValues(dep.Name, poc.ReasonNoFreeGPUOnAnyPodNode).Inc()
		r.event(dep, corev1.EventTypeWarning, EventReasonNoFreeGPUOnAnyNode,
			"need to promote %d more warm pod(s) to hot (target %d) but no warm pod sits on a "+
				"node with a free GPU (%d candidate warm pod(s) had a free-GPU node, %d promoted)",
			need-promoted, need, len(candidates), promoted)
	}

	return promoted, nil
}

// acquireLeaseOnNode allocates the lowest free GPU id on node and creates the
// corresponding GPULease claiming pod, retrying the next free id on AlreadyExists
// (design.md §4.2, §4.4: the lease name (node, gpuID) is the mutual-exclusion mechanism).
// It returns inventory.ErrExhausted if the node has no free id, or every free id raced
// away from under it.
func (r *WarmPoolReconciler) acquireLeaseOnNode(
	ctx context.Context,
	dep *appsv1.Deployment,
	settings poc.DeploymentSettings,
	pod *corev1.Pod,
	node string,
) (*poc.GPULease, error) {
	free, err := r.Inventory.FreeGPUIDs(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("free GPU ids on node %s: %w", node, err)
	}
	if len(free) == 0 {
		return nil, inventory.ErrExhausted
	}

	_, model, err := r.Inventory.NodeCapacity(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("node capacity for %s: %w", node, err)
	}

	for _, id := range free {
		lease := &poc.GPULease{
			ObjectMeta: metav1.ObjectMeta{
				Name:       poc.LeaseName(node, id),
				Finalizers: []string{poc.Finalizer},
			},
			Spec: poc.GPULeaseSpec{
				NodeName: node,
				GPUID:    id,
				GPUModel: model,
				ClaimRef: poc.ObjectRef{
					Namespace: pod.Namespace,
					Name:      pod.Name,
					UID:       pod.UID,
				},
				DeploymentRef: poc.ObjectRef{
					Namespace: dep.Namespace,
					Name:      dep.Name,
					UID:       dep.UID,
				},
				LocalQueueName: settings.LocalQueueName,
			},
		}
		if err := r.Create(ctx, lease); err == nil {
			return lease, nil
		} else if !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("create GPULease %s: %w", lease.Name, err)
		}
		// AlreadyExists: (node, id) was just claimed by a concurrent allocator; try the
		// next free id on this node.
	}
	return nil, inventory.ErrExhausted
}

// demote demotes up to need hot pods to warm, newest-first (LIFO, design.md §4.6). It
// never touches the pod REST API: it labels the pod releasing and deletes its GPULease;
// the GPULease reconciler's finalizer does the actual release and reverts the pod to
// warm.
func (r *WarmPoolReconciler) demote(ctx context.Context, dep *appsv1.Deployment, hotPods []corev1.Pod, need int) error {
	candidates := sortDemotionCandidates(hotPods)

	leases, err := r.leasesForDeployment(ctx, dep)
	if err != nil {
		return err
	}

	demoted := 0
	for i := range candidates {
		if demoted >= need {
			break
		}
		pod := &candidates[i]

		lease := findLeaseForPod(leases, pod)
		if err := patchPodStateLabel(ctx, r.Client, pod, poc.StateReleasing); err != nil {
			return err
		}

		var node string
		var gpuID int32
		leaseName := "<none>"
		if lease != nil {
			node, gpuID, leaseName = lease.Spec.NodeName, lease.Spec.GPUID, lease.Name
			if err := r.Delete(ctx, lease); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete GPULease %s: %w", lease.Name, err)
			}
		}

		DemotionsTotal.Inc()
		demoted++
		r.event(dep, corev1.EventTypeNormal, EventReasonDemoted,
			"demoting pod %s from hot to warm: deleting GPULease %s (node %s, gpu %d) to release the GPU",
			pod.Name, leaseName, node, gpuID)
	}
	return nil
}

// leasesForDeployment lists every GPULease whose spec.deploymentRef points at dep.
// GPULease is cluster-scoped, so this is a full list + client-side filter; the same
// pattern the read-only inventory package uses, and cheap at this PoC's scale.
func (r *WarmPoolReconciler) leasesForDeployment(ctx context.Context, dep *appsv1.Deployment) ([]poc.GPULease, error) {
	var all poc.GPULeaseList
	if err := r.List(ctx, &all); err != nil {
		return nil, fmt.Errorf("list GPULeases: %w", err)
	}
	out := make([]poc.GPULease, 0, len(all.Items))
	for _, l := range all.Items {
		if l.Spec.DeploymentRef.Namespace == dep.Namespace && l.Spec.DeploymentRef.Name == dep.Name {
			out = append(out, l)
		}
	}
	return out, nil
}

// findLeaseForPod returns the lease in leases claiming pod, or nil if none does.
func findLeaseForPod(leases []poc.GPULease, pod *corev1.Pod) *poc.GPULease {
	for i := range leases {
		claim := leases[i].Spec.ClaimRef
		if claim.Namespace == pod.Namespace && claim.Name == pod.Name {
			if claim.UID == "" || claim.UID == pod.UID {
				return &leases[i]
			}
		}
	}
	return nil
}

// reconcileOrphans deletes every GPULease belonging to dep whose claim is no longer
// valid (design.md §4.6 garbage collection): the claimed pod is gone, not Ready, no
// longer selected by dep, or has moved node. This is what reclaims a GPU after e.g. KEDA
// deletes a hot pod out from under its lease.
//
// It also runs the Failed-lease retention sweep (api/v1alpha1.DefaultFailedLeaseRetention's
// doc comment): a Failed lease's claim pod is normally still alive/Ready/on the right node
// (the GPULease reconciler reverted it to warm on failure, design.md §4.7's
// "delete Workload, pod->warm"), so it is never an orphan by the identity rules above, and
// without this second check it would pin its (node, gpuID) pair forever. The returned
// requeueAfter is the minimum time until some Failed lease belonging to dep next becomes
// reclaimable (0 if none is pending), so the caller can schedule a requeue that actually
// lands at/after that moment instead of relying on an unrelated watch event.
func (r *WarmPoolReconciler) reconcileOrphans(ctx context.Context, dep *appsv1.Deployment, sel labels.Selector) (time.Duration, error) {
	leases, err := r.leasesForDeployment(ctx, dep)
	if err != nil {
		return 0, err
	}

	var requeueAfter time.Duration
	for i := range leases {
		lease := &leases[i]
		if lease.DeletionTimestamp != nil {
			continue // already being torn down (e.g. by a demotion this same reconcile).
		}

		if lease.Status.Phase == poc.GPULeasePhaseFailed {
			deleted, retryAfter, err := r.reclaimFailedLease(ctx, dep, lease)
			if err != nil {
				return 0, err
			}
			if deleted {
				continue
			}
			if retryAfter > 0 {
				requeueAfter = minPositiveDuration(requeueAfter, retryAfter)
			}
			// Not yet reclaimable by retention; still fall through to the identity-based
			// orphan check below -- if the claim pod is genuinely gone (rather than alive
			// and reverted to warm, the normal post-failure state), there is no reason to
			// wait out the retention window at all.
		}

		var pod corev1.Pod
		var podPtr *corev1.Pod
		claimKey := client.ObjectKey{Namespace: lease.Spec.ClaimRef.Namespace, Name: lease.Spec.ClaimRef.Name}
		switch err := r.Get(ctx, claimKey, &pod); {
		case err == nil:
			podPtr = &pod
		case apierrors.IsNotFound(err):
			podPtr = nil
		default:
			return 0, fmt.Errorf("get claim pod for lease %s: %w", lease.Name, err)
		}

		reason := leaseOrphanReason(podPtr, lease, sel)
		if reason == "" {
			continue
		}

		if err := r.Delete(ctx, lease); err != nil && !apierrors.IsNotFound(err) {
			return 0, fmt.Errorf("delete orphaned GPULease %s: %w", lease.Name, err)
		}
		r.event(dep, corev1.EventTypeWarning, EventReasonOrphanLeaseReclaimed,
			"reclaiming GPULease %s (node %s, gpu %d) claimed by pod %s/%s: %s",
			lease.Name, lease.Spec.NodeName, lease.Spec.GPUID,
			lease.Spec.ClaimRef.Namespace, lease.Spec.ClaimRef.Name, reason)
	}
	return requeueAfter, nil
}

// failedLeaseFailureReason returns the failure reason recorded on lease's Ready condition
// (design.md §4.2/§4.7: transitionToFailed always sets it to one of api/v1alpha1's
// Reason* constants), or "" if the condition is somehow missing.
func failedLeaseFailureReason(lease *poc.GPULease) string {
	if c := meta.FindStatusCondition(lease.Status.Conditions, poc.ConditionReady); c != nil {
		return c.Reason
	}
	return ""
}

// failedSince returns when lease entered phase Failed: the Ready condition's own
// LastTransitionTime (set the moment transitionToFailed flips it to False -- the same
// mechanism admissionAnchor/podActivatedAnchor rely on elsewhere in this package), falling
// back to lease.CreationTimestamp if that condition is somehow missing or zero.
func failedSince(lease *poc.GPULease) time.Time {
	if c := meta.FindStatusCondition(lease.Status.Conditions, poc.ConditionReady); c != nil && !c.LastTransitionTime.IsZero() {
		return c.LastTransitionTime.Time
	}
	return lease.CreationTimestamp.Time
}

// reclaimFailedLease deletes lease if it has been Failed for at least
// r.effectiveFailedLeaseRetention(), emitting EventReasonFailedLeaseReclaimed on dep.
// Returns deleted=true if it was (or had already been) removed; otherwise returns the
// duration until it next becomes reclaimable, for the caller to fold into its requeue.
// Tolerates the lease having already been deleted concurrently (by an operator, or by the
// GPULease reconciler's own finalizer path racing this sweep).
//
// It strips poc.Finalizer itself instead of leaving that to the GPULease reconciler's own
// deletion handler (reconcileDeletion): by the time a lease reaches Failed,
// transitionToFailed has already done that handler's work (deleted the backing Workload,
// best-effort DELETE /lease and reverted the claim pod to warm), so reconcileDeletion would
// find nothing left to do. Without stripping the finalizer here, Delete would only set a
// DeletionTimestamp and the lease -- still present in etcd -- would keep pinning its
// (node, gpuID) pair (inventory.usedGPUIDs counts every GPULease regardless of phase or
// DeletionTimestamp) until some other reconcile of the *GPULease* happened to run and
// remove it, defeating the point of this sweep. If the GPULease reconciler is concurrently
// running its own reconcileDeletion for this same lease (e.g. an operator deleted it by
// hand at the same moment), the Update below simply fails on a resourceVersion conflict and
// this reconcile errors out and retries -- never a lost update, never a double-free.
func (r *WarmPoolReconciler) reclaimFailedLease(ctx context.Context, dep *appsv1.Deployment, lease *poc.GPULease) (deleted bool, retryAfter time.Duration, err error) {
	retention := r.effectiveFailedLeaseRetention()
	age := time.Since(failedSince(lease))
	if age < retention {
		return false, retention - age, nil
	}

	reason := failedLeaseFailureReason(lease)
	message := lease.Status.Message
	node, gpuID := lease.Spec.NodeName, lease.Spec.GPUID
	name := lease.Name

	if controllerutil.ContainsFinalizer(lease, poc.Finalizer) {
		controllerutil.RemoveFinalizer(lease, poc.Finalizer)
		if err := r.Update(ctx, lease); err != nil {
			if apierrors.IsNotFound(err) {
				return true, 0, nil
			}
			return false, 0, fmt.Errorf("remove finalizer from failed GPULease %s: %w", name, err)
		}
	}

	if err := r.Delete(ctx, lease); err != nil {
		if apierrors.IsNotFound(err) {
			return true, 0, nil
		}
		return false, 0, fmt.Errorf("delete failed GPULease %s: %w", name, err)
	}

	r.event(dep, corev1.EventTypeNormal, EventReasonFailedLeaseReclaimed,
		"reclaiming Failed GPULease %s (node %s, gpu %d) after %s: original failure reason %s: %s",
		name, node, gpuID, retention, reason, message)
	return true, 0, nil
}

// effectiveFailedLeaseRetention returns r.FailedLeaseRetention, or
// poc.DefaultFailedLeaseRetention if unset (mirrors effectiveResyncPeriod).
func (r *WarmPoolReconciler) effectiveFailedLeaseRetention() time.Duration {
	if r.FailedLeaseRetention > 0 {
		return r.FailedLeaseRetention
	}
	return poc.DefaultFailedLeaseRetention
}

// event records an Event on dep if r.Recorder is set; a no-op otherwise (e.g. in unit
// tests that do not care about narration).
func (r *WarmPoolReconciler) event(dep *appsv1.Deployment, eventType, reason, messageFmt string, args ...interface{}) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(dep, eventType, reason, messageFmt, args...)
}

// effectiveResyncPeriod returns r.ResyncPeriod, or DefaultWarmPoolResyncPeriod if unset.
func (r *WarmPoolReconciler) effectiveResyncPeriod() time.Duration {
	if r.ResyncPeriod > 0 {
		return r.ResyncPeriod
	}
	return DefaultWarmPoolResyncPeriod
}

// mapPodToDeployment maps a Pod watch event to the managed Deployment(s) in its
// namespace whose selector matches it. Pods are owned by a ReplicaSet, not directly by
// the Deployment, so this walks the selector rather than an owner-reference chain.
func (r *WarmPoolReconciler) mapPodToDeployment(ctx context.Context, obj client.Object) []reconcile.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil
	}

	var deps appsv1.DeploymentList
	if err := r.List(ctx, &deps, client.InNamespace(pod.Namespace)); err != nil {
		log.FromContext(ctx).Error(err, "list deployments for pod watch mapping", "pod", pod.Name)
		return nil
	}

	var reqs []reconcile.Request
	for i := range deps.Items {
		dep := &deps.Items[i]
		if !poc.IsManaged(dep.Annotations) || dep.Spec.Selector == nil {
			continue
		}
		sel, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
		if err != nil || sel.Empty() {
			continue
		}
		if sel.Matches(labels.Set(pod.Labels)) {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(dep)})
		}
	}
	return reqs
}

// mapLeaseToDeployment maps a GPULease watch event directly to its owning Deployment,
// via spec.deploymentRef.
func mapLeaseToDeployment(_ context.Context, obj client.Object) []reconcile.Request {
	lease, ok := obj.(*poc.GPULease)
	if !ok || lease.Spec.DeploymentRef.Name == "" {
		return nil
	}
	ref := lease.Spec.DeploymentRef
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}}}
}

// SetupWithManager sets up the controller with the Manager.
func (r *WarmPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Inventory == nil {
		r.Inventory = inventory.New(mgr.GetClient())
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("warmpool-controller")
	}
	if r.FailedLeaseRetention <= 0 {
		r.FailedLeaseRetention = poc.DefaultFailedLeaseRetention
	}

	managedPredicate := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return poc.IsManaged(obj.GetAnnotations())
	})

	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1.Deployment{}, builder.WithPredicates(managedPredicate)).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.mapPodToDeployment)).
		Watches(&poc.GPULease{}, handler.EnqueueRequestsFromMapFunc(mapLeaseToDeployment)).
		Named("warmpool").
		Complete(r)
}
