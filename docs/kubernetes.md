# Running on Kubernetes

This page covers five topics for running plex-exporter on Kubernetes. They are a writable `/tmp`, health probes, a sidecar setup, the labels the Prometheus Operator adds and dashboard delivery. It is for readers who deploy it with manifests, Helm, Flux or Terraform.

## A writable /tmp

The exporter writes its readiness marker to `/tmp/.healthy`. With `securityContext.readOnlyRootFilesystem: true`, mount a small in-memory volume there:

```yaml
    volumes:
      - name: tmp
        emptyDir:
          medium: Memory
          sizeLimit: 8Mi
    # in the container spec
    volumeMounts:
      - name: tmp
        mountPath: /tmp
```

Without it, `/api/health` answers 503 for as long as the exporter runs, so an HTTP probe on that endpoint fails. [Security](security.md#read-only-root-filesystem) explains why.

## Probes

These probes are a starting point. Both read `/api/health`, so with `readOnlyRootFilesystem: true` they need the `/tmp` mount above.

```yaml
    livenessProbe:
      httpGet:
        path: /api/health
        port: 9594
      initialDelaySeconds: 0
      periodSeconds: 30
      timeoutSeconds: 5
      failureThreshold: 3
    readinessProbe:
      httpGet:
        path: /api/health
        port: 9594
      initialDelaySeconds: 0
      periodSeconds: 30
      timeoutSeconds: 5
      failureThreshold: 3
```

## Running as a sidecar

You can run the exporter as a sidecar in the Plex pod, with `PLEX_URL` set to `http://localhost:32400`. It needs the `/tmp` mount only when an HTTP probe on `/api/health` is configured. With no probe, it logs one warning at start and runs.

## Labels the Prometheus Operator adds

The Prometheus Operator adds `pod`, `endpoint` and `container` labels to every scraped series, and the `pod` value changes on each restart. A panel that reads one gauge over a time range therefore shows one entry per old pod until its series ages out.

The shipped dashboard avoids this, because its current-state tiles ask for an instant value rather than a range. To drop those labels at ingestion instead, add this to the ServiceMonitor:

```yaml
    metricRelabelings:
      - action: labeldrop
        regex: (pod|endpoint|container)
```

Do this only for a single-replica deployment. Those labels are what tell one replica's series from another's. Dropping all three on a multi-replica ServiceMonitor gives two replicas identical label sets, which Prometheus [warns against](https://prometheus.io/docs/prometheus/latest/configuration/configuration/#relabel_config). With more than one replica, keep `pod` and aggregate in the query.

## Delivering the dashboard

[Monitoring and alerts](monitoring.md#dashboard) lists the release download and the OCI artifact. This grafana-operator resource pulls the OCI artifact. Replace `<tag>` with the tag of the image you run.

```yaml
apiVersion: grafana.integreatly.org/v1beta1
kind: GrafanaDashboard
metadata:
  name: plex-exporter
spec:
  instanceSelector:
    matchLabels:
      dashboards: grafana
  oci:
    reference: ghcr.io/cplieger/plex-exporter/dashboard:<tag>
    path: grafana-dashboard.json
```

If you deliver the dashboard through a Flux Kustomization with `postBuild.substitute`, escape its `${datasource}`. That is Grafana's data source variable, and Flux rewrites every `${...}`. Write it as `$${datasource}`, or set `kustomize.toolkit.fluxcd.io/substitute: disabled` on the ConfigMap.
