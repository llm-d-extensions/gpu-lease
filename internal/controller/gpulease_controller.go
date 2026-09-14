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
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	poc "github.com/llm-d-extensions/gpu-lease/api/v1alpha1"
	"github.com/llm-d-extensions/gpu-lease/internal/controller/inventory"
	"github.com/llm-d-extensions/gpu-lease/internal/controller/podclient"
	kueuev1beta2 "github.com/llm-d-extensions/gpu-lease/internal/kueue/v1beta2"
)

// pollInterval is how often the Pending and Activating phases re-check Kueue admission /
// pod state while waiting, capped against the remaining admission-timeout /
// activation-timeout budget by nextPoll. There is no watch event for "Kueue is still
// thinking about it" or "the pod hasn't reported hot yet", so polling is required either
// way; 2s keeps the demo (docs/kueue-spike.md: sub-second to ~1.4s admission latency)
// feeling responsive without hammering the API server.
const pollInterval = 2 * time.Second

// boundPollInterval is how often a Bound lease re-checks that its claim pod is still
// valid (design.md §4.7 garbage collection). Deliberately not a Pod watch: task
// instructions name only "own GPULease; watch Kueue Workloads" as this reconciler's
// watches, and the WarmPool reconciler (design.md §4.6) already proactively deletes a
// GPULease whose claim pod is gone the moment it notices (via its own Pod watch), making
// this periodic check a redundant safety net for the window before WarmPool's next
// reconcile, not the primary GC path. Matches WarmPool's own DefaultWarmPoolResyncPeriod.
const boundPollInterval = 15 * time.Second

// Event reasons emitted on the GPULease itself that are not already one of the
// api/v1alpha1.Reason* failure-reason constants (which double as Warning event reasons on
// failure, per task instructions "emit Events on every phase change/failure").
const (
	EventReasonQuotaReserved  = "QuotaReserved"
	EventReasonActivating     = "Activating"
	EventReasonBound          = "Bound"
	EventReasonReleased       = "LeaseReleased"
	EventReasonFlavorMismatch = "FlavorMismatch"
)

// GPULeaseReconciler implements the GPULease state machine of design.md §4.7:
// Pending -> Admitted -> Activating -> Bound, with Failed and Releasing as the two exits.
// It owns the backing Kueue Workload's full lifecycle (create, delete+recreate on shape
// mismatch, delete on release/failure) and the claim pod's lease REST calls
// (POST/DELETE /lease, GET /state) and controller-owned labels/annotations. It never
// decides *which* pod to promote or demote -- that is the WarmPoolReconciler's job
// (design.md §4.6); this reconciler only executes the lifecycle of a GPULease once one
// exists.
type GPULeaseReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Recorder emits Events on the GPULease for every phase change/failure. Defaulted by
	// SetupWithManager to mgr.GetEventRecorderFor("gpulease-controller") when nil; tests
	// may inject a record.NewFakeRecorder to assert on emitted events.
	Recorder record.EventRecorder

	// PodClient talks to the claim pod's lease REST API. Defaulted by SetupWithManager
	// to podclient.NewHTTPClient() when nil; tests inject podclient.NewFakeClient().
	PodClient podclient.Client

	// Inventory answers per-node free-GPU-count questions, used only to refresh the
	// NodeGPUsFree gauge (metrics.go). Defaulted by SetupWithManager to
	// inventory.New(mgr.GetClient()) when nil. A nil Inventory (as left by a test that
	// does not care about metrics) simply skips that gauge refresh.
	Inventory inventory.Inventory

	// Clock returns the current time; overridable by tests to deterministically drive
	// admission-timeout / activation-timeout without sleeping. Defaults to time.Now.
	Clock func() time.Time
}

// +kubebuilder:rbac:groups=poc.llm-d.ai,resources=gpuleases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=poc.llm-d.ai,resources=gpuleases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=poc.llm-d.ai,resources=gpuleases/finalizers,verbs=update

