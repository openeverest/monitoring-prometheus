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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	monitoringv1alpha1 "github.com/openeverest/openeverest/v2/api/monitoring/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/monitoring"
)

func TestClassReconcilerClaimsOurClass(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		className       string
		classController string
		want            *metav1.Condition
	}{
		{
			name:            "the served class",
			className:       "prometheus",
			classController: ControllerName,
			want: &metav1.Condition{
				Status: metav1.ConditionTrue, Reason: monitoringv1alpha1.ReasonAccepted, Message: "claimed by " + ControllerName, ObservedGeneration: 1,
			},
		},
		{
			name:            "a second class naming this controller",
			className:       "prometheus-b",
			classController: ControllerName,
		},
		{
			name:            "class of another controller",
			className:       "prometheus",
			classController: "openeverest.io/monitoring-victoriametrics",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			class := testClass(tt.classController)
			class.Name = tt.className
			c := newFakeClient(t, nil, class)
			r := &classReconciler{client: c, cc: monitoring.ClassController{Client: c, ControllerName: ControllerName}, className: "prometheus"}

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: tt.className}})
			require.NoError(t, err)

			got := &monitoringv1alpha1.MonitoringClass{}
			require.NoError(t, c.Get(ctx, client.ObjectKey{Name: tt.className}, got))
			accepted := meta.FindStatusCondition(got.Status.Conditions, monitoringv1alpha1.MonitoringClassConditionAccepted)
			if tt.want == nil {
				assert.Nil(t, accepted)
				return
			}
			require.NotNil(t, accepted)
			assert.Equal(t, tt.want.Status, accepted.Status)
			assert.Equal(t, tt.want.Reason, accepted.Reason)
			assert.Equal(t, tt.want.Message, accepted.Message)
			assert.Equal(t, tt.want.ObservedGeneration, accepted.ObservedGeneration)
		})
	}
}

func TestDestinationReconcilerReportsReady(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		classController string
		parameters      string
		want            *metav1.Condition
	}{
		{
			name:            "labels to be selected by",
			classController: ControllerName,
			parameters:      `{"podMonitorLabels":{"release":"kps","team":"dba"}}`,
			want:            &metav1.Condition{Status: metav1.ConditionTrue, Reason: string(monitoringv1alpha1.ReasonReady), Message: "PodMonitors are labelled release=kps,team=dba"},
		},
		{
			name:            "no parameters",
			classController: ControllerName,
			want:            &metav1.Condition{Status: metav1.ConditionTrue, Reason: string(monitoringv1alpha1.ReasonReady), Message: "PodMonitors are labelled {}"},
		},
		{
			name:            "parameters that cannot be rendered",
			classController: ControllerName,
			parameters:      `{"interval":"often"}`,
			want: &metav1.Condition{
				Status: metav1.ConditionFalse, Reason: string(monitoringv1alpha1.ReasonReadyInvalidParameters),
				Message: `interval "often" is not a Prometheus duration such as 30s or 1m`,
			},
		},
		{
			name:            "class of another controller",
			classController: "openeverest.io/monitoring-victoriametrics",
			parameters:      `{"podMonitorLabels":{"release":"kps"}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			c := newFakeClient(t, nil, testClass(tt.classController), testDestination(tt.parameters))
			r := &destinationReconciler{client: c, cc: monitoring.ClassController{Client: c, ControllerName: ControllerName}}

			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Namespace: testNamespace, Name: "prom"}})
			require.NoError(t, err)

			dst := &monitoringv1alpha1.MonitoringDestination{}
			require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: "prom"}, dst))
			got := meta.FindStatusCondition(dst.Status.Conditions, monitoringv1alpha1.MonitoringDestinationConditionReady)
			if tt.want == nil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, tt.want.Status, got.Status)
			assert.Equal(t, tt.want.Reason, got.Reason)
			assert.Equal(t, tt.want.Message, got.Message)
			assert.Equal(t, int64(1), got.ObservedGeneration)
		})
	}
}

func TestSourcesChanged(t *testing.T) {
	t.Parallel()

	withPhase := func(in *corev1alpha1.Instance, phase string) *corev1alpha1.Instance {
		in.Status.Phase = corev1alpha1.InstancePhase(phase)
		return in
	}
	tests := []struct {
		name     string
		old, new *corev1alpha1.Instance
		want     bool
	}{
		{
			name: "unrelated status change",
			old:  withPhase(testInstance(engineEndpoint()), "Creating"),
			new:  withPhase(testInstance(engineEndpoint()), "Running"),
			want: false,
		},
		{
			name: "endpoint published",
			old:  testInstance(),
			new:  testInstance(engineEndpoint()),
			want: true,
		},
		{
			name: "endpoint withdrawn",
			old:  testInstance(engineEndpoint(), proxyEndpoint()),
			new:  testInstance(engineEndpoint()),
			want: true,
		},
		{
			name: "sources first published",
			old:  &corev1alpha1.Instance{},
			new:  testInstance(),
			want: true,
		},
	}
	p := sourcesChanged()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, p.Update(event.UpdateEvent{ObjectOld: tt.old, ObjectNew: tt.new}))
		})
	}
	assert.False(t, p.Create(event.CreateEvent{Object: testInstance(engineEndpoint())}), "bindings are enqueued by their own create events")
}

func TestBindingWatchesEnqueueOnlyServedBindings(t *testing.T) {
	t.Parallel()
	in := testInstance()
	in.UID = "orders-uid"
	binding := func(name string, mutate func(*monitoringv1alpha1.MonitoringBinding)) client.Object {
		b := testBinding()
		b.Name, b.UID = name, types.UID(name)
		b.Labels = monitoring.BindingLabels("orders", name, "prom", "prometheus")
		b.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Instance", Name: in.Name, UID: in.UID, Controller: new(true),
		}}
		mutate(b)
		return b
	}
	c := newFakeClient(t, nil,
		binding("served", func(*monitoringv1alpha1.MonitoringBinding) {}),
		binding("provider-managed", func(b *monitoringv1alpha1.MonitoringBinding) {
			b.Spec.ExecutionMode = corev1alpha1.MonitoringExecutionModeProviderManaged
		}),
		binding("mode-unresolved", func(b *monitoringv1alpha1.MonitoringBinding) { b.Spec.ExecutionMode = "" }),
		binding("other-class", func(b *monitoringv1alpha1.MonitoringBinding) {
			b.Spec.ClassRef.Name = "victoriametrics"
			b.Labels[monitoringv1alpha1.BindingClassLabel] = "victoriametrics"
		}),
		binding("not-controlled", func(b *monitoringv1alpha1.MonitoringBinding) { b.OwnerReferences = nil }),
		binding("other-destination", func(b *monitoringv1alpha1.MonitoringBinding) {
			b.Spec.DestinationRef.Name = "other"
			b.Labels[monitoringv1alpha1.BindingDestinationLabel] = "other"
		}),
	)
	r := &bindingReconciler{client: c, className: "prometheus"}
	names := func(reqs []reconcile.Request) []string {
		out := make([]string, 0, len(reqs))
		for _, req := range reqs {
			out = append(out, req.Name)
		}
		return out
	}

	assert.ElementsMatch(t, []string{"served", "other-destination"}, names(r.bindingsForInstance(t.Context(), in)))
	assert.ElementsMatch(t, []string{"served", "not-controlled"}, names(r.bindingsForDestination(t.Context(), testDestination(""))))
}
