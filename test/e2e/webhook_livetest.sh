#!/usr/bin/env bash
# webhook_livetest.sh — v0.5.0 event-driven pre-warm end-to-end exercise.
#
# Not a CI job. Not idempotent unless CLUSTER_NAME is unique per run.
# Assumes the maintainer has manually registered a GitHub App, set up
# a smee.io channel, and installed the App on sarataha/warmrunners-livetest.
#
# Required env vars:
#   WEBHOOK_SECRET_FILE   path to a file containing the App's webhook secret
#   APP_PRIVATE_KEY_FILE  path to the App's private key PEM
#   APP_ID                integer, the App ID from github.com
#   SMEE_URL              https://smee.io/<channel> — same URL configured on the App
#   LIVETEST_PAT          PAT with actions:write on sarataha/warmrunners-livetest

set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-wrp-v050-livetest}"
NAMESPACE="warmrunners-system"
IMG_TAG="warmrunners:v0.5.0-livetest"
LIVETEST_REPO="sarataha/warmrunners-livetest"

check_env() {
  : "${WEBHOOK_SECRET_FILE:?required}"
  : "${APP_PRIVATE_KEY_FILE:?required}"
  : "${APP_ID:?required}"
  : "${SMEE_URL:?required}"
  : "${LIVETEST_PAT:?required}"
  [[ -f "$WEBHOOK_SECRET_FILE" ]] || { echo "$WEBHOOK_SECRET_FILE not found" >&2; exit 1; }
  [[ -f "$APP_PRIVATE_KEY_FILE" ]] || { echo "$APP_PRIVATE_KEY_FILE not found" >&2; exit 1; }
  case "$SMEE_URL" in https://smee.io/*) : ;; *) echo "SMEE_URL must be https://smee.io/…" >&2; exit 1;; esac
}

retry() {
  # retry <timeout_seconds> <interval_seconds> <predicate as bash string>
  local timeout="$1" interval="$2"; shift 2
  local end=$(( SECONDS + timeout ))
  while (( SECONDS < end )); do
    if bash -c "$*"; then return 0; fi
    sleep "$interval"
  done
  echo "retry timed out: $*" >&2
  return 1
}

step_up_cluster() {
  kind create cluster --name "$CLUSTER_NAME"
  docker build -t "$IMG_TAG" .
  kind load docker-image "$IMG_TAG" --name "$CLUSTER_NAME"
  make install
  make deploy IMG="$IMG_TAG"
  kubectl -n "$NAMESPACE" rollout status deploy/warmrunners-controller-manager --timeout=180s
}

install_arc() {
  # ARC's official charts are OCI-only; no `helm repo add` needed.
  helm install arc-controller -n arc-systems --create-namespace \
    oci://ghcr.io/actions/actions-runner-controller-charts/gha-runner-scale-set-controller
  kubectl create ns arc-runners --dry-run=client -o yaml | kubectl apply -f -
  kubectl -n arc-runners create secret generic gh-token \
    --from-literal=github_token="$LIVETEST_PAT" \
    --dry-run=client -o yaml | kubectl apply -f -
  helm install livetest-runners -n arc-runners \
    oci://ghcr.io/actions/actions-runner-controller-charts/gha-runner-scale-set \
    --set githubConfigUrl="https://github.com/$LIVETEST_REPO" \
    --set githubConfigSecret=gh-token \
    --set minRunners=0 --set maxRunners=5
}

apply_secrets() {
  kubectl -n "$NAMESPACE" create secret generic warmrunners-app-key \
    --from-file=private-key.pem="$APP_PRIVATE_KEY_FILE" \
    --dry-run=client -o yaml | kubectl apply -f -
  kubectl -n "$NAMESPACE" create secret generic warmrunners-app-webhook \
    --from-file=secret="$WEBHOOK_SECRET_FILE" \
    --dry-run=client -o yaml | kubectl apply -f -
  # gh-token in warmrunners-system for the WRP's demand poll (auth.secretRef).
  kubectl -n "$NAMESPACE" create secret generic gh-token \
    --from-literal=token="$LIVETEST_PAT" \
    --dry-run=client -o yaml | kubectl apply -f -
}

