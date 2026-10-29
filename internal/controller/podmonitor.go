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
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/prometheus/common/model"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	monitoringv1alpha1 "github.com/openeverest/openeverest/v2/api/monitoring/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/monitoring"
)

const (
	podMonitorAPIVersion = "monitoring.coreos.com/v1"
	podMonitorKind       = "PodMonitor"
)

// destinationParameters is MonitoringDestination.spec.parameters for a
// prometheus class.
type destinationParameters struct {
	// PodMonitorLabels are put on every PodMonitor so the target Prometheus
	// selects them through its podMonitorSelector.
	PodMonitorLabels map[string]string `json:"podMonitorLabels,omitempty"`
	// Interval is the scrape interval; empty uses the Prometheus default.
	Interval string `json:"interval,omitempty"`
}

func decodeParameters(raw *runtime.RawExtension) (destinationParameters, error) {
	p := destinationParameters{}
	if raw == nil || len(raw.Raw) == 0 {
		return p, nil
	}
	if err := json.Unmarshal(raw.Raw, &p); err != nil {
		return p, fmt.Errorf("decode parameters: %w", err)
	}
	for _, k := range slices.Sorted(maps.Keys(p.PodMonitorLabels)) {
		if k == monitoringv1alpha1.BindingInstanceLabel || strings.HasPrefix(k, monitoringv1alpha1.GroupVersion.Group+"/") {
			return p, fmt.Errorf("podMonitorLabels: key %q is reserved for OpenEverest", k)
		}
		if errs := validation.IsQualifiedName(k); len(errs) > 0 {
			return p, fmt.Errorf("podMonitorLabels: invalid key %q: %s", k, strings.Join(errs, "; "))
		}
		if errs := validation.IsValidLabelValue(p.PodMonitorLabels[k]); len(errs) > 0 {
			return p, fmt.Errorf("podMonitorLabels: invalid value for %q: %s", k, strings.Join(errs, "; "))
		}
	}
	if p.Interval != "" {
		if _, err := model.ParseDuration(p.Interval); err != nil {
			return p, fmt.Errorf("interval %q is not a Prometheus duration such as 30s or 1m", p.Interval)
		}
	}
	return p, nil
}

// The types below mirror the subset of the Prometheus Operator PodMonitor
// (monitoring.coreos.com/v1) this controller writes.

