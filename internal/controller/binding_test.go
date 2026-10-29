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
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/kube-openapi/pkg/validation/spec"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	common "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	monitoringv1alpha1 "github.com/openeverest/openeverest/v2/api/monitoring/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/monitoring"
)

// crdModels holds the OpenAPI models of the monitoring CRDs shipped with the
// pinned core module, parsed once for all tests.
var crdModels = sync.OnceValues(loadCRDModels) //nolint:gochecknoglobals // parsed once per test binary

func loadCRDModels() (map[string]*spec.Schema, error) {
	out, err := exec.CommandContext(context.Background(), "go", "list", "-m", "-f", "{{.Dir}}", "github.com/openeverest/openeverest/v2").Output()
	if err != nil {
		return nil, err
	}
	crds := os.DirFS(filepath.Join(strings.TrimSpace(string(out)), "config", "crd", "bases"))
	models := map[string]*spec.Schema{}
	for _, plural := range []string{"monitoringclasses", "monitoringdestinations", "monitoringbindings"} {
		raw, err := fs.ReadFile(crds, "monitoring.openeverest.io_"+plural+".yaml")
		if err != nil {
			return nil, err
		}
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := yaml.Unmarshal(raw, crd); err != nil {
			return nil, err
		}
		for _, v := range crd.Spec.Versions {
			js, err := json.Marshal(v.Schema.OpenAPIV3Schema)
			if err != nil {
				return nil, err
			}
			s := &spec.Schema{}
			if err := json.Unmarshal(js, s); err != nil {
				return nil, err
			}
			s.AddExtension("x-kubernetes-group-version-kind", []any{
				map[string]any{"group": crd.Spec.Group, "version": v.Name, "kind": crd.Spec.Names.Kind},
			})
			models[crd.Spec.Group+"."+v.Name+"."+crd.Spec.Names.Kind] = s
		}
	}
	return models, nil
}

// crdTypeConverter makes the fake client's server-side apply follow the CRD
// schemas (listType=map conditions) instead of treating every list as atomic.
func crdTypeConverter(t *testing.T) managedfields.TypeConverter { //nolint:ireturn // the fake client takes the interface
	t.Helper()
	models, err := crdModels()
	require.NoError(t, err)
	tc, err := managedfields.NewTypeConverter(models, false)
	require.NoError(t, err)
	return tc
}

func newFakeClient(t *testing.T, funcs *interceptor.Funcs, objs ...client.Object) client.Client { //nolint:ireturn // the fake is used through the client interface
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(s))
	require.NoError(t, monitoringv1alpha1.AddToScheme(s))
	b := fake.NewClientBuilder().
		WithScheme(s).
		WithTypeConverters(crdTypeConverter(t), managedfields.NewDeducedTypeConverter()).
		WithObjects(objs...).
		WithStatusSubresource(
			&corev1alpha1.Instance{},
			&monitoringv1alpha1.MonitoringClass{},
			&monitoringv1alpha1.MonitoringDestination{},
			&monitoringv1alpha1.MonitoringBinding{},
		)
	if funcs != nil {
		b = b.WithInterceptorFuncs(*funcs)
	}
	return b.Build()
}

func testClass() *monitoringv1alpha1.MonitoringClass {
	return &monitoringv1alpha1.MonitoringClass{
		ObjectMeta: metav1.ObjectMeta{Name: "prometheus", Generation: 1},
		Spec: monitoringv1alpha1.MonitoringClassSpec{
			ExtensionManaged: &monitoringv1alpha1.ExtensionManagedFulfilment{},
			AuthModes:        []common.AuthMode{{Name: common.AuthModeAnonymous}},
		},
	}
}

func testDestination(parameters string) *monitoringv1alpha1.MonitoringDestination {
	dst := &monitoringv1alpha1.MonitoringDestination{
		ObjectMeta: metav1.ObjectMeta{Name: "prom", Namespace: testNamespace, Generation: 1},
		Spec: monitoringv1alpha1.MonitoringDestinationSpec{
			ClassRef:       common.ObjectRef{Name: "prometheus"},
			Authentication: common.Authentication{Mode: common.AuthModeAnonymous},
		},
	}
	if parameters != "" {
		dst.Spec.Parameters = &runtime.RawExtension{Raw: []byte(parameters)}
	}
	return dst
}

