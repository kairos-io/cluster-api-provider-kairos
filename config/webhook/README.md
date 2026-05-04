# Webhook Configuration

This directory contains the webhook configurations for the Kairos CAPI Provider.

## CA Bundle Injection

The webhook configurations require a CA bundle in `clientConfig.caBundle`.
That field is populated automatically by [cert-manager's CA injector](https://cert-manager.io/docs/concepts/ca-injector/),
which watches the `cert-manager.io/inject-ca-from: <namespace>/<cert-name>`
annotation on the Mutating/ValidatingWebhookConfiguration objects (see
`ca_injection_patch.yaml`) and copies the CA from the referenced Certificate's
secret. The annotation's namespace and name are wired to the Certificate via
`replacements` in `../default/kustomization.yaml`, so they track `namePrefix`
and the `namespace:` field automatically.

cert-manager (≥ v1.5) with the CA injector enabled is therefore a hard
prerequisite for installing the provider.