// Reconcile drives one GPULease through design.md §4.7's state machine. GPULease is
// cluster-scoped, so req.Namespace is always empty.
func (r *GPULeaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var lease poc.GPULease
	if err := r.Get(ctx, req.NamespacedName, &lease); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get GPULease: %w", err)
	}

	if lease.DeletionTimestamp != nil {
		res, err := r.reconcileDeletion(ctx, &lease)
		if merr := r.refreshMetrics(ctx); merr != nil {
			logger.Error(merr, "refresh metrics")
		}
		return res, err
	}

	// GPULeases are created with the finalizer already set (WarmPoolReconciler.
	// acquireLeaseOnNode), but defend against one created without it (e.g. directly by a
	// test or an operator) so the release/GC path is never skipped.
	if !controllerutil.ContainsFinalizer(&lease, poc.Finalizer) {
		controllerutil.AddFinalizer(&lease, poc.Finalizer)
		if err := r.Update(ctx, &lease); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	var res ctrl.Result
	var err error
	switch lease.Status.Phase {
	case "", poc.GPULeasePhasePending:
		res, err = r.reconcilePending(ctx, &lease)
	case poc.GPULeasePhaseAdmitted:
		res, err = r.reconcileAdmitted(ctx, &lease)
	case poc.GPULeasePhaseActivating:
		res, err = r.reconcileActivating(ctx, &lease)
	case poc.GPULeasePhaseBound:
		res, err = r.reconcileBound(ctx, &lease)
	case poc.GPULeasePhaseFailed:
		// Terminal: the lease stays exactly as it is until something external (WarmPool
		// or an operator) deletes it, at which point reconcileDeletion runs the release
		// path as an idempotent no-op (the Workload and pod state were already cleaned
		// up by transitionToFailed at the moment of failure).
	case poc.GPULeasePhaseReleasing:
		// Only ever set by reconcileDeletion, which always runs under a DeletionTimestamp
		// and is handled above; reaching this case with no DeletionTimestamp would mean
		// the phase was persisted but the process crashed before the finalizer could be
		// removed. Nothing more to do here: the next real deletion will retry cleanup.
	default:
		logger.Info("GPULease has unknown phase; ignoring", "phase", lease.Status.Phase)
	}

	if merr := r.refreshMetrics(ctx); merr != nil {
		logger.Error(merr, "refresh metrics")
	}
	return res, err
}

