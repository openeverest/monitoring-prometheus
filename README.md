# monitoring-prometheus

PoC of an **ExtensionManaged** `MonitoringClass` controller for spec 014 (extensible
monitoring). It claims the `prometheus` class and, for every Accepted
`MonitoringBinding` of that class, creates one Prometheus Operator
`PodMonitor` per endpoint published in `Instance.status.monitoring.sources.metrics`.

It never writes Instances: it owns only the bindings' `Configured` condition
and the destinations' `Ready` condition. PodMonitors are owned by the binding, so
removing the destination from the Instance garbage-collects them.

```sh
kubectl apply -f deploy/class.yaml
go run ./cmd/controller            # uses the current kubeconfig
```

`MonitoringDestination.spec.parameters`:

```yaml
podMonitorLabels: {release: kube-prometheus-stack}   # how your Prometheus selects PodMonitors
interval: 30s
```
