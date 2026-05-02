#!/usr/bin/env bash
# End-to-end harness: kind cluster + Gateway-API CRDs + Traefik with
# PROXY-protocol on its web/websecure entrypoints AND Gateway-API enabled +
# ouroboros chart with controller.gatewayApi.enabled=true. Verifies that
#   1. ouroboros writes a single CoreDNS BEGIN ouroboros block containing
#      BOTH the Ingress host and the Gateway/HTTPRoute host within a
#      deadline,
#   2. in-cluster DNS for both hostnames resolves to the proxy ClusterIP,
#   3. an in-cluster TLS curl to both hostnames succeeds (which would fail
#      without ouroboros because Traefik with proxyProtocol enabled drops
#      connections that arrive without the PROXY-protocol header).

set -euo pipefail

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
readonly CLUSTER_NAME="${CLUSTER_NAME:-ouroboros-e2e}"
readonly CTX="kind-${CLUSTER_NAME}"
readonly IMAGE="ouroboros:e2e"
readonly INGRESS_HOST="hairpin-ingress.example.invalid"
readonly GATEWAY_HOST="hairpin-gateway.example.invalid"
readonly DEADLINE_SECONDS=180
# MODE selects which reconciler to verify:
#   coredns        — default; CoreDNS Corefile mutation + in-cluster TLS curl
#   coredns-import — wires a separate kube-system/coredns-custom ConfigMap
#                    into the CoreDNS Deployment (mount + import directive),
#                    runs ouroboros in coredns-import mode, asserts the
#                    override key is populated and that in-cluster DNS for
#                    both hosts resolves to the proxy ClusterIP.
#   external-dns   — install external-dns chart with --source=crd
#                    --provider=inmemory; assert DNSEndpoint CRs are
#                    emitted for both hostnames; helm uninstall must
#                    trigger the post-delete cleanup hook and remove the
#                    records.
readonly MODE="${MODE:-coredns}"
readonly COREDNS_CUSTOM_CM="coredns-custom"
readonly COREDNS_CUSTOM_KEY="ouroboros.override"

log() { printf '\033[1;36m==>\033[0m %s\n' "$*" >&2; }
fail() {
  printf '\033[1;31m!!!\033[0m %s\n' "$*" >&2
  exit 1
}