// reconcilePending creates (or repairs) the backing Workload and waits for Kueue to
// admit it (design.md §4.7's Pending state).
func (r *GPULeaseReconciler) reconcilePending(ctx context.Context, lease *poc.GPULease) (ctrl.Result, error) {
	want := buildWorkload(lease)

	var existing kueuev1beta2.Workload
	err := r.Get(ctx, client.ObjectKey{Namespace: want.Namespace, Name: want.Name}, &existing)
	switch {
	case apierrors.IsNotFound(err):
		return r.createWorkload(ctx, lease, want)
	case err != nil:
		return ctrl.Result{}, fmt.Errorf("get workload %s/%s: %w", want.Namespace, want.Name, err)
	}

	if !workloadMatchesSpec(&existing, want) {
		// spec.podSets (as a whole) and spec.queueName are both immutable once set on the
		// real Kueue Workload CRD (docs/kueue-spike.md gotchas #1/#2) -- there is no patch
		// that can reconcile a shape mismatch, only delete+recreate.
		if err := r.Delete(ctx, &existing); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("delete stale workload %s/%s: %w", existing.Namespace, existing.Name, err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	return r.evaluateAdmission(ctx, lease, &existing)
}

// createWorkload creates want as the backing Workload for lease, owned by lease (a
// cluster-scoped owner of a namespaced dependent, confirmed safe by both
// controllerutil.validateOwner and docs/kueue-spike.md gotcha #5) so native Kubernetes GC
// is a safety net if this controller ever fails to clean it up itself.
func (r *GPULeaseReconciler) createWorkload(ctx context.Context, lease *poc.GPULease, want *kueuev1beta2.Workload) (ctrl.Result, error) {
	if err := controllerutil.SetControllerReference(lease, want, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("set owner reference on workload %s/%s: %w", want.Namespace, want.Name, err)
	}
	if err := r.Create(ctx, want); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, fmt.Errorf("create workload %s/%s: %w", want.Namespace, want.Name, err)
	}

	if lease.Status.WorkloadName != want.Name {
		lease.Status.WorkloadName = want.Name
		if err := r.Status().Update(ctx, lease); err != nil {
			return ctrl.Result{}, fmt.Errorf("record workload name on lease: %w", err)
		}
	}
	return ctrl.Result{RequeueAfter: pollInterval}, nil
}

// evaluateAdmission inspects wl's Kueue-reported conditions and either advances lease to
// Admitted, fails it immediately (Inadmissible), fails it on admission-timeout, or
// requeues to keep waiting (design.md §4.7, §4.7.1).
func (r *GPULeaseReconciler) evaluateAdmission(ctx context.Context, lease *poc.GPULease, wl *kueuev1beta2.Workload) (ctrl.Result, error) {
	if isWorkloadAdmitted(wl) {
		return r.admitLease(ctx, lease, wl)
	}

	if bad, kueueMsg := workloadInadmissible(wl); bad {
		// design.md §4.7.1: "Handle as a distinct, non-retryable-by-waiting error" --
		// waiting out admission-timeout would never help a misconfigured LocalQueue.
		// poc.ReasonQueueMisconfigured names this precisely: it is a *configuration*
		// error (bad/missing LocalQueue or ClusterQueue, no flavor matching the pinned
		// node), not a capacity one, and must be kept distinct from
		// ReasonQuotaExhaustedOnNode because the two demand opposite operator
		// responses (fix the manifests vs. wait for capacity) and both surface as the
		// `reason` label on gpulease_lease_acquire_failures_total. Kueue's own message
		// is preserved verbatim in status.message for the operator.
		return r.transitionToFailed(ctx, lease, poc.ReasonQueueMisconfigured,
			fmt.Sprintf("Workload %s is Inadmissible (misconfigured LocalQueue %q): %s", wl.Name, wl.Spec.QueueName, kueueMsg))
	}

	settings := r.deploymentSettings(ctx, lease)
	elapsed := r.now().Sub(admissionAnchor(wl))
	if elapsed >= settings.AdmissionTimeout {
		return r.transitionToFailed(ctx, lease, poc.ReasonQuotaExhaustedOnNode,
			fmt.Sprintf("Workload %s not admitted within admission-timeout (%s): %s",
				wl.Name, settings.AdmissionTimeout, quotaReservedMessage(wl)))
	}
	return ctrl.Result{RequeueAfter: nextPoll(settings.AdmissionTimeout - elapsed)}, nil
}

// admitLease advances lease to Admitted once Kueue has admitted its Workload, recording
// the assigned ResourceFlavor and cross-checking it against the flavor expected for the
// lease's node (design.md item 3: best-effort/non-fatal, since nodeSelector pinning
// guarantees exactly one eligible flavor -- a mismatch "should never happen").
func (r *GPULeaseReconciler) admitLease(ctx context.Context, lease *poc.GPULease, wl *kueuev1beta2.Workload) (ctrl.Result, error) {
	flavor, _ := assignedFlavor(wl)
	message := fmt.Sprintf("admitted with ResourceFlavor %q", flavor)

	if expected, err := expectedFlavorForNode(ctx, r.Client, lease.Spec.NodeName); err != nil {
		log.FromContext(ctx).V(1).Info("skipping expected-flavor cross-check", "lease", lease.Name, "reason", err.Error())
	} else if expected != flavor {
		message = fmt.Sprintf("%s (expected %q for node %s; proceeding anyway, this check is diagnostic-only)", message, expected, lease.Spec.NodeName)
		r.event(lease, corev1.EventTypeWarning, EventReasonFlavorMismatch,
			"Workload %s admitted with flavor %q but node %s expected flavor %q", wl.Name, flavor, lease.Spec.NodeName, expected)
	}

	now := metav1.NewTime(r.now())
	lease.Status.Phase = poc.GPULeasePhaseAdmitted
	lease.Status.AdmittedAt = &now
	lease.Status.Message = message
	setLeaseCondition(lease, poc.ConditionQuotaReserved, metav1.ConditionTrue, "Admitted", message)
	if err := r.Status().Update(ctx, lease); err != nil {
		return ctrl.Result{}, fmt.Errorf("update lease status to Admitted: %w", err)
	}
	r.event(lease, corev1.EventTypeNormal, EventReasonQuotaReserved, "%s", message)
	return ctrl.Result{Requeue: true}, nil
}

// reconcileAdmitted issues POST /lease to the claim pod (design.md §4.7's Admitted
// state). An unreachable pod fails immediately (no timeout wait); success moves lease to
// Activating.
func (r *GPULeaseReconciler) reconcileAdmitted(ctx context.Context, lease *poc.GPULease) (ctrl.Result, error) {
	pod, err := r.getClaimPod(ctx, lease)
	if err != nil {
		return ctrl.Result{}, err
	}
	if reason := leaseOrphanReason(pod, lease, nil); reason != "" {
		return r.transitionToFailed(ctx, lease, reason, fmt.Sprintf("claim pod invalid before activation: reason %s", reason))
	}
	if pod.Status.PodIP == "" {
		return r.transitionToFailed(ctx, lease, poc.ReasonPodUnreachable,
			fmt.Sprintf("claim pod %s/%s has no PodIP yet", pod.Namespace, pod.Name))
	}

	activateErr := r.PodClient.Activate(ctx, pod.Status.PodIP, podclient.ActivateRequest{
		LeaseName: lease.Name,
		Node:      lease.Spec.NodeName,
		GPUID:     lease.Spec.GPUID,
		GPUModel:  lease.Spec.GPUModel,
	})
	switch {
	case errors.Is(activateErr, podclient.ErrNodeMismatch):
		return r.transitionToFailed(ctx, lease, poc.ReasonNodeMismatch,
			fmt.Sprintf("pod %s/%s rejected activation: node mismatch", pod.Namespace, pod.Name))
	case activateErr != nil:
		return r.transitionToFailed(ctx, lease, poc.ReasonPodUnreachable,
			fmt.Sprintf("POST /lease to pod %s/%s (%s) failed: %v", pod.Namespace, pod.Name, pod.Status.PodIP, activateErr))
	}

	message := fmt.Sprintf("POST /lease accepted by pod %s/%s; waiting for state hot", pod.Namespace, pod.Name)
	lease.Status.Phase = poc.GPULeasePhaseActivating
	lease.Status.Message = message
	setLeaseCondition(lease, poc.ConditionPodActivated, metav1.ConditionFalse, "Activating", message)
	if err := r.Status().Update(ctx, lease); err != nil {
		return ctrl.Result{}, fmt.Errorf("update lease status to Activating: %w", err)
	}
	r.event(lease, corev1.EventTypeNormal, EventReasonActivating, "%s", message)
	return ctrl.Result{RequeueAfter: pollInterval}, nil
}

// reconcileActivating polls GET /state until the pod reports "hot", or fails on
// activation-timeout (design.md §4.7's Activating state). A transient error from the
// pod's /state endpoint is treated as "still waiting" (it does not by itself justify
// PodUnreachable, which design.md reserves for the initial POST /lease failure);
// activation-timeout is the only way out of a pod that has gone permanently silent after
// accepting activation.
func (r *GPULeaseReconciler) reconcileActivating(ctx context.Context, lease *poc.GPULease) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	pod, err := r.getClaimPod(ctx, lease)
	if err != nil {
		return ctrl.Result{}, err
	}
	if reason := leaseOrphanReason(pod, lease, nil); reason != "" {
		return r.transitionToFailed(ctx, lease, reason, fmt.Sprintf("claim pod invalid during activation: reason %s", reason))
	}

	if state, err := r.PodClient.State(ctx, pod.Status.PodIP); err != nil {
		logger.Info("transient error polling pod state; will retry until activation-timeout",
			"lease", lease.Name, "pod", pod.Name, "error", err.Error())
	} else if state.State == podclient.StateHot {
		return r.bindLease(ctx, lease, pod)
	}

	settings := r.deploymentSettings(ctx, lease)
	elapsed := r.now().Sub(podActivatedAnchor(lease))
	if elapsed >= settings.ActivationTimeout {
		return r.transitionToFailed(ctx, lease, poc.ReasonActivationTimeout,
			fmt.Sprintf("pod %s/%s did not report state hot within activation-timeout (%s)",
				pod.Namespace, pod.Name, settings.ActivationTimeout))
	}
	return ctrl.Result{RequeueAfter: nextPoll(settings.ActivationTimeout - elapsed)}, nil
}