func testInstance(metrics ...corev1alpha1.MetricsEndpoint) *corev1alpha1.Instance {
	return &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: testNamespace},
		Spec:       corev1alpha1.InstanceSpec{ProviderRef: common.ObjectRef{Name: "percona-xtradb-cluster"}},
		Status: corev1alpha1.InstanceStatus{Monitoring: &corev1alpha1.InstanceMonitoringStatus{
			Sources: &corev1alpha1.MonitoringSources{
				Identity:    testIdentity(),
				Credentials: []corev1alpha1.MonitoringCredential{{Profile: "metrics", SecretRef: common.SecretRef{Name: "orders-monitoring-metrics"}}},
				Metrics:     metrics,
			},
		}},
	}
}

func engineEndpoint() corev1alpha1.MetricsEndpoint {
	return corev1alpha1.MetricsEndpoint{
		Component: "engine", Kind: "mysql", PodSelector: engineSelector(), Port: intstr.FromString("metrics"),
		Auth: &corev1alpha1.MetricsEndpointAuth{CredentialProfile: "metrics"},
	}
}

func proxyEndpoint() corev1alpha1.MetricsEndpoint {
	return corev1alpha1.MetricsEndpoint{
		Component: "proxy", Kind: "haproxy", Port: intstr.FromInt32(8404),
		PodSelector: map[string]string{monitoringv1alpha1.BindingInstanceLabel: "orders", "app.kubernetes.io/component": "haproxy"},
	}
}

func acceptedCondition(generation int64) metav1.Condition {
	return metav1.Condition{
		Type: monitoringv1alpha1.MonitoringBindingConditionAccepted, Status: metav1.ConditionTrue, Reason: monitoringv1alpha1.ReasonAccepted,
		ObservedGeneration: generation, LastTransitionTime: metav1.Now(),
	}
}

func acceptedBinding() *monitoringv1alpha1.MonitoringBinding {
	return bindingWithConditions(acceptedCondition(1))
}

func bindingWithConditions(conditions ...metav1.Condition) *monitoringv1alpha1.MonitoringBinding {
	b := testBinding()
	b.Status.Conditions = conditions
	return b
}

func rejectedCondition() metav1.Condition {
	return metav1.Condition{
		Type: monitoringv1alpha1.MonitoringBindingConditionAccepted, Status: metav1.ConditionFalse, ObservedGeneration: 1,
		Reason: monitoringv1alpha1.ReasonDestinationNotFound, Message: `MonitoringDestination "prom" not found`, LastTransitionTime: metav1.Now(),
	}
}

// podMonitorOwnedBy is a PodMonitor controlled by the named binding.
func podMonitorOwnedBy(name, binding string, uid types.UID) *unstructured.Unstructured {
	pm := &unstructured.Unstructured{}
	pm.SetAPIVersion("monitoring.coreos.com/v1")
	pm.SetKind("PodMonitor")
	pm.SetNamespace(testNamespace)
	pm.SetName(name)
	pm.SetLabels(map[string]string{monitoringv1alpha1.BindingInstanceLabel: "orders", monitoringv1alpha1.BindingClassLabel: "prometheus"})
	pm.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: "monitoring.openeverest.io/v1alpha1", Kind: "MonitoringBinding", Name: binding, UID: uid, Controller: new(true),
	}})
	return pm
}

func reconcileBinding(t *testing.T, c client.Client) ctrl.Result {
	t.Helper()
	r := &bindingReconciler{client: c, cc: monitoring.ClassController{Client: c, FieldOwner: FieldOwner, ClassName: "prometheus"}, className: "prometheus"}
	res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKey{Namespace: testNamespace, Name: testBindingName}})
	require.NoError(t, err)
	return res
}

func bindingCondition(t *testing.T, c client.Client, condType string) *metav1.Condition {
	t.Helper()
	b := &monitoringv1alpha1.MonitoringBinding{}
	require.NoError(t, c.Get(t.Context(), client.ObjectKey{Namespace: testNamespace, Name: testBindingName}, b))
	return meta.FindStatusCondition(b.Status.Conditions, condType)
}

