#!/usr/bin/env bash
# Runs the real-namespace E2E tests (go test -tags azure) with full coverage
# on a temporary Standard-tier Service Bus namespace, then deletes it.
#
# The standing test namespace is Basic (queues only), so the tests that need
# topics, duplicate detection or dead-letter forwarding are skipped there.
# Run this before a release. It costs money while it runs: Standard is
# billed per hour, and the namespace lives for the length of the run (a few
# minutes).
#
# Needs: az (logged in, allowed to create a namespace and a role assignment
# in the resource group), curl, go.
#
# Environment (all optional):
#   LAZYBUS_E2E_SUBSCRIPTION    default lazybus-dev
#   LAZYBUS_E2E_RESOURCE_GROUP  default rg-lazybus-test
#   LAZYBUS_E2E_LOCATION        default westeurope
#   LAZYBUS_E2E_NAMESPACE       default sb-lazybus-e2e-<random>
#
# Arguments are passed on to go test (for example -v).
#
# The namespace is deleted on exit, also after a failure or ctrl-c. If the
# delete fails, the script prints the command that removes it.
set -euo pipefail

SUBSCRIPTION="${LAZYBUS_E2E_SUBSCRIPTION:-lazybus-dev}"
RESOURCE_GROUP="${LAZYBUS_E2E_RESOURCE_GROUP:-rg-lazybus-test}"
LOCATION="${LAZYBUS_E2E_LOCATION:-westeurope}"
NAMESPACE="${LAZYBUS_E2E_NAMESPACE:-sb-lazybus-e2e-$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')}"
FQDN="$NAMESPACE.servicebus.windows.net"
ROLE="Azure Service Bus Data Owner"
RBAC_TIMEOUT=300 # seconds to wait for the role assignment to take effect

for tool in az curl go; do
  command -v "$tool" >/dev/null 2>&1 || { echo "e2e-standard: $tool not found" >&2; exit 1; }
done

cd "$(dirname "$0")/.."

created=0
cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  if [ "$created" = 1 ]; then
    echo "e2e-standard: deleting namespace $NAMESPACE" >&2
    if ! az servicebus namespace delete --subscription "$SUBSCRIPTION" \
      --resource-group "$RESOURCE_GROUP" --name "$NAMESPACE"; then
      echo "e2e-standard: DELETE FAILED, namespace $NAMESPACE is still billed. Remove it with:" >&2
      echo "  az servicebus namespace delete --subscription '$SUBSCRIPTION' --resource-group '$RESOURCE_GROUP' --name '$NAMESPACE'" >&2
      [ "$rc" != 0 ] || rc=1
    fi
  fi
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

echo "e2e-standard: creating Standard namespace $NAMESPACE ($RESOURCE_GROUP, $SUBSCRIPTION, $LOCATION)" >&2
# Set before the call: a create that fails or is interrupted half-way may
# still leave a namespace behind.
created=1
namespace_id="$(az servicebus namespace create --subscription "$SUBSCRIPTION" \
  --resource-group "$RESOURCE_GROUP" --name "$NAMESPACE" --location "$LOCATION" \
  --sku Standard --query id --output tsv)"

user_id="$(az ad signed-in-user show --query id --output tsv)"
echo "e2e-standard: assigning '$ROLE' on $NAMESPACE to the signed-in user" >&2
az role assignment create --subscription "$SUBSCRIPTION" --role "$ROLE" \
  --assignee-object-id "$user_id" --assignee-principal-type User \
  --scope "$namespace_id" --output none

# http_status prints the status of a data-plane queue listing made with the
# signed-in user's token: 200 once the role assignment has taken effect, 401
# before. The token goes to curl on stdin, not on its command line.
http_status() {
  local token
  token="$(az account get-access-token --subscription "$SUBSCRIPTION" \
    --resource https://servicebus.azure.net --query accessToken --output tsv)" || return 1
  curl --silent --output /dev/null --write-out '%{http_code}' --max-time 20 \
    --header @- "https://$FQDN/\$Resources/Queues?api-version=2021-05" \
    <<<"Authorization: Bearer $token"
}

echo "e2e-standard: waiting for the role assignment to take effect (up to ${RBAC_TIMEOUT}s)" >&2
deadline=$((SECONDS + RBAC_TIMEOUT))
ok=0
while :; do
  status="$(http_status || true)"
  if [ "$status" = 200 ]; then
    ok=$((ok + 1))
    # Two answers in a row: the first 200 can come from one front end
    # while another still answers 401.
    [ "$ok" -lt 2 ] || break
  else
    ok=0
    if [ "$SECONDS" -ge "$deadline" ]; then
      echo "e2e-standard: data plane still answers HTTP ${status:-none} after ${RBAC_TIMEOUT}s" >&2
      exit 1
    fi
    echo "e2e-standard: HTTP ${status:-none}, retrying" >&2
  fi
  sleep 10
done

echo "e2e-standard: running the azure E2E tests against $FQDN" >&2
LAZYBUS_AZURE_NAMESPACE="$FQDN" go test -tags azure -count=1 "$@" ./...