// bindLease advances lease to Bound: marks pod hot (label + annotations +
// pod-deletion-cost=100, design.md §4.4) and sets the Ready condition.
func (r *GPULeaseReconciler) bindLease(ctx context.Context, lease *poc.GPULease, pod *corev1.Pod) (ctrl.Result, error) {
	if err := markPodHot(ctx, r.Client, pod, lease); err != nil {
		return ctrl.Result{}, fmt.Errorf("mark pod hot: %w", err)
	}

	now := metav1.NewTime(r.now())
	message := fmt.Sprintf("pod %s/%s reported state hot", pod.Namespace, pod.Name)
	lease.Status.Phase = poc.GPULeasePhaseBound
	lease.Status.BoundAt = &now
	lease.Status.Message = message
	setLeaseCondition(lease, poc.ConditionPodActivated, metav1.ConditionTrue, "Hot", message)
	setLeaseCondition(lease, poc.ConditionReady, metav1.ConditionTrue, "Bound", message)
	if err := r.Status().Update(ctx, lease); err != nil {
		return ctrl.Result{}, fmt.Errorf("update lease status to Bound: %w", err)
	}
	r.event(lease, corev1.EventTypeNormal, EventReasonBound, "%s; lease bound", message)
	return ctrl.Result{RequeueAfter: boundPollInterval}, nil
}