// livePodMonitors returns the PodMonitors stored in the fake API server.
func livePodMonitors(t *testing.T, c client.Client) map[string]podMonitor {
	t.Helper()
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "PodMonitorList"})
	require.NoError(t, c.List(t.Context(), list, client.InNamespace(testNamespace)))
	out := map[string]podMonitor{}
	for _, u := range list.Items {
		data, err := u.MarshalJSON()
		require.NoError(t, err)
		pm := podMonitor{}
		require.NoError(t, json.Unmarshal(data, &pm))
		out[pm.Name] = pm
	}
	return out
}

func TestReconcileAppliesPodMonitorsAndReportsConfigured(t *testing.T) {
	t.Parallel()
	c := newFakeClient(t, nil,
		testClass(),
		testDestination(`{"podMonitorLabels":{"release":"kps"},"interval":"30s"}`),
		testInstance(engineEndpoint(), proxyEndpoint()),
		acceptedBinding(),
	)

	res := reconcileBinding(t, c)
	assert.Zero(t, res)

	live := livePodMonitors(t, c)
	require.Len(t, live, 2)
	engine := live[testBindingName+"-engine"]
	assert.Equal(t, "kps", engine.Labels["release"])
	require.Len(t, engine.OwnerReferences, 1)
	assert.Equal(t, testBindingUID, engine.OwnerReferences[0].UID)
	require.Len(t, engine.Spec.PodMetricsEndpoints, 1)
	assert.Equal(t, "30s", engine.Spec.PodMetricsEndpoints[0].Interval)
	require.NotNil(t, engine.Spec.PodMetricsEndpoints[0].BasicAuth)
	assert.Equal(t, "orders-monitoring-metrics", engine.Spec.PodMetricsEndpoints[0].BasicAuth.Password.Name)
	proxy := live[testBindingName+"-proxy"]
	require.Len(t, proxy.Spec.PodMetricsEndpoints, 1)
	assert.Equal(t, int32(8404), proxy.Spec.PodMetricsEndpoints[0].PortNumber)

	configured := bindingCondition(t, c, monitoringv1alpha1.MonitoringBindingConditionConfigured)
	require.NotNil(t, configured)
	assert.Equal(t, metav1.ConditionTrue, configured.Status)
	assert.Equal(t, string(monitoringv1alpha1.ReasonConfigured), configured.Reason)
	assert.Equal(t, "PodMonitors "+testBindingName+"-engine, "+testBindingName+"-proxy", configured.Message)
	accepted := bindingCondition(t, c, monitoringv1alpha1.MonitoringBindingConditionAccepted)
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionTrue, accepted.Status, "core's Accepted condition survives the controller's apply")
}

