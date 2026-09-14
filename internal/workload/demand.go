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

package workload

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// DefaultDemandConfigMapName is the ConfigMap name design.md §5 watches by default,
// overridable by the DEMAND_CONFIGMAP env var.
const DefaultDemandConfigMapName = "gpu-lease-demand"

// DemandWatcher watches ConfigMap gpu-lease-demand (or the configured override) in
// the pod's own namespace and exposes the current demand (rps) for one deployment
// (design.md §5's "demand injection"). Key = deployment name, value = rps as a
// decimal string.
//
// It tolerates:
//   - the ConfigMap being absent at startup or deleted later (value stays at the
//     last known good reading, or 0 if there has never been one),
//   - the deployment's key being absent from the ConfigMap's data (same fallback),
//   - an unparseable value (same fallback; logged once per distinct bad value, not
//     on every resync, so a persistently bad ConfigMap does not spam the log).
//
// Run uses a client-go informer, whose Reflector already retries list/watch forever
// on any error (expired watch, apiserver hiccup, ...) -- Run blocks until ctx is
// cancelled, it never gives up and returns early.
type DemandWatcher struct {
	client     kubernetes.Interface
	namespace  string
	cmName     string
	deployment string
	logger     *slog.Logger

	// onUpdate, if non-nil, is called (outside the lock) every time the resolved
	// value is (re)computed, including with an unchanged value -- callers that only
	// care about changes should compare themselves.
	onUpdate func(float64)

	mu          sync.RWMutex
	value       float64
	lastWarning string // last logged warning message, to log-once-per-distinct-issue
}

// NewDemandWatcher builds a DemandWatcher. client is injected (rather than built
// in-cluster here) so tests can supply a fake clientset.
func NewDemandWatcher(client kubernetes.Interface, namespace, cmName, deployment string, onUpdate func(float64), logger *slog.Logger) *DemandWatcher {
	if cmName == "" {
		cmName = DefaultDemandConfigMapName
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &DemandWatcher{
		client:     client,
		namespace:  namespace,
		cmName:     cmName,
		deployment: deployment,
		onUpdate:   onUpdate,
		logger:     logger,
	}
}

// Value returns the current best-known demand (rps) for this watcher's deployment.
func (w *DemandWatcher) Value() float64 {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.value
}

// Run watches the ConfigMap until ctx is cancelled. Safe to run in its own goroutine.
func (w *DemandWatcher) Run(ctx context.Context) {
	selector := fields.OneTermEqualSelector("metadata.name", w.cmName).String()

	informerLW := &cache.ListWatch{
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			options.FieldSelector = selector
			return w.client.CoreV1().ConfigMaps(w.namespace).List(ctx, options)
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			options.FieldSelector = selector
			return w.client.CoreV1().ConfigMaps(w.namespace).Watch(ctx, options)
		},
	}

	handle := func(obj interface{}) {
		cm, ok := obj.(*corev1.ConfigMap)
		if !ok {
			return
		}
		w.applyConfigMap(cm)
	}

	_, controller := cache.NewInformer(informerLW, &corev1.ConfigMap{}, 0, cache.ResourceEventHandlerFuncs{
		AddFunc: handle,
		UpdateFunc: func(_, newObj interface{}) {
			handle(newObj)
		},
		DeleteFunc: func(obj interface{}) {
			// Tombstones on delete: keep the last known good value (design.md §5
			// tolerance) rather than resetting to 0. Just log for visibility.
			w.logger.Info("demand configmap deleted; keeping last known demand", "configmap", w.cmName, "namespace", w.namespace)
		},
	})

	w.logger.Info("watching demand configmap", "configmap", w.cmName, "namespace", w.namespace, "deployment", w.deployment)
	controller.Run(ctx.Done())
}

// applyConfigMap resolves this watcher's deployment key out of cm.Data and updates
// the current value, tolerating an absent key or unparseable value (design.md §5).
func (w *DemandWatcher) applyConfigMap(cm *corev1.ConfigMap) {
	raw, ok := cm.Data[w.deployment]
	if !ok {
		w.warnOnce(fmt.Sprintf("configmap %s/%s has no key %q; keeping last known demand", w.namespace, w.cmName, w.deployment))
		w.publish(w.fallback())
		return
	}

	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		w.warnOnce(fmt.Sprintf("configmap %s/%s key %q value %q is not a number; keeping last known demand", w.namespace, w.cmName, w.deployment, raw))
		w.publish(w.fallback())
		return
	}

	w.mu.Lock()
	w.value = v
	w.lastWarning = ""
	w.mu.Unlock()

	w.publish(v)
}

// fallback returns the last known good value, or 0 if there has never been one.
func (w *DemandWatcher) fallback() float64 {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.value
}

// publish invokes onUpdate outside the lock.
func (w *DemandWatcher) publish(v float64) {
	if w.onUpdate != nil {
		w.onUpdate(v)
	}
}

// warnOnce logs msg only if it differs from the last warning logged, so a
// persistently-bad ConfigMap (missing key, unparseable value) logs one line per
// distinct problem rather than spamming on every resync/update.
func (w *DemandWatcher) warnOnce(msg string) {
	w.mu.Lock()
	if w.lastWarning == msg {
		w.mu.Unlock()
		return
	}
	w.lastWarning = msg
	w.mu.Unlock()
	w.logger.Warn(msg)
}
