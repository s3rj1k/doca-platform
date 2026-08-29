#!/usr/bin/env bash
# Clear the external-reboot annotation so DPF resumes, then open the dashboard.
# Run it only after the server has actually been power cycled.
set -uo pipefail

NS=dpf-operator-system
ANN=provisioning.dpu.nvidia.com/dpunode-external-reboot-required
FILTER='{range .items[?(@.metadata.annotations.provisioning\.dpu\.nvidia\.com/dpunode-external-reboot-required=="true")]}{.metadata.name}{"\n"}{end}'

TRIES=${TRIES:-60}
DELAY=${DELAY:-5}

# This script normally runs seconds after a boot, when the apiserver and the
# provisioning webhook are not accepting connections yet.
retry() {
  local out rc i announced=

  for ((i = 1; i <= TRIES; i++)); do
    out=$("$@" 2>&1)
    rc=$?

    if [ "$rc" -eq 0 ]; then
      printf '%s\n' "$out"
      return 0
    fi

    case "$out" in
      *'failed to call webhook'*|*'connection refused'*|*'was refused'*|\
      *'no endpoints available'*|*'i/o timeout'*|*'server API group list'*|*'unexpected EOF'*)
        if [ -z "$announced" ]; then
          echo "Waiting for the cluster to finish coming up..." >&2
          announced=1
        fi
        sleep "$DELAY"
        ;;
      *)
        printf '%s\n' "$out" >&2
        return "$rc"
        ;;
    esac
  done

  printf '%s\n' "$out" >&2
  echo "Gave up after $((TRIES * DELAY))s." >&2
  return 1
}

if ! raw=$(retry kubectl get dpunode -n "$NS" -o jsonpath="$FILTER"); then
  echo "Could not list DPUNodes, nothing was released." >&2
  exit 1
fi

mapfile -t nodes < <(printf '%s' "$raw")

if [ "${#nodes[@]}" -eq 0 ]; then
  echo "No DPUNode is waiting for a power cycle."
else
  echo "Releasing ${nodes[*]}"
  if ! retry kubectl annotate dpunode -n "$NS" "${nodes[@]}" "${ANN}-"; then
    echo "Could not clear the annotation on ${nodes[*]}." >&2
    exit 1
  fi
fi

dash="$(dirname "$0")/dpf-dashboard.py"
[ -f "$dash" ] && exec python3 "$dash"

exit 0