func TestReconcileReportsWhyNothingIsConfigured(t *testing.T) {
	t.Parallel()

	noPodMonitorCRD := &interceptor.Funcs{
		Apply: func(context.Context, client.WithWatch, runtime.ApplyConfiguration, ...client.ApplyOption) error {
			return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "monitoring.coreos.com", Kind: "PodMonitor"}, SearchedVersions: []string{"v1"}}
		},
	}
	wasConfigured := metav1.Condition{
		Type: monitoringv1alpha1.MonitoringBindingConditionConfigured, Status: metav1.ConditionTrue, ObservedGeneration: 1,
		Reason: string(monitoringv1alpha1.ReasonConfigured), LastTransitionTime: metav1.Now(),
	}

	tests := []struct {
		name        string
		binding     *monitoringv1alpha1.MonitoringBinding
		destination *monitoringv1alpha1.MonitoringDestination
		instance    *corev1alpha1.Instance
		funcs       *interceptor.Funcs
		wantStatus  metav1.ConditionStatus
		wantReason  monitoringv1alpha1.ConfiguredReason
		wantMessage string
		wantRequeue bool
	}{
		{
			name:        "invalid destination parameters",
			binding:     acceptedBinding(),
			destination: testDestination(`{"podMonitorLabels":{"not a key":"x"}}`),
			instance:    testInstance(engineEndpoint()),
			wantStatus:  metav1.ConditionFalse,
			wantReason:  monitoringv1alpha1.ReasonRenderFailed,
			wantMessage: `MonitoringDestination prom: podMonitorLabels: invalid key "not a key"`,
		},
		{
			name:        "destination gone",
			binding:     acceptedBinding(),
			instance:    testInstance(engineEndpoint()),
			wantStatus:  metav1.ConditionFalse,
			wantReason:  monitoringv1alpha1.ReasonRenderFailed,
			wantMessage: `get MonitoringDestination "prom"`,
		},
		{
			name:        "Instance publishes no endpoints",
			binding:     acceptedBinding(),
			destination: testDestination(""),
			instance:    testInstance(),
			wantStatus:  metav1.ConditionFalse,
			wantReason:  monitoringv1alpha1.ReasonEndpointNotServing,
			wantMessage: "Instance orders publishes no metrics endpoints",
		},
		{
			name:        "endpoint naming an unpublished credential profile",
			binding:     acceptedBinding(),
			destination: testDestination(""),
			instance: testInstance(corev1alpha1.MetricsEndpoint{
				Component: "engine", Kind: "mysql", PodSelector: engineSelector(), Port: intstr.FromString("metrics"),
				Auth: &corev1alpha1.MetricsEndpointAuth{CredentialProfile: qanProfile},
			}),
			wantStatus:  metav1.ConditionFalse,
			wantReason:  monitoringv1alpha1.ReasonRenderFailed,
			wantMessage: `credential profile "queryAnalytics" is not published by the Instance`,
		},
		{
			name:        "Prometheus Operator CRDs not installed",
			binding:     acceptedBinding(),
			destination: testDestination(""),
			instance:    testInstance(engineEndpoint()),
			funcs:       noPodMonitorCRD,
			wantStatus:  metav1.ConditionFalse,
			wantReason:  monitoringv1alpha1.ReasonApplyFailed,
			wantMessage: `apply PodMonitor ` + testBindingName + `-engine: no matches for kind "PodMonitor"`,
			wantRequeue: true,
		},
		{
			name:        "core has not decided yet",
			binding:     bindingWithConditions(),
			destination: testDestination(""),
			instance:    testInstance(engineEndpoint()),
			wantStatus:  metav1.ConditionUnknown,
			wantReason:  monitoringv1alpha1.ReasonPending,
			wantMessage: "waiting for the binding to be accepted",
		},
		{
			name:        "accepted for an older generation",
			binding:     bindingWithConditions(acceptedCondition(0)),
			destination: testDestination(""),
			instance:    testInstance(engineEndpoint()),
			wantStatus:  metav1.ConditionUnknown,
			wantReason:  monitoringv1alpha1.ReasonPending,
			wantMessage: "waiting for the binding to be accepted",
		},
		{
			name:        "rejected by core",
			binding:     bindingWithConditions(rejectedCondition()),
			instance:    testInstance(engineEndpoint()),
			wantStatus:  metav1.ConditionFalse,
			wantReason:  monitoringv1alpha1.ReasonNotAccepted,
			wantMessage: `binding not accepted: DestinationNotFound: MonitoringDestination "prom" not found`,
		},
		{
			name:        "rejected after an earlier configuration whose PodMonitors are gone",
			binding:     bindingWithConditions(rejectedCondition(), wasConfigured),
			instance:    testInstance(engineEndpoint()),
			wantStatus:  metav1.ConditionFalse,
			wantReason:  monitoringv1alpha1.ReasonNotAccepted,
			wantMessage: `binding not accepted: DestinationNotFound: MonitoringDestination "prom" not found`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			objs := []client.Object{testClass(), tt.binding, tt.instance}
			if tt.destination != nil {
				objs = append(objs, tt.destination)
			}
			c := newFakeClient(t, tt.funcs, objs...)

			res := reconcileBinding(t, c)

			configured := bindingCondition(t, c, monitoringv1alpha1.MonitoringBindingConditionConfigured)
			require.NotNil(t, configured)
			assert.Equal(t, tt.wantStatus, configured.Status)
			assert.Equal(t, string(tt.wantReason), configured.Reason)
			assert.Contains(t, configured.Message, tt.wantMessage)
			if tt.wantRequeue {
				assert.Equal(t, applyRetryInterval, res.RequeueAfter)
			} else {
				assert.Zero(t, res)
			}
			assert.Empty(t, livePodMonitors(t, c))
		})
	}
}

