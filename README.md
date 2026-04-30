# ouroboros

Go reimplementation of [compumike/hairpin-proxy](https://github.com/compumike/hairpin-proxy) — fixes the hairpin-NAT problem for Kubernetes Ingress controllers configured with PROXY-protocol.

## Why

When an external load balancer in front of an ingress-controller (typically `ingress-nginx` with `use-proxy-protocol: true`) prepends the PROXY-protocol header, internal traffic from in-cluster pods bypasses the LB and reaches the ingress-controller without the header. The connection is then rejected. Common offenders: cert-manager HTTP-01 challenges, internal `https://` calls to your own public hostnames, healthchecks.

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
  --version 0.1.0 \
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

| Mode           | Reconciler                                                                                            | Use when                                                                                                                                |
| -------------- | ----------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------- |
| `coredns`      | mutates `kube-system/coredns` ConfigMap                                                               | default — works for any pod that uses CoreDNS for DNS                                                                                   |
| `etc-hosts`    | writes `/etc/hosts` on each node (DaemonSet)                                                          | for kubelet, container runtime, or anything bypassing CoreDNS                                                                           |
| `external-dns` | emits [`externaldns.k8s.io/v1alpha1.DNSEndpoint`](https://kubernetes-sigs.github.io/external-dns/) CRs | managed clusters that block writes to `kube-system/coredns` (EKS Auto, GKE Autopilot, AKS), split-horizon DNS, multi-cluster published DNS |

Switch via `--set controller.mode=external-dns` (or `--set etcHosts.enabled=true` for `etc-hosts`).

### `external-dns` mode

Set `controller.mode=external-dns` and ouroboros stops mutating CoreDNS. Instead it writes one `DNSEndpoint` per hostname (one A record per IPv4 ClusterIP, one AAAA per IPv6 — dual-stack Services produce both) into the controller's namespace. An [external-dns](https://github.com/kubernetes-sigs/external-dns) deployment configured with `--source=crd` picks them up and publishes to whichever DNS provider it manages.

```bash
helm install ouroboros oci://ghcr.io/lexfrei/charts/ouroboros \
  --namespace ouroboros --create-namespace \
  --set controller.mode=external-dns
```

`externalDns.proxyService` (default: the chart-rendered proxy Service) is auto-resolved to a ClusterIP at startup. Use `externalDns.proxyIP` to override; in that case the controller does not need a `get` on Services. Add provider-specific annotations such as `external-dns.alpha.kubernetes.io/cloudflare-proxied: "false"` via `externalDns.annotations`.

`externalDns.cleanupOnUninstall` (default `true`) renders a Helm `pre-delete` hook that runs `kubectl delete dnsendpoints` filtered by ouroboros's ownership labels before the chart is uninstalled, so external-dns sees the records vanish via watch and drops upstream DNS without waiting for its TXT-registry sweep.

### Why DNSEndpoint and not annotated Services / DNSRecordSet

Two alternatives surface during design discussions:

- **Annotated headless Services** (the `lexfrei/kuberture` pattern): produces a Service object per hostname, which pollutes the Service catalog and races with kube-proxy.
- **`DNSRecordSet`**: an evolving proposal in external-dns; not yet ratified, so adopting it would tie the chart to a moving target.

`DNSEndpoint` is external-dns's documented stable contract — every shipping provider (Cloudflare, Route53, AzureDNS, GCloud, etc.) supports it through the CRD source.

**Coverage caveat (both modes).** Hostname extraction is asymmetric by design:

- **Ingress**: only `spec.tls[].hosts` is read; plain HTTP-only Ingresses are ignored. The hairpin-NAT problem manifests for TLS-terminated PROXY-protocol traffic.
- **Gateway-API**: `Gateway.spec.listeners[].hostname` and `HTTPRoute.spec.hostnames` are read **regardless of protocol**. Listeners are commonly paired (HTTP + HTTPS for redirect-to-TLS), and operators expect both to be hairpinned.

**etc-hosts caveat.** Each DaemonSet pod runs a full controller — Ingress/Gateway informers per node. On large clusters that is N replicated kube-apiserver watches producing identical results. Prefer `coredns` mode unless your nodes genuinely bypass cluster DNS.

### RBAC matrix (operator-facing)

The chart suppresses the Role belonging to the *other* modes — operators running `external-dns` mode never see kube-system manifests, which is the frequent ask from managed-cluster users.

| Mode           | Cluster-scope reads                                                  | Namespaced writes                                                                                                          |
| -------------- | -------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------- |
| `coredns`      | `networking.k8s.io/ingresses` (+ `gateway.networking.k8s.io` opt-in) | `kube-system`: `configmaps/coredns` `get,update,patch`                                                                     |
| `external-dns` | same                                                                 | release-ns (or `externalDns.namespace`): `externaldns.k8s.io/dnsendpoints` full CRUD + named-Service `get` for auto-discovery |
| `etc-hosts`    | same                                                                 | *(no extra Role; node-local file write via DaemonSet hostPath)*                                                            |

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
| `--mode`              | `OUROBOROS_CONTROLLER_MODE`            | `coredns`                                                | One of `coredns`, `etc-hosts`, `external-dns`.                 |
| `--kubeconfig`        | `OUROBOROS_CONTROLLER_KUBECONFIG`      | *(empty = in-cluster)*                                   | Path to a kubeconfig file.                                     |
| `--gateway-api`       | `OUROBOROS_CONTROLLER_GATEWAY_API`     | `false`                                                  | Watch Gateway/HTTPRoute in addition to Ingress.                |
| `--resync`            | `OUROBOROS_CONTROLLER_RESYNC`          | `10m`                                                    | Informer resync period.                                        |
| `--coredns-namespace` | `OUROBOROS_CONTROLLER_COREDNS_NAMESPACE` | `kube-system`                                          | CoreDNS ConfigMap namespace.                                   |
| `--coredns-configmap` | `OUROBOROS_CONTROLLER_COREDNS_CONFIGMAP` | `coredns`                                              | CoreDNS ConfigMap name.                                        |
| `--coredns-key`       | `OUROBOROS_CONTROLLER_COREDNS_KEY`     | `Corefile`                                               | Data key holding the Corefile.                                 |
| `--proxy-fqdn`        | `OUROBOROS_CONTROLLER_PROXY_FQDN`      | `ouroboros-proxy.ouroboros.svc.cluster.local.`           | Required for `coredns` mode. **Must end with a trailing dot.** |
| `--etc-hosts`         | `OUROBOROS_CONTROLLER_ETC_HOSTS`       | `/host/etc/hosts`                                        | Path to host-mounted hosts file (`etc-hosts` mode).            |
| `--proxy-ip`          | `OUROBOROS_CONTROLLER_PROXY_IP`        | *(empty)*                                                | Required for `etc-hosts` mode.                                 |
| `--external-dns-namespace`     | `OUROBOROS_CONTROLLER_EXTERNAL_DNS_NAMESPACE`     | *(release namespace)* | Where DNSEndpoint CRs are written (`external-dns` mode). RFC 1123 label. |
| `--external-dns-record-ttl`    | `OUROBOROS_CONTROLLER_EXTERNAL_DNS_RECORD_TTL`    | `60`                  | Record TTL on each emitted DNSEndpoint, [1, 86400] seconds.    |
| `--external-dns-proxy-ip`      | `OUROBOROS_CONTROLLER_EXTERNAL_DNS_PROXY_IP`      | *(empty)*             | Override target IP. Empty = discover via the named Service.    |
| `--external-dns-proxy-service` | `OUROBOROS_CONTROLLER_EXTERNAL_DNS_PROXY_SERVICE` | `ouroboros-proxy`     | Service name resolved at startup to ClusterIP.                 |
| `--external-dns-annotation`    | *(no env mapping; chart only)*                    | *(none)*              | Repeatable `key=value` annotations copied onto every emitted DNSEndpoint. Reserved keys are rejected at runtime. |

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
