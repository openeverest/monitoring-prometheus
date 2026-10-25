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
	if !b.DeletionTimestamp.IsZero() || b.Spec.ClassRef == nil {
		return ctrl.Result{}, nil
	}
	ours, err := r.cc.OwnsClass(ctx, b.Spec.ClassRef.Name)
	if err != nil || !ours {
		return ctrl.Result{}, err
	}
	if !monitoring.IsAccepted(b) {
		reason, message := notAcceptedReason(b)
		return ctrl.Result{}, r.cc.SetConfigured(ctx, b, reason, message)
	}

	in, dst, err := r.cc.Resolve(ctx, b)
	if apierrors.IsNotFound(err) {
		return ctrl.Result{}, r.cc.SetConfigured(ctx, b, monitoringv1alpha1.ReasonRenderFailed, err.Error())
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	params, err := decodeParameters(dst.Spec.Parameters)
	if err != nil {
		return ctrl.Result{}, r.cc.SetConfigured(ctx, b, monitoringv1alpha1.ReasonRenderFailed,
			fmt.Sprintf("MonitoringDestination %s: %v", dst.Name, err))
	}
	sources := sourcesOf(in)
	if sources == nil || len(sources.Metrics) == 0 {
		return ctrl.Result{}, r.cc.SetConfigured(ctx, b, monitoringv1alpha1.ReasonEndpointNotServing,
			fmt.Sprintf("Instance %s publishes no metrics endpoints yet", in.Name))
	}
	podMonitors, err := podMonitorsFor(b, r.className, sources, params)
	if err != nil {
		return ctrl.Result{}, r.cc.SetConfigured(ctx, b, monitoringv1alpha1.ReasonRenderFailed, err.Error())
	}

	names, err := r.applyPodMonitors(ctx, b, podMonitors)
	if err != nil {
		return ctrl.Result{RequeueAfter: applyRetryInterval}, r.cc.SetConfigured(ctx, b, monitoringv1alpha1.ReasonApplyFailed, err.Error())
	}
	return ctrl.Result{}, r.cc.SetConfigured(ctx, b, monitoringv1alpha1.ReasonConfigured, "PodMonitors "+strings.Join(names, ", "))
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
		if err := r.client.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(ControllerName), client.ForceOwnership); err != nil {
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
// accepted: Retained while PodMonitors from an earlier acceptance are kept,
// NotAccepted when core rejected it, Pending while core has not decided.
func notAcceptedReason(b *monitoringv1alpha1.MonitoringBinding) (monitoringv1alpha1.ConfiguredReason, string) {
	accepted := meta.FindStatusCondition(b.Status.Conditions, monitoringv1alpha1.MonitoringBindingConditionAccepted)
	configured := meta.FindStatusCondition(b.Status.Conditions, monitoringv1alpha1.MonitoringBindingConditionConfigured)
	acceptedReason := "not evaluated yet"
	if accepted != nil && accepted.ObservedGeneration == b.Generation {
		acceptedReason = accepted.Reason
		if accepted.Message != "" {
			acceptedReason += ": " + accepted.Message
		}
	}
	switch {
	case configured != nil && (configured.Status == metav1.ConditionTrue || configured.Reason == string(monitoringv1alpha1.ReasonRetained)):
		return monitoringv1alpha1.ReasonRetained, "keeping the PodMonitors while the binding is not accepted (" + acceptedReason + ")"
	case accepted != nil && accepted.ObservedGeneration == b.Generation && accepted.Status == metav1.ConditionFalse:
		return monitoringv1alpha1.ReasonNotAccepted, "binding not accepted: " + acceptedReason
	default:
		return monitoringv1alpha1.ReasonPending, "waiting for the binding to be accepted"
	}
}

// pruneStale deletes the binding's PodMonitors for components the Instance
// no longer publishes.
func (r *bindingReconciler) pruneStale(ctx context.Context, b *monitoringv1alpha1.MonitoringBinding, keep []string) error {
	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion(podMonitorAPIVersion)
	list.SetKind(podMonitorKind + "List")
	if err := r.client.List(ctx, list, client.InNamespace(b.Namespace), client.MatchingLabels{
		monitoringv1alpha1.BindingInstanceLabel: b.Spec.InstanceRef.Name,
		monitoringv1alpha1.BindingClassLabel:    r.className,
	}); err != nil {
		return fmt.Errorf("list PodMonitors: %w", err)
	}
	for i := range list.Items {
		pm := &list.Items[i]
		owner := metav1.GetControllerOf(pm)
		if owner == nil || owner.UID != b.UID || slices.Contains(keep, pm.GetName()) {
			continue
		}
		if err := r.client.Delete(ctx, pm); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete stale PodMonitor %s: %w", pm.GetName(), err)
		}
	}
	return nil
}