func TestReconcileKeepsLivePodMonitorsItCannotRender(t *testing.T) {
	t.Parallel()

	unpublished := testInstance()
	unpublished.Status.Monitoring = nil
	foreignPods := engineEndpoint()
	foreignPods.PodSelector = map[string]string{monitoringv1alpha1.BindingInstanceLabel: "payments"}

	tests := []struct {
		name        string
		binding     *monitoringv1alpha1.MonitoringBinding
		destination *monitoringv1alpha1.MonitoringDestination
		instance    *corev1alpha1.Instance
		wantStatus  metav1.ConditionStatus
		wantReason  monitoringv1alpha1.ConfiguredReason
		wantMessage string
	}{
		{
			name:        "rejected by core",
			binding:     bindingWithConditions(rejectedCondition()),
			instance:    testInstance(engineEndpoint()),
			wantStatus:  metav1.ConditionUnknown,
			wantReason:  monitoringv1alpha1.ReasonRetained,
			wantMessage: `keeping the PodMonitors while the binding is not accepted (DestinationNotFound: MonitoringDestination "prom" not found)`,
		},
		{
			name:        "core has not decided yet",
			binding:     bindingWithConditions(),
			destination: testDestination(""),
			instance:    testInstance(engineEndpoint()),
			wantStatus:  metav1.ConditionUnknown,
			wantReason:  monitoringv1alpha1.ReasonRetained,
			wantMessage: "keeping the PodMonitors while the binding is not accepted (not evaluated yet)",
		},
		{
			name:        "destination gone",
			binding:     acceptedBinding(),
			instance:    testInstance(engineEndpoint()),
			wantStatus:  metav1.ConditionFalse,
			wantReason:  monitoringv1alpha1.ReasonRenderFailed,
			wantMessage: `get MonitoringDestination "prom"`,
		},
		{
			name:        "invalid destination parameters",
			binding:     acceptedBinding(),
			destination: testDestination(`{"interval":"often"}`),
			instance:    testInstance(engineEndpoint()),
			wantStatus:  metav1.ConditionFalse,
			wantReason:  monitoringv1alpha1.ReasonRenderFailed,
			wantMessage: `MonitoringDestination prom: interval "often" is not a Prometheus duration`,
		},
		{
			name:        "endpoint selecting the pods of another Instance",
			binding:     acceptedBinding(),
			destination: testDestination(""),
			instance:    testInstance(foreignPods),
			wantStatus:  metav1.ConditionFalse,
			wantReason:  monitoringv1alpha1.ReasonRenderFailed,
			wantMessage: `component "engine": pod selector does not select core.openeverest.io/instance=orders`,
		},
		{
			name:        "sources not published",
			binding:     acceptedBinding(),
			destination: testDestination(""),
			instance:    unpublished,
			wantStatus:  metav1.ConditionFalse,
			wantReason:  monitoringv1alpha1.ReasonEndpointNotServing,
			wantMessage: "Instance orders has not published monitoring sources yet",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			objs := []client.Object{
				testClass(), tt.binding, tt.instance,
				podMonitorOwnedBy(testBindingName+"-engine", testBindingName, testBindingUID),
			}
			if tt.destination != nil {
				objs = append(objs, tt.destination)
			}
			c := newFakeClient(t, nil, objs...)

			res := reconcileBinding(t, c)

			assert.Zero(t, res)
			configured := bindingCondition(t, c, monitoringv1alpha1.MonitoringBindingConditionConfigured)
			require.NotNil(t, configured)
			assert.Equal(t, tt.wantStatus, configured.Status)
			assert.Equal(t, string(tt.wantReason), configured.Reason)
			assert.Contains(t, configured.Message, tt.wantMessage)
			live := livePodMonitors(t, c)
			assert.Len(t, live, 1)
			assert.Contains(t, live, testBindingName+"-engine")
		})
	}
}

