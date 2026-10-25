# monitoring-prometheus

PoC of an **ExtensionManaged** `MonitoringClass` controller for spec 014 (extensible
monitoring). It claims the `prometheus` class and, for every Accepted
`MonitoringBinding` of that class, creates one Prometheus Operator
`PodMonitor` per endpoint published in `Instance.status.monitoring.sources.metrics`.

It never writes Instances: it owns only the bindings' `Configured` condition
and the destinations' `Ready` condition. PodMonitors are owned by the binding, so
removing the destination from the Instance garbage-collects them.

```sh
# Installs the controller and claims the `prometheus` MonitoringClass. Like a
# provider, it is its own chart, so it can ship as a hub extension or as a
# conditional subchart of the OpenEverest chart.
helm install monitoring-prometheus charts/monitoring-prometheus -n openeverest-system
```

For development against the current kubeconfig: `make build && IMG=... make docker-build`, or run the controller out of cluster with `kubectl apply -f <(helm template x charts/monitoring-prometheus -s templates/monitoringclass.yaml)` and `go run ./cmd/controller`.

`MonitoringDestination.spec.parameters`:

```yaml
podMonitorLabels: {release: kube-prometheus-stack}   # how your Prometheus selects PodMonitors
interval: 30s
```
