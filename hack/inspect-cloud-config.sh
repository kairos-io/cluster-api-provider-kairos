#!/bin/bash
# Inspect cloud-config from KairosConfig secret
# Usage: ./hack/inspect-cloud-config.sh [kairosconfig-name] [namespace]

KAIROS_CONFIG_NAME=${1:-kairos-control-plane-0}
NAMESPACE=${2:-default}

echo "=== Finding KairosConfig: $KAIROS_CONFIG_NAME ==="
SECRET_NAME=$(kubectl get kairosconfig "$KAIROS_CONFIG_NAME" -n "$NAMESPACE" -o jsonpath='{.status.dataSecretName}' 2>/dev/null)

if [ -z "$SECRET_NAME" ]; then
    echo "❌ KairosConfig not found or secret not ready"
    exit 1
fi

echo "Secret name: $SECRET_NAME"
echo ""
echo "=== DECODED CLOUD-CONFIG (as it will be used by cloud-init) ==="
echo ""

# Decode: Kubernetes base64 -> controller base64 -> actual YAML
CLOUD_CONFIG=$(kubectl get secret "$SECRET_NAME" -n "$NAMESPACE" -o jsonpath='{.data.value}' 2>/dev/null | base64 -d | base64 -d)
echo "$CLOUD_CONFIG" | cat -n

echo ""
echo "=== YAML VALIDATION ==="
if echo "$CLOUD_CONFIG" | python3 -c "import sys, yaml; yaml.safe_load(sys.stdin)" 2>&1 >/dev/null; then
    echo "✅ Valid YAML"
else
    echo "❌ Invalid YAML"
    echo "$CLOUD_CONFIG" | python3 -c "import sys, yaml; yaml.safe_load(sys.stdin)" 2>&1
fi

echo ""
echo "=== CLOUD-CONFIG STRUCTURE ANALYSIS ==="
echo "Total lines: $(echo "$CLOUD_CONFIG" | wc -l)"
echo "Has #cloud-config header: $(echo "$CLOUD_CONFIG" | grep -q '^#cloud-config' && echo 'Yes ✅' || echo 'No ❌')"
echo "Has users section: $(echo "$CLOUD_CONFIG" | grep -q '^users:' && echo 'Yes ✅' || echo 'No ❌')"
echo "Has k0s section: $(echo "$CLOUD_CONFIG" | grep -q '^k0s:' && echo 'Yes ✅' || echo 'No ❌')"
echo ""
echo "=== POTENTIAL ISSUES ==="
ISSUES=0
if echo "$CLOUD_CONFIG" | grep -q '^  ssh_authorized_keys:$'; then
    NEXT_LINE=$(echo "$CLOUD_CONFIG" | grep -A 1 '^  ssh_authorized_keys:$' | tail -1)
    if [[ ! "$NEXT_LINE" =~ ^[[:space:]]*- ]]; then
        echo "⚠️  ssh_authorized_keys is empty (line $(echo "$CLOUD_CONFIG" | grep -n '^  ssh_authorized_keys:$' | cut -d: -f1))"
        echo "   Cloud-init may ignore empty lists. This will be fixed in the next deployment."
        ISSUES=$((ISSUES+1))
    fi
fi
if echo "$CLOUD_CONFIG" | grep -q '{{'; then
    echo "ℹ️  Template variables found (Kairos templating - this is intentional):"
    echo "$CLOUD_CONFIG" | grep -n '{{' | sed 's/^/   Line /'
fi
if [ $ISSUES -eq 0 ]; then
    echo "✅ No obvious issues found"
fi

echo ""
echo "=== CLOUD-CONFIG SECTIONS (parsed) ==="
# The heredoc is python3's stdin, so it would override a pipe and the config
# would never reach the program. Hand it over in the environment instead, which
# keeps the program itself readable as a heredoc.
CLOUD_CONFIG="$CLOUD_CONFIG" python3 << 'PYTHON'
import os
import yaml

try:
    data = yaml.safe_load(os.environ["CLOUD_CONFIG"])
    if data:
        for key, value in data.items():
            print(f"  {key}:")
            if isinstance(value, dict):
                for subkey in value.keys():
                    print(f"    - {subkey}")
            elif isinstance(value, list):
                print(f"    (list with {len(value)} items)")
            else:
                print(f"    = {value}")
    else:
        print("  (YAML parsed to None - check for syntax issues)")
except Exception as e:
    print(f"  Error: {e}")
PYTHON

echo ""
echo "=== TO VIEW ON VM ==="
echo "Once the VM is running, you can check the cloud-config on the VM:"
echo "  sudo cat /var/lib/cloud/instance/user-data.txt"
echo "  sudo cloud-init status"
echo "  sudo cloud-init status --long"