// reconcileBound is the steady state (design.md §4.7's Bound state): periodically
// re-verify the claim pod is still valid (garbage collection -- pod gone, terminating, or
// moved node), failing the lease with ReasonPodGone/ReasonNodeMismatch if not. There is no
// Pod watch (see boundPollInterval's doc comment), so this poll is what catches a claim
// going stale between WarmPool reconciles.
func (r *GPULeaseReconciler) reconcileBound(ctx context.Context, lease *poc.GPULease) (ctrl.Result, error) {
	pod, err := r.getClaimPod(ctx, lease)
	if err != nil {
		return ctrl.Result{}, err
	}
	if reason := leaseOrphanReason(pod, lease, nil); reason != "" {
		return r.transitionToFailed(ctx, lease, reason, fmt.Sprintf("claim pod invalid while bound: reason %s", reason))
	}
	return ctrl.Result{RequeueAfter: boundPollInterval}, nil
}

// transitionToFailed is the shared cleanup+fail path used by every non-terminal phase's
// failure exit (design.md §4.7's literal "(delete Workload, pod->warm)" annotations):
// delete the backing Workload, best-effort release+revert the claim pod, record the
// failure metric/event, and persist Failed with reason/message. Workload deletion failure
// (other than NotFound) returns an error so the whole reconcile retries rather than
// recording Failed while a Workload (and its reserved quota) is left behind.
func (r *GPULeaseReconciler) transitionToFailed(ctx context.Context, lease *poc.GPULease, reason, message string) (ctrl.Result, error) {
	if err := r.deleteWorkload(ctx, lease); err != nil {
		return ctrl.Result{}, err
	}
	r.releaseClaimPod(ctx, lease)

	lease.Status.Phase = poc.GPULeasePhaseFailed
	lease.Status.Message = message
	setLeaseCondition(lease, poc.ConditionReady, metav1.ConditionFalse, reason, message)
	if err := r.Status().Update(ctx, lease); err != nil {
		return ctrl.Result{}, fmt.Errorf("update lease status to Failed: %w", err)
	}

	LeaseAcquireFailuresTotal.WithLabelValues(lease.Spec.DeploymentRef.Name, reason).Inc()
	r.event(lease, corev1.EventTypeWarning, reason, "%s", message)
	return ctrl.Result{}, nil
}