apply_gha_and_wrp() {
  cat <<YAML | kubectl apply -f -
apiVersion: autoscaling.warmrunners.io/v1alpha1
kind: GitHubApp
metadata:
  name: warmrunners-app
spec:
  appID: $APP_ID
  privateKeyRef: {name: warmrunners-app-key, key: private-key.pem, namespace: $NAMESPACE}
  webhookSecretRef: {name: warmrunners-app-webhook, key: secret, namespace: $NAMESPACE}
  ingress:
    mode: tunnel
    tunnel:
      relayURL: $SMEE_URL
---
apiVersion: autoscaling.warmrunners.io/v1alpha1
kind: WarmRunnerPolicy
metadata:
  name: livetest
  namespace: $NAMESPACE
spec:
  github:
    owner: sarataha
    repository: warmrunners-livetest
    labels: [self-hosted, linux, x64]
    auth:
      secretRef: {name: gh-token, key: token}
  target:
    arc:
      runnerSet:
        name: livetest-runners
        namespace: arc-runners
  floor: {min: 0, max: 5}
  queueRule: {pollInterval: 30s}
  githubAppRef: {name: warmrunners-app}
  activeWindowSeconds: 300
YAML
}

trigger_workflow() {
  env -u GITHUB_TOKEN GITHUB_TOKEN="$LIVETEST_PAT" \
    gh workflow run load.yml -R "$LIVETEST_REPO" --ref main
}

wait_for_signal() {
  echo "waiting for lastEventSource=webhook…"
  retry 30 1 "[[ \"\$(kubectl -n $NAMESPACE get wrp livetest -o jsonpath='{.status.lastEventSource}')\" == 'webhook' ]]"
  echo "waiting for activeUntil non-empty…"
  retry 15 1 "[[ -n \"\$(kubectl -n $NAMESPACE get wrp livetest -o jsonpath='{.status.activeUntil}')\" ]]"
  echo "waiting for minRunners > 0 on livetest-runners…"
  retry 30 1 "[[ \"\$(kubectl -n arc-runners get autoscalingrunnerset livetest-runners -o jsonpath='{.spec.minRunners}')\" != '0' ]]"
}

test_poll_fallback() {
  echo "breaking tunnel: patching GitHubApp to unreachable URL…"
  kubectl patch githubapp warmrunners-app --type=merge \
    -p '{"spec":{"ingress":{"mode":"tunnel","tunnel":{"relayURL":"https://smee.io/does-not-exist-000000"}}}}'
  echo "waiting for lastEventSource to fall back to poll…"
  retry 120 5 "[[ \"\$(kubectl -n $NAMESPACE get wrp livetest -o jsonpath='{.status.lastEventSource}')\" == 'poll' ]]"
}

test_expiry_drop() {
  echo "waiting past activeWindow (300s) for floor to drop back to 0…"
  retry 360 10 "[[ \"\$(kubectl -n arc-runners get autoscalingrunnerset livetest-runners -o jsonpath='{.spec.minRunners}')\" == '0' ]]"
}

tear_down() {
  if [[ "${KEEP_CLUSTER:-0}" == "1" ]]; then
    echo "KEEP_CLUSTER=1 — leaving cluster $CLUSTER_NAME in place for debugging"
    return 0
  fi
  echo "tearing down…"
  kind delete cluster --name "$CLUSTER_NAME" || true
  # Revert any config/manager/kustomization.yaml mutation from `make deploy`
  git -C "$(git rev-parse --show-toplevel)" checkout -- config/manager/kustomization.yaml 2>/dev/null || true
}

main() {
  check_env
  trap tear_down EXIT

  step_up_cluster
  install_arc
  apply_secrets
  apply_gha_and_wrp

  # Give the tunnel a moment to connect before triggering the workflow.
  sleep 10

  trigger_workflow
  wait_for_signal
  test_poll_fallback
  test_expiry_drop

  echo "livetest PASS"
}

main "$@"
