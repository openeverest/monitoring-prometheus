// Copyright (C) 2026 The OpenEverest Contributors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	monitoringv1alpha1 "github.com/openeverest/openeverest/v2/api/monitoring/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/monitoring"
)

// bindingReconciler turns one binding into PodMonitors.
type bindingReconciler struct {
	client    client.Client
	cc        monitoring.ClassController
	className string
}

func (r *bindingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	b := &monitoringv1alpha1.MonitoringBinding{}
	if err := r.client.Get(ctx, req.NamespacedName, b); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !b.DeletionTimestamp.IsZero() || !r.executes(b) {
		return ctrl.Result{}, nil
	}

	var reason monitoringv1alpha1.ConfiguredReason
	var message string
	var err error
	if monitoring.IsAccepted(b) {
		reason, message, err = r.render(ctx, b)
	} else {
		reason, message, err = r.notAcceptedReason(ctx, b)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	res := ctrl.Result{}
	if reason == monitoringv1alpha1.ReasonApplyFailed {
		res.RequeueAfter = applyRetryInterval
	}
	return res, r.cc.SetConfigured(ctx, b, reason, message)
}

// executes reports whether this controller is the Configured writer of the
// binding: an ExtensionManaged binding of the served class.
func (r *bindingReconciler) executes(b *monitoringv1alpha1.MonitoringBinding) bool {
	return b.Spec.ExecutionMode == corev1alpha1.MonitoringExecutionModeExtensionManaged &&
		b.Spec.ClassRef != nil && b.Spec.ClassRef.Name == r.className
}

// render brings the binding's PodMonitors in line with the Instance's
// sources and returns the Configured reason and message. Inputs that cannot
// be rendered keep the live PodMonitors; an Instance that publishes no
// metrics endpoints loses them all.
func (r *bindingReconciler) render(ctx context.Context, b *monitoringv1alpha1.MonitoringBinding) (monitoringv1alpha1.ConfiguredReason, string, error) {
	in, dst, err := r.cc.Resolve(ctx, b)
	if apierrors.IsNotFound(err) {
		return monitoringv1alpha1.ReasonRenderFailed, err.Error(), nil
	}
	if err != nil {
		return "", "", err
	}
	params, err := decodeParameters(dst.Spec.Parameters)
	if err != nil {
		return monitoringv1alpha1.ReasonRenderFailed, fmt.Sprintf("MonitoringDestination %s: %v", dst.Name, err), nil
	}
	sources := sourcesOf(in)
	if sources == nil {
		return monitoringv1alpha1.ReasonEndpointNotServing, fmt.Sprintf("Instance %s has not published monitoring sources yet", in.Name), nil
	}
	podMonitors, err := podMonitorsFor(b, r.className, sources, params)
	if err != nil {
		return monitoringv1alpha1.ReasonRenderFailed, fmt.Sprintf("Instance %s: %v", in.Name, err), nil
	}
	names, err := r.applyPodMonitors(ctx, b, podMonitors)
	if err != nil {
		return monitoringv1alpha1.ReasonApplyFailed, fmt.Sprintf("%v; retrying in %s", err, applyRetryInterval), nil
	}
	if len(names) == 0 {
		return monitoringv1alpha1.ReasonEndpointNotServing, fmt.Sprintf("Instance %s publishes no metrics endpoints", in.Name), nil
	}
	return monitoringv1alpha1.ReasonConfigured, "PodMonitors " + strings.Join(names, ", "), nil
}

// applyPodMonitors writes the rendered PodMonitors, deletes the binding's
// PodMonitors for components the Instance no longer publishes and returns
// the sorted names of the live ones.
func (r *bindingReconciler) applyPodMonitors(ctx context.Context, b *monitoringv1alpha1.MonitoringBinding, podMonitors []*podMonitor) ([]string, error) {
	names := make([]string, 0, len(podMonitors))
	for _, pm := range podMonitors {
		u, err := pm.toUnstructured()
		if err != nil {
			return nil, err
		}
		if err := r.client.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(FieldOwner), client.ForceOwnership); err != nil {
			return nil, fmt.Errorf("apply PodMonitor %s: %w", pm.Name, err)
		}
		names = append(names, pm.Name)
	}
	if err := r.pruneStale(ctx, b, names); err != nil {
		return nil, err
	}
	slices.Sort(names)
	return names, nil
}

// notAcceptedReason picks the Configured reason of a binding core has not
// accepted: Retained while the binding still owns PodMonitors, NotAccepted
// when core rejected it, Pending while core has not decided.
func (r *bindingReconciler) notAcceptedReason(ctx context.Context, b *monitoringv1alpha1.MonitoringBinding) (monitoringv1alpha1.ConfiguredReason, string, error) {
	accepted := meta.FindStatusCondition(b.Status.Conditions, monitoringv1alpha1.MonitoringBindingConditionAccepted)
	decided := accepted != nil && accepted.ObservedGeneration == b.Generation
	acceptedReason := "not evaluated yet"
	if decided {
		acceptedReason = accepted.Reason
		if accepted.Message != "" {
			acceptedReason += ": " + accepted.Message
		}
	}
	live, err := r.ownedPodMonitors(ctx, b)
	if err != nil {
		return "", "", err
	}
	switch {
	case len(live) > 0:
		return monitoringv1alpha1.ReasonRetained, "keeping the PodMonitors while the binding is not accepted (" + acceptedReason + ")", nil
	case decided && accepted.Status == metav1.ConditionFalse:
		return monitoringv1alpha1.ReasonNotAccepted, "binding not accepted: " + acceptedReason, nil
	default:
		return monitoringv1alpha1.ReasonPending, "waiting for the binding to be accepted", nil
	}
}

// ownedPodMonitors lists the PodMonitors the binding controls; there are
// none while the PodMonitor CRD is not installed.
func (r *bindingReconciler) ownedPodMonitors(ctx context.Context, b *monitoringv1alpha1.MonitoringBinding) ([]unstructured.Unstructured, error) {
	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion(podMonitorAPIVersion)
	list.SetKind(podMonitorKind + "List")
	err := r.client.List(ctx, list, client.InNamespace(b.Namespace), client.MatchingLabels{
		monitoringv1alpha1.BindingInstanceLabel: b.Spec.InstanceRef.Name,
		monitoringv1alpha1.BindingClassLabel:    r.className,
	})
	if meta.IsNoMatchError(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list PodMonitors: %w", err)
	}
	return slices.DeleteFunc(list.Items, func(pm unstructured.Unstructured) bool {
		return !metav1.IsControlledBy(&pm, b)
	}), nil
}

// pruneStale deletes the binding's PodMonitors for components the Instance
// no longer publishes.
func (r *bindingReconciler) pruneStale(ctx context.Context, b *monitoringv1alpha1.MonitoringBinding, keep []string) error {
	live, err := r.ownedPodMonitors(ctx, b)
	if err != nil {
		return err
	}
	for i := range live {
		pm := &live[i]
		if slices.Contains(keep, pm.GetName()) {
			continue
		}
		if err := r.client.Delete(ctx, pm); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete stale PodMonitor %s: %w", pm.GetName(), err)
		}
	}
	return nil
}