// reconcileDeletion implements the Releasing state (design.md §4.7): DELETE /lease,
// delete the Workload, remove the finalizer. It is idempotent (safe to run repeatedly,
// including as a no-op if a Failed lease already cleaned everything up) and must never
// block finalizer removal on an unreachable or already-gone pod -- releaseClaimPod only
// logs pod-related errors, it never returns one.
func (r *GPULeaseReconciler) reconcileDeletion(ctx context.Context, lease *poc.GPULease) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(lease, poc.Finalizer) {
		return ctrl.Result{}, nil
	}

	if lease.Status.Phase != poc.GPULeasePhaseReleasing {
		lease.Status.Phase = poc.GPULeasePhaseReleasing
		if err := r.Status().Update(ctx, lease); err != nil {
			return ctrl.Result{}, fmt.Errorf("update lease status to Releasing: %w", err)
		}
	}

	if err := r.deleteWorkload(ctx, lease); err != nil {
		return ctrl.Result{}, err
	}
	r.releaseClaimPod(ctx, lease)

	controllerutil.RemoveFinalizer(lease, poc.Finalizer)
	if err := r.Update(ctx, lease); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
	}
	r.event(lease, corev1.EventTypeNormal, EventReasonReleased,
		"released GPU lease (node %s, gpu %d)", lease.Spec.NodeName, lease.Spec.GPUID)
	return ctrl.Result{}, nil
}

// deleteWorkload deletes the Workload backing lease, tolerating it already being gone.
func (r *GPULeaseReconciler) deleteWorkload(ctx context.Context, lease *poc.GPULease) error {
	wl := &kueuev1beta2.Workload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workloadName(lease),
			Namespace: lease.Spec.ClaimRef.Namespace,
		},
	}
	if err := r.Delete(ctx, wl); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete workload %s/%s: %w", wl.Namespace, wl.Name, err)
	}
	return nil
}

// releaseClaimPod best-effort releases and reverts lease's claim pod: DELETE /lease, then
// revert the pod's state label/annotations to warm (design.md §4.4/§4.7). Every failure
// (pod gone, pod unreachable, patch conflict) is logged, never returned: this is called
// from both the finalizer path (which must never be blocked by an unreachable pod) and
// transitionToFailed (where the pod may already be gone, e.g. ReasonPodGone).
func (r *GPULeaseReconciler) releaseClaimPod(ctx context.Context, lease *poc.GPULease) {
	logger := log.FromContext(ctx)

	pod, err := r.getClaimPod(ctx, lease)
	if err != nil {
		logger.Error(err, "get claim pod for release (best effort)", "lease", lease.Name)
		return
	}
	if pod == nil {
		return
	}
	if lease.Spec.ClaimRef.UID != "" && pod.UID != lease.Spec.ClaimRef.UID {
		// The name was reused by an unrelated pod; there is nothing of ours to revert.
		return
	}

	if pod.Status.PodIP != "" {
		if err := r.PodClient.Release(ctx, pod.Status.PodIP); err != nil {
			logger.Error(err, "DELETE /lease on claim pod (best effort, ignoring)", "lease", lease.Name, "pod", pod.Name)
		}
	}
	if err := revertPodToWarm(ctx, r.Client, pod); err != nil {
		logger.Error(err, "revert claim pod to warm (best effort, ignoring)", "lease", lease.Name, "pod", pod.Name)
	}
}

