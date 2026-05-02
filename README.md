# ouroboros

Go reimplementation of [compumike/hairpin-proxy](https://github.com/compumike/hairpin-proxy) — fixes the hairpin-NAT problem for Kubernetes Ingress controllers configured with PROXY-protocol.

## Why

When an external load balancer in front of an ingress-controller (typically `ingress-nginx` with `use-proxy-protocol: true`) prepends the PROXY-protocol header, internal traffic from in-cluster pods bypasses the LB and reaches the ingress-controller without the header. The connection is then rejected. Common offenders: cert-manager HTTP-01 challenges, internal `https://` calls to your own public hostnames, healthchecks.

> **Do you need ouroboros at all?** Since [KEP-1860](https://github.com/kubernetes/enhancements/tree/master/keps/sig-network/1860-kube-proxy-IP-node-binding) (beta in Kubernetes 1.30, GA in 1.32) the kube-proxy can stop short-circuiting LoadBalancer IPs to the local Service when the cloud-controller-manager (CCM) sets `status.loadBalancer.ingress[].ipMode: Proxy`. With that flag set the LB always processes the connection — including its PROXY-protocol injection — and the hairpin path simply does not exist. A CNI that overrides kube-proxy must also honour the `ipMode` field — check your CNI's release notes for KEP-1860 support before relying on this fix. If your CCM and CNI both implement the contract you can remove ouroboros entirely; if only your CCM does, deploy on Kubernetes 1.30+ and check that the CNI agrees. ouroboros remains a workaround for the cluster topologies where that machinery is not available.

`ouroboros` fixes this with two cooperating components:

1. A **controller** that watches `Ingress` (and optionally Gateway-API `Gateway` + `HTTPRoute`) and rewrites the `kube-system/coredns` ConfigMap so internal lookups for those hostnames resolve to a small in-cluster proxy.
2. A **TCP proxy** that listens on 8080/8443, prepends the PROXY-protocol v1 header, and forwards to the real ingress-controller.

Both components ship as one binary, dispatched by subcommand.

## Architecture

```text
                        ┌────────────────────┐
                        │ Ingress / Gateway  │
                        │   (k8s API)        │
                        └────────┬───────────┘
                                 │ informers
                                 ▼
   ┌───────────────────────────────────────────┐
   │ ouroboros controller                      │
   │ - extracts hostnames                      │
   │ - reconciles CoreDNS ConfigMap (or hosts) │
   └───────────────────┬───────────────────────┘
                       │
                       ▼
       ┌───────────────────────────┐
       │ kube-system/coredns       │
       │  rewrite name foo.example │
       │    ouroboros-proxy....    │
       └───────────┬───────────────┘
                   │ DNS lookup from a pod
                   ▼
   ┌───────────────────────────────────────────┐
   │ ouroboros proxy   (Service ClusterIP)     │
   │ - accepts TCP                             │
   │ - prepends PROXY-protocol v1 header       │
   └───────────────────┬───────────────────────┘
                       │
                       ▼
              ingress-nginx-controller
```

## Install

```bash
helm install ouroboros oci://ghcr.io/lexfrei/charts/ouroboros \
  --version 0.3.0 \
  --namespace ouroboros --create-namespace
```

Override the upstream backend if you don't run ingress-nginx:

```bash
helm install ouroboros oci://ghcr.io/lexfrei/charts/ouroboros \
  --namespace ouroboros --create-namespace \
  --set proxy.target.host=my-ingress.my-ns.svc.cluster.local
```

Enable Gateway-API support:

```bash
--set controller.gatewayApi.enabled=true
```

## Modes

| Mode             | Reconciler                                                                                            | Use when                                                                                                                                |
| ---------------- | ----------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------- |
| `coredns`        | mutates `kube-system/coredns` ConfigMap directly                                                      | default — works for any pod that uses CoreDNS for DNS                                                                                   |
| `coredns-import` | writes plugin-only `rewrite name` snippets into a *separate* ConfigMap (default `kube-system/coredns-custom`) that the operator's CoreDNS chart pulls in via an `import /etc/coredns/custom/*.override` directive — see the prereqs in the [`coredns-import` mode](#coredns-import-mode) section | clusters where the main Corefile is owned by Helm/Flux and re-rendered (otherwise the inline block is overwritten on every reconcile) |
| `etc-hosts`      | writes `/etc/hosts` on each node (DaemonSet)                                                          | for kubelet, container runtime, or anything bypassing CoreDNS                                                                           |
| `external-dns`   | emits [`externaldns.k8s.io/v1alpha1.DNSEndpoint`](https://kubernetes-sigs.github.io/external-dns/) CRs (default) or annotated headless Services (`externalDns.output=service`, for instances that read `--source=service` with `--annotation-prefix`) | managed clusters that block writes to `kube-system/coredns` (EKS Auto, GKE Autopilot, AKS); clusters with node-local-dns (the per-node cache bypasses CoreDNS for non-`cluster.local` queries — see caveat below); split-horizon DNS; multi-cluster published DNS |

Switch via `--set controller.mode=external-dns` (or `--set etcHosts.enabled=true` for `etc-hosts`).

### `coredns-import` mode

`coredns` mode patches the live `kube-system/coredns` Corefile between `# === BEGIN ouroboros ===` markers. That works on bare-kubeadm clusters where ouroboros is the only writer, but on clusters where the Corefile is rendered from Helm/Kustomize/Flux values (cozystack, RKE2, k3s, EKS with `coredns-custom`, anyone running the [`coredns/helm-charts`](https://github.com/coredns/helm-charts) chart with custom `extraConfig`), the chart owner re-templates `data.Corefile` on every reconcile, the markers vanish, ouroboros re-injects them, and so on — a flap every reconcile cycle.

`coredns-import` side-steps the contention by writing plugin-only directives into a *separate* ConfigMap and never touching the main Corefile. CoreDNS picks the rewrites up via its [`import` plugin](https://coredns.io/plugins/import/).

This chart does **not** deploy CoreDNS. Two prerequisites must already be in place on the CoreDNS side before `coredns-import` works — set them up in whichever chart actually owns your CoreDNS:

1. **The import ConfigMap exists.** Default name `kube-system/coredns-custom` (matches the de-facto k8s convention used by RKE2/k3s and the CoreDNS Helm chart). Empty data is fine; ouroboros writes its own key. Override via `controller.corednsImport.{namespace,configmap}` if your CoreDNS uses a different name.
2. **The Corefile contains an `import` directive that picks up the configured key**, and the import ConfigMap is mounted into the CoreDNS Deployment at the matching path. Canonical wiring:

   ```text
   .:53 {
       errors
       health
       ready
       kubernetes cluster.local in-addr.arpa ip6.arpa { ... }
       prometheus :9153
       forward . /etc/resolv.conf { ... }
       cache 30
       loop
       reload
       loadbalance
       import /etc/coredns/custom/*.override
   }
   ```

   plus a volume mount for the `coredns-custom` ConfigMap at `/etc/coredns/custom/`. The `reload` plugin is required so CoreDNS notices ouroboros writes without a pod restart (same caveat as `coredns` mode).

Distros that ship this wiring out of the box: RKE2, k3s, the [`coredns/helm-charts`](https://github.com/coredns/helm-charts) chart with `extraConfig.import`. EKS, GKE Autopilot, AKS — does not work, the managed CoreDNS is read-only; use `external-dns` mode instead.

Without prereqs (1) and (2), ouroboros writes the override key successfully, CoreDNS silently ignores it, and hairpin keeps failing with no log surfacing — the failure mode this section exists to prevent.

```bash
helm install ouroboros oci://ghcr.io/lexfrei/charts/ouroboros \
  --namespace ouroboros --create-namespace \
  --set controller.mode=coredns-import
```

### `external-dns` mode

Set `controller.mode=external-dns` and ouroboros stops mutating CoreDNS. Instead it writes a `DNSEndpoint` per hostname per address family (one A record holding all IPv4 ClusterIPs of the proxy Service, one AAAA holding all IPv6 — dual-stack Services produce both) into the controller's namespace. An [external-dns](https://github.com/kubernetes-sigs/external-dns) deployment configured with `--source=crd` picks them up and publishes to whichever DNS provider it manages.

```bash
helm install ouroboros oci://ghcr.io/lexfrei/charts/ouroboros \
  --namespace ouroboros --create-namespace \
  --set controller.mode=external-dns
```

`externalDns.proxyService` (default: the chart-rendered proxy Service) is auto-resolved to a ClusterIP at startup. Use `externalDns.proxyIP` to override; in that case the controller does not need a `get` on Services. Add provider-specific annotations such as `external-dns.alpha.kubernetes.io/cloudflare-proxied: "false"` via `externalDns.annotations`.

`externalDns.cleanupOnUninstall` (default `true`) renders a Helm `post-delete` hook that runs `kubectl delete` for ouroboros-owned records after the chart is uninstalled — DNSEndpoint CRs in `output=crd`, headless Services in `output=service`. It runs `post-delete` (not `pre-delete`) on purpose: at `post-delete` time the controller Deployment is already gone, so it cannot race-recreate anything we delete. external-dns then sees the records vanish via watch and drops upstream DNS without waiting for its TXT-registry sweep.

> ⚠️  **Switching output between `crd` and `service` on an existing release leaves orphans.** The reconciler only manages its own active output kind, and the post-delete cleanup hook fires only on `helm uninstall` (not on `helm upgrade`). If you flip `externalDns.output`, run the explicit cleanup once for the previous kind:
>
> ```bash
> # Switching crd → service: clean up old DNSEndpoint CRs
> kubectl --namespace <release-ns> delete dnsendpoints.externaldns.k8s.io \
>   --selector='app.kubernetes.io/managed-by=ouroboros,ouroboros.lexfrei.tech/instance=<release-name>'
>
> # Switching service → crd: clean up old Services
> kubectl --namespace <release-ns> delete services \
>   --selector='app.kubernetes.io/managed-by=ouroboros,ouroboros.lexfrei.tech/instance=<release-name>'
> ```
>
> The controller probes the inactive output kind on startup and logs a Warn (with this command pre-rendered) when orphans are present. The probe is a best-effort safety net — it only succeeds when the controller's ServiceAccount has read access to the inactive kind. Chart-managed deployments deliberately minimise RBAC and grant verbs for the active kind only, so the probe silently 403s and the warning never fires; operators flipping output in a chart-managed release MUST still run the kubectl delete commands above. The probe surfaces orphans only in clusters where an operator has manually broadened the controller's RBAC (e.g. cluster-admin or a custom Role).

#### Empty-hosts mass-prune guard

If every Ingress / HTTPRoute disappears (or every hostname becomes a wildcard — controller filters wildcards before they reach the reconciler), the reconciler refuses to prune ouroboros-owned records and instead logs a Warn telling the operator to either uninstall the chart/manifests or restore at least one hostname. This protects against a single accidental commit silently wiping all published DNS — for example, an Ingress / HTTPRoute manifest rewrite that replaces every hostname with a wildcard, or a GitOps revert that drops every Ingress / HTTPRoute in one pass. The legitimate "remove ouroboros entirely" path stays through `helm uninstall`, which fires the post-delete cleanup hook — uninstall, not config drift, is the way to mass-delete.

### `external-dns` output: `crd` vs `service`

Two ways for ouroboros to talk to external-dns. Pick the one that matches what your external-dns instance is configured to read.

| `externalDns.output` | Object emitted | external-dns side requires | When to use |
| --- | --- | --- | --- |
| `crd` (default) | `externaldns.k8s.io/v1alpha1.DNSEndpoint` per hostname per address family | `--source=crd --crd-source-apiversion=externaldns.k8s.io/v1alpha1 --crd-source-kind=DNSEndpoint` | The cleanest path for instances that speak the CRD source. Tag with `externalDns.labels` for `--label-filter` separation. |
| `service` | One annotated headless Service per hostname (`<prefix>/hostname`, `<prefix>/target`, `<prefix>/ttl`) | `--source=service --annotation-prefix=<prefix>` | Existing external-dns instances that consume Services and use `--annotation-prefix` to scope themselves. Common in homelab split-horizon (one public, one internal). |

In `service` mode set `externalDns.annotationPrefix` to whatever your target external-dns instance uses (default is the upstream `external-dns.alpha.kubernetes.io/`):

```bash
helm install ouroboros oci://ghcr.io/lexfrei/charts/ouroboros \
  --namespace ouroboros --create-namespace \
  --set controller.mode=external-dns \
  --set externalDns.output=service \
  --set externalDns.annotationPrefix=internal-dns/
```

The receiving external-dns then needs `--annotation-prefix=internal-dns/` for its config to match. Other instances reading `external-dns.alpha.kubernetes.io/` ignore ouroboros's Services.

Service-mode keeps the chart-rendered Services lightweight (`spec.clusterIP: None`, no selector, no ports) — they exist purely as annotation carriers. Dual-stack targets are joined into a single `<prefix>/target: 10.0.0.1,fd00::1` entry, and external-dns produces both A and AAAA records from the one Service.

### Why not `DNSRecordSet`

`DNSRecordSet` is an evolving proposal in external-dns; not yet ratified, so adopting it would tie the chart to a moving target. `DNSEndpoint` is the documented stable CRD contract — every shipping provider supports it through `--source=crd`. The annotated-Service path uses upstream's stable Service source, so neither output ties ouroboros to an unstable interface.

**Coverage caveat (both modes).** Hostname extraction is asymmetric by design:

- **Ingress**: only `spec.tls[].hosts` is read; plain HTTP-only Ingresses are ignored. The hairpin-NAT problem manifests for TLS-terminated PROXY-protocol traffic.
- **Gateway-API**: `Gateway.spec.listeners[].hostname` and `HTTPRoute.spec.hostnames` are read **regardless of protocol**. Listeners are commonly paired (HTTP + HTTPS for redirect-to-TLS), and operators expect both to be hairpinned.

**etc-hosts caveat.** Each DaemonSet pod runs a full controller — Ingress/Gateway informers per node. On large clusters that is N replicated kube-apiserver watches producing identical results. Prefer `coredns` mode unless your nodes genuinely bypass cluster DNS.

**node-local-dns caveat.** Both `coredns` and `coredns-import` modes do NOT cover pods that resolve through [node-local-dns](https://kubernetes.io/docs/tasks/administer-cluster/nodelocaldns/). Pods on node-local-dns-equipped clusters query the per-node cache first; for non-`cluster.local` queries (which is exactly the hairpin case) node-local-dns forwards UPSTREAM and never sees the rewrite block ouroboros writes into CoreDNS (or the import ConfigMap CoreDNS pulls in). Hairpin silently fails for those pods. ouroboros logs a Warn at startup when it detects the `kube-system/node-local-dns` ConfigMap (in either mode). Override the lookup target via `OUROBOROS_NODE_LOCAL_DNS_NAMESPACE` / `OUROBOROS_NODE_LOCAL_DNS_CONFIGMAP` if your deployment uses a non-default location. Two reliable workarounds:

- Switch `controller.mode=external-dns` — DNSEndpoint records flow through whatever provider/CCM the cluster uses, independent of the node-local cache.
- Manually add the same `rewrite name` directives to the node-local-dns Corefile block(s) that handle external queries. ouroboros does not auto-mutate node-local-dns because its Corefile uses pillar templates and zone scopes that are gnarly to safely transform without per-cluster knowledge.

**Multi-ingress-controller caveat.** Clusters running two ingress controllers (one with PROXY-protocol, one without) need to scope ouroboros to the PROXY-protocol one — otherwise every hostname is rewritten and traffic for the other controller's hosts hits a 404. Set `controller.ingressClass=<class>` (and `controller.gatewayClass=<class>` for Gateway-API) to filter sources. Ingresses without an explicit `spec.ingressClassName` are dropped under the filter — they are ambiguous, and silently hairpinning them via the wrong controller is the failure mode this knob exists to prevent.

### RBAC matrix (operator-facing)

The chart suppresses the Role belonging to the *other* modes — operators running `external-dns` mode never see kube-system manifests, which is the frequent ask from managed-cluster users.

| Mode             | Cluster-scope reads                                                  | Namespaced writes                                                                                                          |
| ---------------- | -------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------- |
| `coredns`        | `networking.k8s.io/ingresses` (+ `gateway.networking.k8s.io` opt-in) | `kube-system`: `configmaps/coredns` `get,update,patch`                                                                     |
| `coredns-import` | same                                                                 | `controller.corednsImport.namespace` (default `kube-system`): `configmaps/<controller.corednsImport.configmap>` (default `coredns-custom`) `get,update,patch`. NOT granted access to the main `kube-system/coredns` ConfigMap. |
| `external-dns` (`output=crd`)     | same                                                                 | `externalDns.namespace` (default release-ns): `externaldns.k8s.io/dnsendpoints` full CRUD; release-ns: named-Service `get` for ClusterIP auto-discovery; release-ns post-delete hook: SA + Role[`dnsendpoints` `list,delete,deletecollection`] + RoleBinding for cleanup-on-uninstall |
| `external-dns` (`output=service`) | same                                                                 | `externalDns.namespace` (default release-ns): core `services` full CRUD (replaces `dnsendpoints`); release-ns: named-Service `get` for ClusterIP auto-discovery; release-ns post-delete hook: SA + Role[`services` `list,delete,deletecollection`] + RoleBinding for cleanup-on-uninstall |
| `etc-hosts`      | same                                                                 | *(no extra Role; node-local file write via DaemonSet hostPath)*                                                            |

**CoreDNS reload caveat.** `coredns` mode assumes CoreDNS' [`reload` plugin](https://coredns.io/plugins/reload/) is enabled (the default in kubeadm). If your Corefile lacks it, ouroboros logs a warning and the rewrite block is written but not picked up until CoreDNS pods are restarted manually. Verify with:

```bash
kubectl --namespace kube-system get configmap coredns --output jsonpath='{.data.Corefile}' | grep -w reload
```

## Verification

After install:

```bash
kubectl --namespace kube-system get configmap coredns --output jsonpath='{.data.Corefile}' | grep -A20 BEGIN.ouroboros
```

You should see a block like:

```text
# === BEGIN ouroboros (do not edit by hand) ===
rewrite name foo.example.com ouroboros-proxy.ouroboros.svc.cluster.local.
# === END ouroboros ===
```

From any pod:

```bash
getent hosts foo.example.com
# returns the ouroboros-proxy ClusterIP, not the LoadBalancer IP
```

A `curl https://foo.example.com/` from a pod will then see `X-Forwarded-For` populated correctly by the ingress-controller because the PROXY-protocol header reached it.

## Configuration

Both subcommands accept flags **and** env vars (flags override env, env overrides defaults). Run with `--help` for the full list — the table below is the source-of-truth alphabetical reference.

### `ouroboros controller`

| Flag                  | Env var                                | Default                                                  | Notes                                                          |
| --------------------- | -------------------------------------- | -------------------------------------------------------- | -------------------------------------------------------------- |
| `--mode`              | `OUROBOROS_CONTROLLER_MODE`            | `coredns`                                                | One of `coredns`, `coredns-import`, `etc-hosts`, `external-dns`. |
| `--kubeconfig`        | `OUROBOROS_CONTROLLER_KUBECONFIG`      | *(empty = in-cluster)*                                   | Path to a kubeconfig file.                                     |
| `--gateway-api`       | `OUROBOROS_CONTROLLER_GATEWAY_API`     | `false`                                                  | Watch Gateway/HTTPRoute in addition to Ingress.                |
| `--resync`            | `OUROBOROS_CONTROLLER_RESYNC`          | `10m`                                                    | Informer resync period.                                        |
| `--coredns-namespace` | `OUROBOROS_CONTROLLER_COREDNS_NAMESPACE` | `kube-system`                                          | CoreDNS ConfigMap namespace.                                   |
| `--coredns-configmap` | `OUROBOROS_CONTROLLER_COREDNS_CONFIGMAP` | `coredns`                                              | CoreDNS ConfigMap name.                                        |
| `--coredns-key`       | `OUROBOROS_CONTROLLER_COREDNS_KEY`     | `Corefile`                                               | Data key holding the Corefile.                                 |
| `--coredns-import-namespace` | `OUROBOROS_CONTROLLER_COREDNS_IMPORT_NAMESPACE` | `kube-system`                              | Namespace of the separate import ConfigMap (`coredns-import` mode). |
| `--coredns-import-configmap` | `OUROBOROS_CONTROLLER_COREDNS_IMPORT_CONFIGMAP` | `coredns-custom`                            | Name of the import ConfigMap (`coredns-import` mode).          |
| `--coredns-import-key` | `OUROBOROS_CONTROLLER_COREDNS_IMPORT_KEY`     | `ouroboros.override`                                  | Data key inside the import ConfigMap holding the rewrite snippet. |
| `--proxy-fqdn`        | `OUROBOROS_CONTROLLER_PROXY_FQDN`      | `ouroboros-proxy.ouroboros.svc.cluster.local.`           | Required for `coredns` and `coredns-import` modes. **Must end with a trailing dot.** |
| `--etc-hosts`         | `OUROBOROS_CONTROLLER_ETC_HOSTS`       | `/host/etc/hosts`                                        | Path to host-mounted hosts file (`etc-hosts` mode).            |
| `--proxy-ip`          | `OUROBOROS_CONTROLLER_PROXY_IP`        | *(empty)*                                                | Required for `etc-hosts` mode.                                 |
| `--external-dns-namespace`     | `OUROBOROS_CONTROLLER_EXTERNAL_DNS_NAMESPACE`     | *(release namespace)* | Where DNSEndpoint CRs are written (`external-dns` mode). Validated as RFC 1123 label only when explicitly set; the release-namespace fallback is already valid by definition. |
| `--external-dns-record-ttl`    | `OUROBOROS_CONTROLLER_EXTERNAL_DNS_RECORD_TTL`    | `60`                  | Record TTL on each emitted DNSEndpoint, [1, 86400] seconds.    |
| `--external-dns-proxy-ip`      | `OUROBOROS_CONTROLLER_EXTERNAL_DNS_PROXY_IP`      | *(empty)*             | Override target IP. Empty = discover via the named Service.    |
| `--external-dns-proxy-service` | `OUROBOROS_CONTROLLER_EXTERNAL_DNS_PROXY_SERVICE` | `ouroboros-proxy`     | Service name resolved at startup to ClusterIP.                 |
| `--external-dns-annotation`    | *(no env mapping; chart only)*                    | *(none)*              | Repeatable `key=value` annotations copied onto every emitted record (DNSEndpoint in `output=crd`, headless Service in `output=service`). Reserved keys are rejected at runtime. |
| `--external-dns-label`         | *(no env mapping; chart only)*                    | *(none)*              | Repeatable `key=value` labels copied onto every emitted record (DNSEndpoint in `output=crd`, headless Service in `output=service`). Use case: multi-instance external-dns with `--label-filter` (e.g. dedicated internal-DNS instance) — the filter is applied to whichever kind ouroboros emits. Reserved keys (`app.kubernetes.io/managed-by`, `ouroboros.lexfrei.tech/instance`) are rejected. |
| `--external-dns-output`        | `OUROBOROS_CONTROLLER_EXTERNAL_DNS_OUTPUT`        | `crd`                 | One of `crd` (DNSEndpoint CRs) or `service` (annotated headless Services). |
| `--external-dns-annotation-prefix` | `OUROBOROS_CONTROLLER_EXTERNAL_DNS_ANNOTATION_PREFIX` | `external-dns.alpha.kubernetes.io/` | Annotation prefix for `output=service`. Must end with `/`. Override to match your target external-dns instance's `--annotation-prefix`. |
| `--ingress-class`              | `OUROBOROS_CONTROLLER_INGRESS_CLASS`              | *(empty)*             | Filter Ingresses by `spec.ingressClassName`. Empty = all. Ingresses without an explicit class are dropped under the filter. |
| `--gateway-class`              | `OUROBOROS_CONTROLLER_GATEWAY_CLASS`              | *(empty)*             | Filter Gateways by `spec.gatewayClassName` (and HTTPRoutes attached to surviving Gateways). Empty = all. |

### `ouroboros proxy`

| Flag                  | Env var                              | Default                                              |
| --------------------- | ------------------------------------ | ---------------------------------------------------- |
| `--listen-http`       | `OUROBOROS_PROXY_LISTEN_HTTP`        | `:8080`                                              |
| `--listen-https`      | `OUROBOROS_PROXY_LISTEN_HTTPS`       | `:8443`                                              |
| `--listen-health`     | `OUROBOROS_PROXY_LISTEN_HEALTH`      | `:8081`                                              |
| `--target-host`       | `OUROBOROS_PROXY_TARGET_HOST`        | `ingress-nginx-controller.ingress-nginx.svc.cluster.local` |
| `--target-http-port`  | `OUROBOROS_PROXY_TARGET_HTTP_PORT`   | `80`                                                 |
| `--target-https-port` | `OUROBOROS_PROXY_TARGET_HTTPS_PORT`  | `443`                                                |
| `--dial-timeout`      | `OUROBOROS_PROXY_DIAL_TIMEOUT`       | `5s`                                                 |
| `--ready-timeout`     | `OUROBOROS_PROXY_READY_TIMEOUT`      | `2s`                                                 |
| `--shutdown-grace`    | `OUROBOROS_PROXY_SHUTDOWN_GRACE`     | `30s`                                                |

## Build

```bash
go build ./cmd/ouroboros
docker build --file Containerfile --tag ouroboros:dev .
```

## Develop

```bash
go test -race -count=1 ./...
golangci-lint run ./...
helm lint   charts/ouroboros
helm unittest charts/ouroboros
```

## License

BSD 3-Clause — see [LICENSE](./LICENSE).