func TestReconcileWithoutThePodMonitorCRD(t *testing.T) {
	t.Parallel()

	noPodMonitorCRD := &interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if u, ok := list.(*unstructured.UnstructuredList); ok && u.GetKind() == "PodMonitorList" {
				return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "monitoring.coreos.com", Kind: "PodMonitor"}, SearchedVersions: []string{"v1"}}
			}
			return c.List(ctx, list, opts...)
		},
	}
	tests := []struct {
		name        string
		binding     *monitoringv1alpha1.MonitoringBinding
		instance    *corev1alpha1.Instance
		wantReason  monitoringv1alpha1.ConfiguredReason
		wantMessage string
	}{
		{
			name:        "rejected by core",
			binding:     bindingWithConditions(rejectedCondition()),
			instance:    testInstance(engineEndpoint()),
			wantReason:  monitoringv1alpha1.ReasonNotAccepted,
			wantMessage: `binding not accepted: DestinationNotFound: MonitoringDestination "prom" not found`,
		},
		{
			name:        "Instance publishes no endpoints",
			binding:     acceptedBinding(),
			instance:    testInstance(),
			wantReason:  monitoringv1alpha1.ReasonEndpointNotServing,
			wantMessage: "Instance orders publishes no metrics endpoints",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newFakeClient(t, noPodMonitorCRD, testClass(), testDestination(""), tt.instance, tt.binding)

			res := reconcileBinding(t, c)

			assert.Zero(t, res)
			configured := bindingCondition(t, c, monitoringv1alpha1.MonitoringBindingConditionConfigured)
			require.NotNil(t, configured)
			assert.Equal(t, string(tt.wantReason), configured.Reason)
			assert.Equal(t, tt.wantMessage, configured.Message)
		})
	}
}

func TestReconcileIgnoresBindingsItDoesNotServe(t *testing.T) {
	t.Parallel()

	withMode := func(mode corev1alpha1.MonitoringExecutionMode) *monitoringv1alpha1.MonitoringBinding {
		b := acceptedBinding()
		b.Spec.ExecutionMode = mode
		return b
	}
	ofOtherClass := acceptedBinding()
	ofOtherClass.Spec.ClassRef = &common.ObjectRef{Name: "victoriametrics"}

	tests := []struct {
		name    string
		binding *monitoringv1alpha1.MonitoringBinding
	}{
		{
			name:    "ProviderManaged binding of the class",
			binding: withMode(corev1alpha1.MonitoringExecutionModeProviderManaged),
		},
		{
			name:    "binding whose mode core has not resolved",
			binding: withMode(""),
		},
		{
			name:    "binding of another class",
			binding: ofOtherClass,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newFakeClient(t, nil, testClass(), testDestination(""), testInstance(engineEndpoint()), tt.binding)

			res := reconcileBinding(t, c)

			assert.Zero(t, res)
			assert.Nil(t, bindingCondition(t, c, monitoringv1alpha1.MonitoringBindingConditionConfigured))
			assert.Empty(t, livePodMonitors(t, c))
		})
	}
}

func TestReconcilePrunesPodMonitorsOfComponentsNoLongerPublished(t *testing.T) {
	t.Parallel()
	foreign := podMonitorOwnedBy("orders-prometheus-0000000-proxy", "orders-prometheus-0000000", "other")
	c := newFakeClient(t, nil,
		testClass(),
		testDestination(""),
		testInstance(engineEndpoint(), proxyEndpoint()),
		acceptedBinding(),
		foreign,
	)
	reconcileBinding(t, c)
	require.Len(t, livePodMonitors(t, c), 3)

	publish := func(metrics ...corev1alpha1.MetricsEndpoint) {
		in := &corev1alpha1.Instance{}
		require.NoError(t, c.Get(t.Context(), client.ObjectKey{Namespace: testNamespace, Name: "orders"}, in))
		in.Status.Monitoring.Sources.Metrics = metrics
		require.NoError(t, c.Status().Update(t.Context(), in))
	}

	publish(engineEndpoint())
	reconcileBinding(t, c)
	live := livePodMonitors(t, c)
	assert.Len(t, live, 2)
	assert.Contains(t, live, testBindingName+"-engine")
	assert.Contains(t, live, foreign.GetName(), "PodMonitors of other bindings are left alone")
	configured := bindingCondition(t, c, monitoringv1alpha1.MonitoringBindingConditionConfigured)
	require.NotNil(t, configured)
	assert.Equal(t, "PodMonitors "+testBindingName+"-engine", configured.Message)

	publish()
	reconcileBinding(t, c)
	live = livePodMonitors(t, c)
	assert.Len(t, live, 1)
	assert.Contains(t, live, foreign.GetName())
	configured = bindingCondition(t, c, monitoringv1alpha1.MonitoringBindingConditionConfigured)
	require.NotNil(t, configured)
	assert.Equal(t, string(monitoringv1alpha1.ReasonEndpointNotServing), configured.Reason)
	assert.Equal(t, "Instance orders publishes no metrics endpoints", configured.Message)
}