// getClaimPod fetches lease's claim pod, returning (nil, nil) if it does not exist.
func (r *GPULeaseReconciler) getClaimPod(ctx context.Context, lease *poc.GPULease) (*corev1.Pod, error) {
	var pod corev1.Pod
	key := client.ObjectKey{Namespace: lease.Spec.ClaimRef.Namespace, Name: lease.Spec.ClaimRef.Name}
	if err := r.Get(ctx, key, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get claim pod %s: %w", key, err)
	}
	return &pod, nil
}

// markPodHot patches only the controller-owned keys that mark a pod hot (design.md §4.4):
// the state label, the lease/gpu-id/gpu-model annotations, and pod-deletion-cost=100. A
// merge patch (not Update) is used so a concurrent WarmPool write to unrelated pod fields
// is never clobbered.
func markPodHot(ctx context.Context, c client.Client, pod *corev1.Pod, lease *poc.GPULease) error {
	orig := pod.DeepCopy()
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	pod.Labels[poc.StateLabelKey] = poc.StateHot
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[poc.AnnotationLease] = lease.Name
	pod.Annotations[poc.AnnotationGPUID] = strconv.FormatInt(int64(lease.Spec.GPUID), 10)
	pod.Annotations[poc.AnnotationGPUModel] = lease.Spec.GPUModel
	pod.Annotations[poc.AnnotationPodDeletionCost] = poc.PodDeletionCostHot
	if err := c.Patch(ctx, pod, client.MergeFrom(orig)); err != nil {
		return fmt.Errorf("patch pod %s/%s hot: %w", pod.Namespace, pod.Name, err)
	}
	return nil
}

// revertPodToWarm patches only the controller-owned keys that revert a pod to warm
// (design.md §4.4/§4.7): the state label, pod-deletion-cost=-100, and strips the
// lease/gpu-id/gpu-model annotations entirely (a JSON merge patch represents "delete this
// key" as an explicit null, which client.MergeFrom's diff produces for a key removed from
// the modified object's map).
func revertPodToWarm(ctx context.Context, c client.Client, pod *corev1.Pod) error {
	orig := pod.DeepCopy()
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	pod.Labels[poc.StateLabelKey] = poc.StateWarm
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	delete(pod.Annotations, poc.AnnotationLease)
	delete(pod.Annotations, poc.AnnotationGPUID)
	delete(pod.Annotations, poc.AnnotationGPUModel)
	pod.Annotations[poc.AnnotationPodDeletionCost] = poc.PodDeletionCostWarm
	if err := c.Patch(ctx, pod, client.MergeFrom(orig)); err != nil {
		return fmt.Errorf("patch pod %s/%s warm: %w", pod.Namespace, pod.Name, err)
	}
	return nil
}

// setLeaseCondition sets one of lease.Status.Conditions via meta.SetStatusCondition
// (which only bumps LastTransitionTime when .Status actually flips -- the mechanism
// admissionAnchor/podActivatedAnchor rely on for timeout tracking), stamping the current
// ObservedGeneration.
func setLeaseCondition(lease *poc.GPULease, condType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&lease.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		ObservedGeneration: lease.Generation,
		Reason:             reason,
		Message:            message,
	})
}

// podActivatedAnchor returns the timestamp activation-timeout is measured against: the
// PodActivated condition's own LastTransitionTime (set False the moment POST /lease is
// accepted, in reconcileAdmitted), or lease.CreationTimestamp as a defensive fallback if
// that condition is somehow missing.
func podActivatedAnchor(lease *poc.GPULease) time.Time {
	if c := meta.FindStatusCondition(lease.Status.Conditions, poc.ConditionPodActivated); c != nil {
		return c.LastTransitionTime.Time
	}
	return lease.CreationTimestamp.Time
}

