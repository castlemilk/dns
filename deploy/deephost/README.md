# Deephost Prometheus and Grafana integration

The VKE Prometheus is standalone and does not consume `ServiceMonitor` or
`PrometheusRule` resources. Merge the two jobs from
`prometheus-scrape-configs.yaml` into its `scrape_configs`, mount
`deephost-rules.yaml`, and add that mounted path to `rule_files`.

For the committed `castlemilk/deephost` chart, the ready-to-apply
`deephost-observability.patch` performs that integration, enables it
in VKE and Paprika values, and provisions the dashboard into the existing
Deephost Grafana. It does not install another Grafana. The dashboard is added
to the existing `deephost-grafana-dashboards` ConfigMap, appears in the
read-only `DeepHost` folder, and uses the existing `deephost-prometheus`
datasource UID.

The patch was generated and Helm/promtool-validated against the committed
Deephost `HEAD`, never against the checkout's unrelated uncommitted changes.
Apply it from that repository's root with `git apply --check` followed by
`git apply` after reviewing overlap with any newer monitoring changes.

Both jobs use DNS A-record discovery against the headless
`dns-otel-metrics.dns.svc.cluster.local` Service. This is intentional: the two
collector replicas keep independent in-memory exporter state, so scraping the
load-balanced `dns-otel-collector` ClusterIP would produce incomplete data.
Port 8889 exposes application and bounded span-derived metrics; port 8888
exposes collector health and loss counters.

The NetworkPolicy permits these ports only from namespace `deephost` pods with
label `app=deephost-prometheus`. Grafana does not need direct collector access;
it queries that Prometheus datasource. Keep the selectors aligned if the
Prometheus workload identity changes. The included rules evaluate in
Prometheus and their state is visible in the dashboard, but the cluster
currently has no Alertmanager, so they will not page until notification
delivery is installed.

After Paprika reconciles the patched Deephost chart, open the existing Grafana
and select **DeepHost / DeepHost Operations**. Confirm both DeepHost scrape
jobs, two collector replicas, and three authority replicas before treating the
dashboard as healthy.
