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
#   coredns       — default; CoreDNS Corefile mutation + in-cluster TLS curl
#   external-dns  — install external-dns chart with --source=crd
#                   --provider=inmemory; assert DNSEndpoint CRs are emitted
#                   for both hostnames; helm uninstall must trigger the
#                   pre-delete cleanup hook and remove the records.
readonly MODE="${MODE:-coredns}"

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

if [[ "${MODE}" == "external-dns" ]]; then
  log "installing upstream DNSEndpoint CRD"
  kubectl --context "${CTX}" apply --filename \
    https://raw.githubusercontent.com/kubernetes-sigs/external-dns/v0.20.0/config/crd/standard/dnsendpoints.externaldns.k8s.io.yaml >/dev/null

  log "installing external-dns with the in-memory provider and CRD source"
  helm --kube-context "${CTX}" repo add external-dns https://kubernetes-sigs.github.io/external-dns/ >/dev/null 2>&1 || true
  helm --kube-context "${CTX}" repo update external-dns >/dev/null
  # NOTE: 'registry' and 'policy' are top-level chart values; setting them
  # via extraArgs duplicates the flag and external-dns aborts with
  # "flag 'registry' cannot be repeated". CRD-source flags + namespace
  # scoping stay in extraArgs because the chart does not expose them as
  # values yet.
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

log "installing ouroboros from local chart in mode=${MODE}"
ouroboros_extra_set=()
if [[ "${MODE}" == "external-dns" ]]; then
  ouroboros_extra_set+=( --set "controller.mode=external-dns" )
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
  log "waiting (deadline ${DEADLINE_SECONDS}s) for ouroboros to emit DNSEndpoint CRs for BOTH hosts"
  deadline=$(( $(date +%s) + DEADLINE_SECONDS ))
  found_dns=0
  while [[ $(date +%s) -lt ${deadline} ]]; do
    items="$(kubectl --context "${CTX}" --namespace ouroboros get dnsendpoints.externaldns.k8s.io \
      --selector='app.kubernetes.io/managed-by=ouroboros' \
      --output jsonpath='{range .items[*]}{.spec.endpoints[0].dnsName}{"\n"}{end}' 2>/dev/null || true)"
    if grep --quiet --line-regexp "${INGRESS_HOST}" <<<"${items}" \
        && grep --quiet --line-regexp "${GATEWAY_HOST}" <<<"${items}"; then
      found_dns=1
      log "DNSEndpoint emission observed:"
      kubectl --context "${CTX}" --namespace ouroboros get dnsendpoints.externaldns.k8s.io \
        --output 'custom-columns=NAME:.metadata.name,DNS:.spec.endpoints[0].dnsName,TARGETS:.spec.endpoints[0].targets,TTL:.spec.endpoints[0].recordTTL' \
        | sed 's/^/    /'
      break
    fi
    sleep 2
  done
  [[ "${found_dns}" == "1" ]] || fail "ouroboros did not emit DNSEndpoints for both hosts within ${DEADLINE_SECONDS}s"

  log "verifying every emitted DNSEndpoint targets the proxy ClusterIP"
  targets="$(kubectl --context "${CTX}" --namespace ouroboros get dnsendpoints.externaldns.k8s.io \
    --selector='app.kubernetes.io/managed-by=ouroboros' \
    --output jsonpath='{range .items[*]}{.spec.endpoints[0].targets[0]}{"\n"}{end}' 2>/dev/null)"
  while IFS= read -r target; do
    [[ -z "${target}" ]] && continue
    [[ "${target}" == "${PROXY_IP}" ]] || fail "DNSEndpoint target ${target} != proxy ClusterIP ${PROXY_IP}"
  done <<<"${targets}"

  log "running helm uninstall and asserting cleanup hook removes DNSEndpoints"
  if ! helm --kube-context "${CTX}" uninstall ouroboros --namespace ouroboros --wait --timeout 2m; then
    log "helm uninstall failed — capturing cleanup-hook Job state before tear-down"
    kubectl --context "${CTX}" --namespace ouroboros get jobs --output wide 2>&1 | sed 's/^/    job: /' || true
    kubectl --context "${CTX}" --namespace ouroboros describe job/ouroboros-cleanup 2>&1 | sed 's/^/    desc: /' || true
    kubectl --context "${CTX}" --namespace ouroboros get pods --selector=job-name=ouroboros-cleanup --output wide 2>&1 | sed 's/^/    pod: /' || true
    kubectl --context "${CTX}" --namespace ouroboros logs --selector=job-name=ouroboros-cleanup --tail=200 2>&1 | sed 's/^/    log: /' || true
    fail "helm uninstall failed — see job/pod state above"
  fi

  cleanup_deadline=$(( $(date +%s) + 60 ))
  cleanup_ok=0
  while [[ $(date +%s) -lt ${cleanup_deadline} ]]; do
    remaining="$(kubectl --context "${CTX}" --namespace ouroboros get dnsendpoints.externaldns.k8s.io \
      --selector='app.kubernetes.io/managed-by=ouroboros' \
      --output name 2>/dev/null | wc -l | tr -d ' ')"
    if [[ "${remaining}" == "0" ]]; then
      cleanup_ok=1
      break
    fi
    sleep 2
  done
  [[ "${cleanup_ok}" == "1" ]] || fail "pre-delete cleanup hook did not remove ouroboros DNSEndpoints within 60s of helm uninstall"

  log "all e2e checks passed (external-dns mode: DNSEndpoint emission + cleanup hook)"
  exit 0
fi

log "waiting (deadline ${DEADLINE_SECONDS}s) for ouroboros to write BOTH hosts into CoreDNS Corefile"
deadline=$(( $(date +%s) + DEADLINE_SECONDS ))
while [[ $(date +%s) -lt ${deadline} ]]; do
  cm="$(kubectl --context "${CTX}" --namespace kube-system get configmap coredns --output jsonpath='{.data.Corefile}')"
  if grep --quiet "BEGIN ouroboros" <<<"${cm}" \
      && grep --quiet "${INGRESS_HOST}" <<<"${cm}" \
      && grep --quiet "${GATEWAY_HOST}" <<<"${cm}"; then
    log "Corefile mutation observed:"
    grep --extended-regexp "BEGIN ouroboros|${INGRESS_HOST}|${GATEWAY_HOST}|END ouroboros" <<<"${cm}" | sed 's/^/    /'
    found=1
    break
  fi
  sleep 2
done
[[ "${found:-0}" == "1" ]] || fail "ouroboros did not write both hosts into the CoreDNS rewrite block within ${DEADLINE_SECONDS}s"

log "ensuring CoreDNS reload picks up the change (default reload interval is 30s)"
# CoreDNS' reload plugin watches the Corefile by polling every 30s by default;
# wait long enough that the rewrite rules are guaranteed to be active before
# we issue DNS queries.
sleep 45

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