// nextPoll returns the delay before the next admission/activation poll: pollInterval,
// capped to whatever remains of the timeout budget so the reconciler does not overshoot
// past the deadline before re-checking.
func nextPoll(remaining time.Duration) time.Duration {
	if remaining < pollInterval {
		return remaining
	}
	return pollInterval
}

// deploymentSettings resolves lease's per-lease timeouts from its DeploymentRef's
// gpu-lease.llm-d.ai/* annotations (design.md §4.3): GPULeaseSpec itself carries no
// timeout fields by design, only LocalQueueName (resolved once at creation time by
// WarmPool). A missing/unreadable Deployment falls back to the documented defaults rather
// than blocking the reconcile -- a Deployment deleted out from under an in-flight lease
// must not wedge that lease's timeout handling.
func (r *GPULeaseReconciler) deploymentSettings(ctx context.Context, lease *poc.GPULease) poc.DeploymentSettings {
	var dep appsv1.Deployment
	key := client.ObjectKey{Namespace: lease.Spec.DeploymentRef.Namespace, Name: lease.Spec.DeploymentRef.Name}
	if err := r.Get(ctx, key, &dep); err != nil {
		if !apierrors.IsNotFound(err) {
			log.FromContext(ctx).Error(err, "get deployment for lease timeouts; using defaults", "lease", lease.Name, "deployment", key)
		}
		return poc.ParseDeploymentSettings(nil, lease.Spec.DeploymentRef.Name)
	}
	return poc.ParseDeploymentSettings(dep.Annotations, dep.Name)
}

// refreshMetrics recomputes the two gauges this reconciler owns (metrics.go):
// LeasesActive (every GPULease that currently exists, per node -- not just Bound ones,
// per metrics.go's own doc comment) and NodeGPUsFree (from Inventory.NodeFreeCounts).
// Errors are returned for logging only; a metrics refresh failure must never fail the
// reconcile it rode in on.
func (r *GPULeaseReconciler) refreshMetrics(ctx context.Context) error {
	var all poc.GPULeaseList
	if err := r.List(ctx, &all); err != nil {
		return fmt.Errorf("list GPULeases for metrics: %w", err)
	}
	counts := map[string]int{}
	for _, l := range all.Items {
		counts[l.Spec.NodeName]++
	}
	for node, n := range counts {
		LeasesActive.WithLabelValues(node).Set(float64(n))
	}

	if r.Inventory == nil {
		return nil
	}
	free, err := r.Inventory.NodeFreeCounts(ctx)
	if err != nil {
		return fmt.Errorf("node free GPU counts for metrics: %w", err)
	}
	for node, n := range free {
		NodeGPUsFree.WithLabelValues(node).Set(float64(n))
	}
	return nil
}

// event records an Event on lease if r.Recorder is set; a no-op otherwise (e.g. in unit
// tests that do not care about narration).
func (r *GPULeaseReconciler) event(lease *poc.GPULease, eventType, reason, messageFmt string, args ...interface{}) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(lease, eventType, reason, messageFmt, args...)
}

// now returns r.Clock() if set, else time.Now(); tests inject Clock to deterministically
// drive admission-timeout/activation-timeout.
func (r *GPULeaseReconciler) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

// SetupWithManager sets up the controller with the Manager. It watches GPULease (its
// primary resource) and Kueue Workloads it owns (design.md §4.1: the OwnerReference set
// by createWorkload), so an external change/deletion of a Workload -- e.g. Kueue itself
// reconciling admission -- promptly re-triggers this reconciler rather than waiting out
// pollInterval.
func (r *GPULeaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("gpulease-controller")
	}
	if r.PodClient == nil {
		r.PodClient = podclient.NewHTTPClient()
	}
	if r.Inventory == nil {
		r.Inventory = inventory.New(mgr.GetClient())
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&poc.GPULease{}).
		Owns(&kueuev1beta2.Workload{}).
		Named("gpulease").
		Complete(r)
}