type podMonitor struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`

	Spec podMonitorSpec `json:"spec"`
}

type podMonitorSpec struct {
	Selector            metav1.LabelSelector `json:"selector"`
	NamespaceSelector   namespaceSelector    `json:"namespaceSelector"`
	PodMetricsEndpoints []podMetricsEndpoint `json:"podMetricsEndpoints"`
}

type namespaceSelector struct {
	MatchNames []string `json:"matchNames"`
}

type podMetricsEndpoint struct {
	Port        string              `json:"port,omitempty"`
	PortNumber  int32               `json:"portNumber,omitempty"`
	Path        string              `json:"path"`
	Scheme      string              `json:"scheme"`
	Params      map[string][]string `json:"params,omitempty"`
	Interval    string              `json:"interval,omitempty"`
	TLSConfig   *tlsConfig          `json:"tlsConfig,omitempty"`
	BasicAuth   *basicAuth          `json:"basicAuth,omitempty"`
	Relabelings []relabelConfig     `json:"relabelings"`
}

type tlsConfig struct {
	CA                 *secretOrConfigMap `json:"ca,omitempty"`
	ServerName         string             `json:"serverName,omitempty"`
	InsecureSkipVerify bool               `json:"insecureSkipVerify,omitempty"`
}

type secretOrConfigMap struct {
	Secret *corev1.SecretKeySelector `json:"secret"`
}

type basicAuth struct {
	Username corev1.SecretKeySelector `json:"username"`
	Password corev1.SecretKeySelector `json:"password"`
}

type relabelConfig struct {
	SourceLabels []string `json:"sourceLabels,omitempty"`
	TargetLabel  string   `json:"targetLabel"`
	Replacement  *string  `json:"replacement,omitempty"`
	Action       string   `json:"action"`
}

func (pm *podMonitor) toUnstructured() (*unstructured.Unstructured, error) {
	data, err := json.Marshal(pm)
	if err != nil {
		return nil, fmt.Errorf("marshal PodMonitor %s: %w", pm.Name, err)
	}
	u := &unstructured.Unstructured{}
	if err := u.UnmarshalJSON(data); err != nil {
		return nil, fmt.Errorf("unmarshal PodMonitor %s: %w", pm.Name, err)
	}
	return u, nil
}

// podMonitorsFor renders one PodMonitor per component of the Instance's
// published metrics endpoints, in the order the components first appear.
// A PodMonitor has a single pod selector, so all endpoints of a component
// must select the same pods, and only pods of the bound Instance, since the
// scrape carries that Instance's credentials.
func podMonitorsFor(b *monitoringv1alpha1.MonitoringBinding, className string, sources *corev1alpha1.MonitoringSources, params destinationParameters) ([]*podMonitor, error) {
	var out []*podMonitor
	byComponent := map[string]*podMonitor{}
	for _, ep := range sources.Metrics {
		if ep.PodSelector[monitoringv1alpha1.BindingInstanceLabel] != b.Spec.InstanceRef.Name {
			return nil, fmt.Errorf("component %q: pod selector does not select %s=%s",
				ep.Component, monitoringv1alpha1.BindingInstanceLabel, b.Spec.InstanceRef.Name)
		}
		pm, ok := byComponent[ep.Component]
		if !ok {
			pm = podMonitorShell(b, className, ep, params)
			byComponent[ep.Component] = pm
			out = append(out, pm)
		} else if !equality.Semantic.DeepEqual(pm.Spec.Selector.MatchLabels, ep.PodSelector) {
			return nil, fmt.Errorf("metrics endpoints of component %q select different pods", ep.Component)
		}
		endpoint, err := podMetricsEndpointFor(sources, ep, params)
		if err != nil {
			return nil, fmt.Errorf("component %q: %w", ep.Component, err)
		}
		pm.Spec.PodMetricsEndpoints = append(pm.Spec.PodMetricsEndpoints, endpoint)
	}
	return out, nil
}

// podMonitorShell builds the PodMonitor for one component, owned by the
// binding, without endpoints.
func podMonitorShell(b *monitoringv1alpha1.MonitoringBinding, className string, ep corev1alpha1.MetricsEndpoint, params destinationParameters) *podMonitor {
	labels := maps.Clone(params.PodMonitorLabels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[monitoringv1alpha1.BindingInstanceLabel] = b.Spec.InstanceRef.Name
	labels[monitoringv1alpha1.BindingClassLabel] = className
	return &podMonitor{
		TypeMeta: metav1.TypeMeta{APIVersion: podMonitorAPIVersion, Kind: podMonitorKind},
		ObjectMeta: metav1.ObjectMeta{
			// Component names such as configServer are not DNS labels.
			Name:            b.Name + "-" + strings.ToLower(ep.Component),
			Namespace:       b.Namespace,
			Labels:          labels,
			OwnerReferences: []metav1.OwnerReference{monitoring.OwnedBy(b)},
		},
		Spec: podMonitorSpec{
			Selector:            metav1.LabelSelector{MatchLabels: maps.Clone(ep.PodSelector)},
			NamespaceSelector:   namespaceSelector{MatchNames: []string{b.Namespace}},
			PodMetricsEndpoints: []podMetricsEndpoint{},
		},
	}
}

// podMetricsEndpointFor renders one scrape endpoint.
func podMetricsEndpointFor(sources *corev1alpha1.MonitoringSources, ep corev1alpha1.MetricsEndpoint, params destinationParameters) (podMetricsEndpoint, error) {
	endpoint := podMetricsEndpoint{
		Path:        ep.Path,
		Scheme:      ep.Scheme,
		Params:      ep.Params,
		Interval:    params.Interval,
		Relabelings: relabelingsFor(sources.Identity, ep),
	}
	if endpoint.Path == "" {
		endpoint.Path = "/metrics"
	}
	if endpoint.Scheme == "" {
		endpoint.Scheme = "http"
	}
	switch {
	case ep.Port.Type == intstr.String && ep.Port.StrVal != "":
		endpoint.Port = ep.Port.StrVal
	case ep.Port.Type == intstr.Int && ep.Port.IntVal > 0:
		endpoint.PortNumber = ep.Port.IntVal
	default:
		return endpoint, errors.New("metrics endpoint has no port")
	}
	if ep.TLS != nil {
		endpoint.TLSConfig = &tlsConfig{ServerName: ep.TLS.ServerName, InsecureSkipVerify: ep.TLS.InsecureSkipVerify}
		if ep.TLS.CA != nil && ep.TLS.CA.SecretKeyRef != nil {
			ref := ep.TLS.CA.SecretKeyRef
			endpoint.TLSConfig.CA = &secretOrConfigMap{Secret: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: ref.Name},
				Key:                  ref.Key,
			}}
		}
	}
	if ep.Auth != nil {
		i := slices.IndexFunc(sources.Credentials, func(c corev1alpha1.MonitoringCredential) bool {
			return c.Profile == ep.Auth.CredentialProfile
		})
		if i < 0 {
			return endpoint, fmt.Errorf("credential profile %q is not published by the Instance", ep.Auth.CredentialProfile)
		}
		secret := corev1.LocalObjectReference{Name: sources.Credentials[i].SecretRef.Name}
		endpoint.BasicAuth = &basicAuth{
			Username: corev1.SecretKeySelector{LocalObjectReference: secret, Key: corev1alpha1.CredentialSecretKeyUsername},
			Password: corev1.SecretKeySelector{LocalObjectReference: secret, Key: corev1alpha1.CredentialSecretKeyPassword},
		}
	}
	return endpoint, nil
}

// relabelingsFor stamps the contract's identity labels, and no other key:
// an arbitrary target label such as __address__ would redirect the scrape.
// A multi-target probe (params.target) moves the probed target into the
// instance label so each target is its own series.
func relabelingsFor(identity corev1alpha1.MonitoringIdentity, ep corev1alpha1.MetricsEndpoint) []relabelConfig {
	labels := map[string]string{"openeverest_component": ep.Component, "openeverest_kind": ep.Kind}
	for _, k := range []string{"openeverest_instance", "openeverest_namespace", "openeverest_provider", "k8s_cluster_id"} {
		if v, ok := identity.Labels[k]; ok {
			labels[k] = v
		}
	}
	out := make([]relabelConfig, 0, len(labels)+1)
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		out = append(out, relabelConfig{TargetLabel: k, Replacement: new(labels[k]), Action: "replace"})
	}
	if _, ok := ep.Params["target"]; ok {
		out = append(out, relabelConfig{SourceLabels: []string{"__param_target"}, TargetLabel: "instance", Action: "replace"})
	}
	return out
}