cleanup() {
  if [[ -n "${KEEP_CLUSTER:-}" || -n "${KIND_CLUSTER_REUSE:-}" ]]; then
    log "leaving kind cluster ${CLUSTER_NAME} alive (KEEP_CLUSTER or KIND_CLUSTER_REUSE set)"
    return
  fi
  log "deleting kind cluster ${CLUSTER_NAME}"
  kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

require() {
  command -v "$1" >/dev/null 2>&1 || fail "required tool not on PATH: $1"
}

require kind
require kubectl
require helm
require docker
require openssl

if kind get clusters 2>/dev/null | grep --quiet --line-regexp "${CLUSTER_NAME}"; then
  log "kind cluster ${CLUSTER_NAME} already exists, reusing"
else
  log "creating kind cluster"
  kind create cluster --name "${CLUSTER_NAME}" --config "${SCRIPT_DIR}/kind-config.yaml" --wait 120s
fi

log "building ouroboros image"
# Note: Gateway-API CRDs are intentionally NOT pre-installed here. The
# Traefik chart bundles its own copy and tries to claim ownership via
# server-side apply. Pre-installing them with kubectl-apply (any apply
# strategy) leaves a kubectl-typed manager that the chart's installer
# refuses to overwrite. Letting Traefik install them avoids the conflict.
docker build --file "${REPO_ROOT}/Containerfile" --tag "${IMAGE}" \
  --build-arg VERSION=e2e --build-arg REVISION="$(cd "${REPO_ROOT}" && git rev-parse --short HEAD)" \
  "${REPO_ROOT}"

log "loading ouroboros image into kind"
kind load docker-image "${IMAGE}" --name "${CLUSTER_NAME}"

log "installing Traefik with PROXY-protocol + Gateway-API enabled"
helm --kube-context "${CTX}" repo add traefik https://traefik.github.io/charts >/dev/null 2>&1 || true
# Update only the traefik repo so a stale sibling repo on the host (CI runner
# or developer machine) cannot fail the whole step.
helm --kube-context "${CTX}" repo update traefik >/dev/null
helm --kube-context "${CTX}" upgrade --install traefik traefik/traefik \
  --namespace traefik --create-namespace \
  --set "providers.kubernetesGateway.enabled=true" \
  --set "providers.kubernetesIngress.enabled=true" \
  --set "ports.web.proxyProtocol.trustedIPs[0]=0.0.0.0/0" \
  --set "ports.web.proxyProtocol.insecure=true" \
  --set "ports.websecure.proxyProtocol.trustedIPs[0]=0.0.0.0/0" \
  --set "ports.websecure.proxyProtocol.insecure=true" \
  --set "service.type=ClusterIP" \
  --wait --timeout 5m

log "minting self-signed TLS certs"
TMPDIR="$(mktemp -d)"
for host in "${INGRESS_HOST}" "${GATEWAY_HOST}"; do
  openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
    -subj "/CN=${host}" \
    -addext "subjectAltName=DNS:${host}" \
    -keyout "${TMPDIR}/${host}.key" \
    -out "${TMPDIR}/${host}.crt" >/dev/null 2>&1
done

log "applying test workloads"
kubectl --context "${CTX}" apply --filename "${SCRIPT_DIR}/test-workloads.yaml" >/dev/null
kubectl --context "${CTX}" --namespace hairpin-test create secret tls ingress-tls \
  --cert="${TMPDIR}/${INGRESS_HOST}.crt" --key="${TMPDIR}/${INGRESS_HOST}.key" \
  --dry-run=client --output=yaml | kubectl --context "${CTX}" apply --filename - >/dev/null
kubectl --context "${CTX}" --namespace hairpin-test create secret tls gateway-tls \
  --cert="${TMPDIR}/${GATEWAY_HOST}.crt" --key="${TMPDIR}/${GATEWAY_HOST}.key" \
  --dry-run=client --output=yaml | kubectl --context "${CTX}" apply --filename - >/dev/null
kubectl --context "${CTX}" --namespace hairpin-test rollout status deployment/echo --timeout=2m

readonly OUTPUT="${OUTPUT:-crd}"

if [[ "${MODE}" == "coredns-import" ]]; then
  log "creating empty kube-system/${COREDNS_CUSTOM_CM} ConfigMap (chart precondition)"
  kubectl --context "${CTX}" --namespace kube-system create configmap "${COREDNS_CUSTOM_CM}" \
    --dry-run=client --output yaml | kubectl --context "${CTX}" apply --filename - >/dev/null

  log "patching CoreDNS Corefile to import /etc/coredns/custom/*.override"
  # kind ships CoreDNS with a single .:53 block; insert the import directive
  # right after 'reload' so the override snippets are picked up. awk does
  # the in-place edit unambiguously — sed escaping for the embedded
  # brace-and-newline content of Corefile is brittle.
  cur_corefile="$(kubectl --context "${CTX}" --namespace kube-system \
    get configmap coredns --output jsonpath='{.data.Corefile}')"
  if grep --quiet 'import /etc/coredns/custom' <<<"${cur_corefile}"; then
    log "  Corefile already contains the import directive; skipping insert"
  else
    new_corefile="$(awk '
      /^[[:space:]]*reload[[:space:]]*$/ {
        print
        match($0, /^[[:space:]]*/)
        printf "%simport /etc/coredns/custom/*.override\n", substr($0, RSTART, RLENGTH)
        next
      }
      { print }
    ' <<<"${cur_corefile}")"
    kubectl --context "${CTX}" --namespace kube-system create configmap coredns \
      --from-literal=Corefile="${new_corefile}" \
      --dry-run=client --output yaml | kubectl --context "${CTX}" apply --filename - >/dev/null
  fi

  log "mounting ${COREDNS_CUSTOM_CM} ConfigMap into the CoreDNS Deployment"
  if ! kubectl --context "${CTX}" --namespace kube-system get deployment coredns \
       --output jsonpath='{.spec.template.spec.volumes[*].name}' \
       | tr ' ' '\n' | grep --quiet --line-regexp "${COREDNS_CUSTOM_CM}"; then
    kubectl --context "${CTX}" --namespace kube-system patch deployment coredns --type=json \
      --patch="[
        {\"op\":\"add\",\"path\":\"/spec/template/spec/volumes/-\",\"value\":{\"name\":\"${COREDNS_CUSTOM_CM}\",\"configMap\":{\"name\":\"${COREDNS_CUSTOM_CM}\",\"optional\":true}}},
        {\"op\":\"add\",\"path\":\"/spec/template/spec/containers/0/volumeMounts/-\",\"value\":{\"name\":\"${COREDNS_CUSTOM_CM}\",\"mountPath\":\"/etc/coredns/custom\",\"readOnly\":true}}
      ]" >/dev/null
  else
    log "  CoreDNS Deployment already mounts ${COREDNS_CUSTOM_CM}; skipping patch"
  fi

  kubectl --context "${CTX}" --namespace kube-system rollout status deployment/coredns --timeout=2m
fi

if [[ "${MODE}" == "external-dns" ]]; then
  helm --kube-context "${CTX}" repo add external-dns https://kubernetes-sigs.github.io/external-dns/ >/dev/null 2>&1 || true
  helm --kube-context "${CTX}" repo update external-dns >/dev/null

  if [[ "${OUTPUT}" == "service" ]]; then
    log "installing external-dns with --source=service for service-output e2e"
    helm --kube-context "${CTX}" upgrade --install external-dns external-dns/external-dns \
      --namespace external-dns --create-namespace \
      --set "provider.name=inmemory" \
      --set "sources={service}" \
      --set "registry=noop" \
      --set "policy=sync" \
      --set "interval=10s" \
      --set "extraArgs={--namespace=ouroboros}" \
      --wait --timeout 5m
  else
    log "installing upstream DNSEndpoint CRD"
    kubectl --context "${CTX}" apply --filename \
      https://raw.githubusercontent.com/kubernetes-sigs/external-dns/v0.20.0/config/crd/standard/dnsendpoints.externaldns.k8s.io.yaml >/dev/null

    log "installing external-dns with the in-memory provider and CRD source"
    helm --kube-context "${CTX}" upgrade --install external-dns external-dns/external-dns \
      --namespace external-dns --create-namespace \
      --set "provider.name=inmemory" \
      --set "sources={crd}" \
      --set "registry=noop" \
      --set "policy=sync" \
      --set "interval=10s" \
      --set "extraArgs={--crd-source-apiversion=externaldns.k8s.io/v1alpha1,--crd-source-kind=DNSEndpoint,--namespace=ouroboros}" \
      --wait --timeout 5m
  fi
fi

log "installing ouroboros from local chart in mode=${MODE}"
ouroboros_extra_set=()
if [[ "${MODE}" == "external-dns" ]]; then
  ouroboros_extra_set+=( --set "controller.mode=external-dns" )

  if [[ "${OUTPUT}" == "service" ]]; then
    ouroboros_extra_set+=( --set "externalDns.output=service" )
    ouroboros_extra_set+=( --set "externalDns.annotationPrefix=external-dns.alpha.kubernetes.io/" )
  fi
fi

if [[ "${MODE}" == "coredns-import" ]]; then
  ouroboros_extra_set+=( --set "controller.mode=coredns-import" )
fi

helm --kube-context "${CTX}" upgrade --install ouroboros "${REPO_ROOT}/charts/ouroboros" \
  --namespace ouroboros --create-namespace \
  --set "image.repository=ouroboros" \
  --set "image.tag=e2e" \
  --set "image.pullPolicy=Never" \
  --set "controller.gatewayApi.enabled=true" \
  --set "proxy.target.host=traefik.traefik.svc.cluster.local" \
  "${ouroboros_extra_set[@]}" \
  --wait --timeout 3m

log "waiting for the proxy Service to receive a ClusterIP"
PROXY_IP=""
for _ in $(seq 1 30); do
  PROXY_IP="$(kubectl --context "${CTX}" --namespace ouroboros get svc ouroboros-proxy --output jsonpath='{.spec.clusterIP}' 2>/dev/null || true)"
  [[ -n "${PROXY_IP}" ]] && break
  sleep 2
done
[[ -n "${PROXY_IP}" ]] || fail "ouroboros-proxy Service has no ClusterIP after 60s"
log "proxy ClusterIP: ${PROXY_IP}"

if [[ "${MODE}" == "external-dns" ]]; then
  if [[ "${OUTPUT}" == "service" ]]; then
    emission_kind="services"
    name_col=".metadata.name"
    dns_jsonpath='{range .items[*]}{.metadata.annotations.external-dns\.alpha\.kubernetes\.io/hostname}{"\n"}{end}'
    target_jsonpath='{range .items[*]}{.metadata.annotations.external-dns\.alpha\.kubernetes\.io/target}{"\n"}{end}'
  else
    emission_kind="dnsendpoints.externaldns.k8s.io"
    name_col=".spec.endpoints[0].dnsName"
    dns_jsonpath='{range .items[*]}{.spec.endpoints[0].dnsName}{"\n"}{end}'
    target_jsonpath='{range .items[*]}{.spec.endpoints[0].targets[0]}{"\n"}{end}'
  fi

  log "waiting (deadline ${DEADLINE_SECONDS}s) for ouroboros to emit ${emission_kind} for BOTH hosts (output=${OUTPUT})"
  deadline=$(( $(date +%s) + DEADLINE_SECONDS ))
  found_dns=0
  while [[ $(date +%s) -lt ${deadline} ]]; do
    items="$(kubectl --context "${CTX}" --namespace ouroboros get "${emission_kind}" \
      --selector='app.kubernetes.io/managed-by=ouroboros' \
      --output jsonpath="${dns_jsonpath}" 2>/dev/null || true)"
    if grep --quiet --line-regexp "${INGRESS_HOST}" <<<"${items}" \
        && grep --quiet --line-regexp "${GATEWAY_HOST}" <<<"${items}"; then
      found_dns=1
      log "${emission_kind} emission observed:"
      kubectl --context "${CTX}" --namespace ouroboros get "${emission_kind}" \
        --selector='app.kubernetes.io/managed-by=ouroboros' \
        --output "custom-columns=NAME:.metadata.name,DNS:${name_col}" \
        | sed 's/^/    /'
      break
    fi
    sleep 2
  done
  [[ "${found_dns}" == "1" ]] || fail "ouroboros did not emit ${emission_kind} for both hosts within ${DEADLINE_SECONDS}s"

  log "verifying every emitted record targets the proxy ClusterIP"
  targets="$(kubectl --context "${CTX}" --namespace ouroboros get "${emission_kind}" \
    --selector='app.kubernetes.io/managed-by=ouroboros' \
    --output jsonpath="${target_jsonpath}" 2>/dev/null)"
  while IFS= read -r target; do
    [[ -z "${target}" ]] && continue
    [[ "${target}" == "${PROXY_IP}" ]] || fail "record target ${target} != proxy ClusterIP ${PROXY_IP}"
  done <<<"${targets}"

  # Empty-hosts mass-prune guard live verification — drop every
  # Ingress/HTTPRoute and let the informer's Delete events drive a
  # natural reconcile. Don't restart the controller: a fresh pod
  # observes an empty cache and gets zero informer events to
  # process (no Add/Update/Delete from the informer initial-list
  # phase when the underlying objects are gone), so Reconcile
  # would only fire on the next 10-minute resync. The Delete
  # events from kubectl delete are what the guard is designed to
  # protect against in production — that is the realistic accident.
  log "verifying empty-hosts mass-prune guard"
  # Delete every hostname-bearing resource in a single kubectl call so
  # the informer Delete events propagate in quick succession and the
  # workqueue's sentinel-key coalescing has all three gone in cache
  # by the time the worker picks the key up. Sequential deletes
  # cause intermediate reconciles with partial hosts (e.g. Ingress
  # gone but Gateway still listening) which would trim a subset of
  # records via normal prune — that's correct behaviour, but it
  # confuses a before/after count check. ExtractHostnames pulls
  # from Gateway listeners too, hence the Gateway needs to go.
  kubectl --context "${CTX}" --namespace hairpin-test delete \
    ingress/echo-ingress \
    httproute/echo-route \
    gateway/echo-gateway \
    --ignore-not-found >/dev/null

  # Informer Delete events propagate within ~1-2s; reconcile fires
  # immediately after. Pad to 30s so a slow CI runner still sees
  # the Warn.
  guard_deadline=$(( $(date +%s) + 30 ))
  guard_seen=0
  while [[ $(date +%s) -lt ${guard_deadline} ]]; do
    if kubectl --context "${CTX}" --namespace ouroboros logs deployment/ouroboros --tail=200 2>/dev/null \
       | grep --quiet "skipping prune to avoid silent mass-delete"; then
      guard_seen=1
      break
    fi
    sleep 2
  done
  if [[ "${guard_seen}" != "1" ]]; then
    log "  empty-hosts guard never fired; controller log:"
    kubectl --context "${CTX}" --namespace ouroboros logs deployment/ouroboros --tail=50 2>&1 | sed 's/^/    /'
    fail "empty-hosts guard did not log Warn after route removal — regression in the mass-prune defence"
  fi

  # The guard protected records that were owned at the moment hosts
  # became []. If intermediate reconciles trimmed some (events
  # propagated one-by-one despite the bulk delete), the survivor
  # count is just whatever was alive when the guard finally fired.
  # The invariant we check: at least one record survived the
  # empty-hosts reconcile — i.e. the guard did NOT silently
  # delete-all. Without the guard, count would drop to 0.
  after_count="$(kubectl --context "${CTX}" --namespace ouroboros get "${emission_kind}" \
    --selector='app.kubernetes.io/managed-by=ouroboros' \
    --output name 2>/dev/null | wc -l | tr -d ' ')"
  if [[ "${after_count}" -lt 1 ]]; then
    fail "empty-hosts guard did not protect records: ${after_count} survived (expected >=1)"
  fi
  log "  guard held: ${after_count} records survived the empty-hosts reconcile"

  log "running helm uninstall and asserting cleanup hook removes DNSEndpoints"
  if ! helm --kube-context "${CTX}" uninstall ouroboros --namespace ouroboros --wait --timeout 2m; then
    log "helm uninstall failed — capturing cleanup-hook Job state before tear-down"
    kubectl --context "${CTX}" --namespace ouroboros get jobs --output wide 2>&1 | sed 's/^/    job: /' || true
    kubectl --context "${CTX}" --namespace ouroboros describe job/ouroboros-cleanup 2>&1 | sed 's/^/    desc: /' || true
    kubectl --context "${CTX}" --namespace ouroboros get pods --selector=job-name=ouroboros-cleanup --output wide 2>&1 | sed 's/^/    pod: /' || true
    kubectl --context "${CTX}" --namespace ouroboros logs --selector=job-name=ouroboros-cleanup --tail=200 2>&1 | sed 's/^/    log: /' || true
    fail "helm uninstall failed — see job/pod state above"
  fi

  # 120s (not 60s) because helm uninstall returns BEFORE post-delete
  # hook completes — the wait flag does not block on hooks. The Job
  # then needs to: pull alpine/kubectl image (~few MB cold pull on a
  # fresh kind node), schedule the Pod, run kubectl delete, wait for
  # finalizers. On a slow CI runner all that adds up.
  cleanup_deadline=$(( $(date +%s) + 120 ))
  cleanup_seen_job=0
  cleanup_ok=0
  while [[ $(date +%s) -lt ${cleanup_deadline} ]]; do
    remaining="$(kubectl --context "${CTX}" --namespace ouroboros get "${emission_kind}" \
      --selector='app.kubernetes.io/managed-by=ouroboros' \
      --output name 2>/dev/null | wc -l | tr -d ' ')"
    if [[ "${remaining}" == "0" ]]; then
      cleanup_ok=1
      break
    fi
    # Snapshot Job + Pod state once during the wait window — the Job's
    # ttlSecondsAfterFinished may reap its Pod before the diagnostic
    # dump runs, so capture logs while they exist.
    if [[ "${cleanup_seen_job}" == "0" ]]; then
      job_status="$(kubectl --context "${CTX}" --namespace ouroboros get job/ouroboros-cleanup --output jsonpath='{.status.conditions[?(@.type=="Complete")].status}{.status.conditions[?(@.type=="Failed")].status}' 2>/dev/null || true)"
      if [[ -n "${job_status}" ]]; then
        log "cleanup Job state observed: ${job_status}"
        kubectl --context "${CTX}" --namespace ouroboros logs --selector=job-name=ouroboros-cleanup --tail=200 2>&1 | sed 's/^/    log: /' || true
        cleanup_seen_job=1
      fi
    fi
    sleep 2
  done
  [[ "${cleanup_ok}" == "1" ]] || fail "post-delete cleanup hook did not remove ouroboros ${emission_kind} within 120s of helm uninstall"

  log "all e2e checks passed (external-dns mode: DNSEndpoint emission + cleanup hook)"
  exit 0
fi

log "waiting (deadline ${DEADLINE_SECONDS}s) for ouroboros to publish BOTH hosts to CoreDNS"
deadline=$(( $(date +%s) + DEADLINE_SECONDS ))
while [[ $(date +%s) -lt ${deadline} ]]; do
  if [[ "${MODE}" == "coredns-import" ]]; then
    # jsonpath: bracket-notation for the data-key because COREDNS_CUSTOM_KEY
    # contains a dot ('ouroboros.override') and the dotted form would parse
    # as nested fields instead of one literal map key.
    snippet="$(kubectl --context "${CTX}" --namespace kube-system \
      get configmap "${COREDNS_CUSTOM_CM}" \
      --output jsonpath="{.data['${COREDNS_CUSTOM_KEY}']}" 2>/dev/null || true)"
    if grep --quiet "${INGRESS_HOST}" <<<"${snippet}" \
        && grep --quiet "${GATEWAY_HOST}" <<<"${snippet}" \
        && ! grep --quiet "BEGIN ouroboros" <<<"${snippet}"; then
      log "import-CM snippet observed (no inline-block markers, plugin lines only):"
      printf '%s\n' "${snippet}" | sed 's/^/    /'
      found=1
      break
    fi
  else
    cm="$(kubectl --context "${CTX}" --namespace kube-system get configmap coredns --output jsonpath='{.data.Corefile}')"
    if grep --quiet "BEGIN ouroboros" <<<"${cm}" \
        && grep --quiet "${INGRESS_HOST}" <<<"${cm}" \
        && grep --quiet "${GATEWAY_HOST}" <<<"${cm}"; then
      log "Corefile mutation observed:"
      grep --extended-regexp "BEGIN ouroboros|${INGRESS_HOST}|${GATEWAY_HOST}|END ouroboros" <<<"${cm}" | sed 's/^/    /'
      found=1
      break
    fi
  fi
  sleep 2
done
[[ "${found:-0}" == "1" ]] || fail "ouroboros did not publish both hosts to CoreDNS within ${DEADLINE_SECONDS}s (mode=${MODE})"

log "rolling out CoreDNS to pick up the new Corefile without a reload-poll race"
# CoreDNS Deployment has 2 replicas; each reloads independently every 30s
# via the reload plugin. After a Corefile mutation the two pods are
# de-synchronised by their poll offsets, and dig hitting the still-stale
# replica returns NODATA for half the queries. A rollout-restart
# guarantees every replica boots WITH the new Corefile already loaded —
# no reload race for the in-cluster checks below.
kubectl --context "${CTX}" --namespace kube-system rollout restart deployment/coredns >/dev/null
kubectl --context "${CTX}" --namespace kube-system rollout status deployment/coredns --timeout=2m

log "running in-cluster DNS + curl checks for both Ingress and Gateway-API paths"
# What we are testing:
#   - DNS resolution for each host MUST point at the ouroboros-proxy ClusterIP
#     (proves the controller wrote the rewrite block AND CoreDNS reloaded)
#   - The TLS handshake + PROXY-protocol injection MUST complete (proves the
#     proxy is intercepting and prepending PROXY v1 correctly).
# Curl is run WITHOUT --fail: any HTTP status code (200/404/500) is acceptable
# because it means the connection succeeded end-to-end. A connection reset
# would be the actual ouroboros regression (Traefik dropping a hairpin
# without PROXY header).
# Pod runs detached (no --rm / --attach) so we can always `kubectl logs`
# after, even when an attach session loses stdout under fast-failure
# conditions on a CI runner.
kubectl --context "${CTX}" --namespace hairpin-test delete pod dnscheck --ignore-not-found >/dev/null 2>&1 || true
kubectl --context "${CTX}" --namespace hairpin-test run dnscheck \
  --image=nicolaka/netshoot:v0.13 --restart=Never \
  --command -- bash -c "
    set -e
    proxy_ip='${PROXY_IP}'
    for host in ${INGRESS_HOST} ${GATEWAY_HOST}; do
      echo '--- dig +short '\$host
      addr=\$(dig +short +tries=2 +time=5 \$host | head -n 1)
      echo \"  resolved to: \${addr:-<empty>}\"
      if [[ \"\$addr\" != \"\$proxy_ip\" ]]; then
        echo \"!!! \$host resolved to '\$addr', want proxy ClusterIP '\$proxy_ip'\"
        exit 1
      fi
      echo '--- curl '\$host '(any HTTP status accepted; we only assert the connection succeeds)'
      # --resolve pins the host:port to the proxy IP we just verified
      # via dig. This bypasses getaddrinfo's ndots=5 search-list dance
      # (default in pod /etc/resolv.conf), which can SERVFAIL on
      # appended .svc.cluster.local suffixes for the .invalid TLD and
      # poison the whole lookup. DNS correctness is already proven by
      # the dig step above; curl here is the TLS + PROXY-protocol probe.
      http=\$(curl --silent --show-error --insecure --max-time 30 \\
        --resolve \"\$host:443:\$proxy_ip\" \\
        --output /dev/null --write-out '%{http_code}' https://\$host/) || {
          echo \"!!! curl https://\$host/ failed at the connection layer\"
          exit 1
        }
      echo \"curl-ok-\$host HTTP:\$http\"
    done
    echo
    echo 'all e2e checks passed: DNS rewrite + PROXY-protocol injection working for both Ingress and Gateway-API paths'
  "

log "waiting for dnscheck pod to finish"
deadline=$(( $(date +%s) + 180 ))
phase=""
while [[ $(date +%s) -lt ${deadline} ]]; do
  phase=$(kubectl --context "${CTX}" --namespace hairpin-test get pod dnscheck \
    --output jsonpath='{.status.phase}' 2>/dev/null || true)
  case "${phase}" in
    Succeeded|Failed) break ;;
  esac
  sleep 3
done

log "dnscheck pod logs (phase=${phase}):"
kubectl --context "${CTX}" --namespace hairpin-test logs dnscheck 2>&1 | sed 's/^/    /'

kubectl --context "${CTX}" --namespace hairpin-test delete pod dnscheck \
  --ignore-not-found >/dev/null 2>&1 || true

[[ "${phase}" == "Succeeded" ]] || fail "dnscheck pod ended in phase '${phase}', expected Succeeded"

log "all e2e checks passed (Ingress + Gateway-API + HTTPRoute via Traefik with PROXY-protocol)"
