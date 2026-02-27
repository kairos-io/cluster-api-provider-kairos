# Webhook Configuration

This directory contains the webhook configurations for the Kairos CAPI Provider.

The webhook resources are named `mutating-webhook-configuration` and `validating-webhook-configuration` (see `manifests.yaml`). If you use a different kustomize overlay that adds name prefixes, adjust the patch commands accordingly.

## CA Bundle Injection

The webhook configurations require a CA bundle to be injected into the `clientConfig.caBundle` field. This CA bundle comes from the certificate secret created by cert-manager.

### Automatic Injection (Recommended)

If cert-manager v1.5+ is installed with webhook injection enabled, the CA bundle will be automatically injected via annotations on the Certificate resource (see `../certmanager/certificate.yaml`).

### Manual Injection

If automatic injection is not available, you need to manually patch the webhook configurations after the certificate is ready:

```bash
# Wait for certificate to be ready
kubectl wait --for=condition=ready certificate/kairos-capi-webhook-server-cert -n kairos-capi-system --timeout=60s

# Get the CA bundle from the certificate secret
CA_BUNDLE=$(kubectl get secret kairos-capi-webhook-server-cert -n kairos-capi-system -o jsonpath='{.data.ca\.crt}')

# Patch mutating webhook configuration
kubectl patch mutatingwebhookconfiguration mutating-webhook-configuration \
  --type='json' \
  -p="[
    {\"op\": \"replace\", \"path\": \"/webhooks/0/clientConfig/caBundle\", \"value\": \"$CA_BUNDLE\"},
    {\"op\": \"replace\", \"path\": \"/webhooks/1/clientConfig/caBundle\", \"value\": \"$CA_BUNDLE\"}
  ]"

# Patch validating webhook configuration
kubectl patch validatingwebhookconfiguration validating-webhook-configuration \
  --type='json' \
  -p="[
    {\"op\": \"replace\", \"path\": \"/webhooks/0/clientConfig/caBundle\", \"value\": \"$CA_BUNDLE\"},
    {\"op\": \"replace\", \"path\": \"/webhooks/1/clientConfig/caBundle\", \"value\": \"$CA_BUNDLE\"}
  ]"
```

### Post-Install Hook Script

For automated deployments, you can use a post-install hook script. See `hack/post-install-webhook-ca-injection.sh` for an example.
