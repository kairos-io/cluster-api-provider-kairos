/*
Copyright 2024 The Kairos CAPI Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
implied. See the License for the specific language governing
permissions and limitations under the License.
*/

package kubeconfig

import "fmt"

// RejectionReason identifies which rule in the sanitizer's spec (ADR 0011,
// "Sanitizer (internal/kubeconfig, pure)") caused Sanitize or
// CheckClientCertificateExpiry to reject an input. Callers (the KairosControlPlane
// publish path) switch on Reason to choose a status condition reason
// (NodePushRejected, NodePushCAMismatch, KubeconfigCertificateExpired, ...)
// without parsing free-form error text.
type RejectionReason string

const (
	// ReasonOversized is S0: the raw input exceeds MaxSize.
	ReasonOversized RejectionReason = "Oversized"
	// ReasonUnparseable means clientcmd.Load could not decode the input at
	// all. This is a precondition for every other rule, not one of S1-S4
	// itself.
	ReasonUnparseable RejectionReason = "Unparseable"
	// ReasonStructure is S1: cardinality or reference violations among
	// clusters, users and contexts, or a caller argument (cluster name,
	// expected server) that is missing or malformed.
	ReasonStructure RejectionReason = "InvalidStructure"
	// ReasonUnsafeCluster is S2: a cluster entry carries a disallowed field,
	// or its certificate-authority-data is missing, malformed, or contains
	// no CA certificate.
	ReasonUnsafeCluster RejectionReason = "UnsafeClusterField"
	// ReasonUnsafeIdentity is S4: a user or context entry carries a
	// disallowed field.
	ReasonUnsafeIdentity RejectionReason = "UnsafeIdentityField"
	// ReasonCredentials is S3: the credential shape is wrong (neither or
	// both of an inline cert/key pair and a token), or the cert/key pair
	// does not load with tls.X509KeyPair.
	ReasonCredentials RejectionReason = "InvalidCredentials"
	// ReasonExpired is returned only by CheckClientCertificateExpiry, never
	// by Sanitize: the inline client certificate is outside its validity
	// window as of the time checked.
	ReasonExpired RejectionReason = "CertificateExpired"
)

// RejectedError is returned whenever the sanitizer refuses an input.
//
// Message names the rule and, where useful, the offending field or entry
// name/index — never a byte of input that could be key material or a token.
// Callers must not construct log lines or status conditions from anything
// other than Reason and Message; do not fmt.Sprintf the original raw
// kubeconfig bytes alongside this error.
type RejectedError struct {
	Reason  RejectionReason
	Message string
}

func (e *RejectedError) Error() string {
	return fmt.Sprintf("kubeconfig rejected (%s): %s", e.Reason, e.Message)
}

// reject builds a *RejectedError. format and args must never include raw
// kubeconfig bytes, PEM blocks, keys or tokens — only rule names, field
// names, and entry names/indices, which are structural metadata, not secret
// material.
func reject(reason RejectionReason, format string, args ...any) error {
	return &RejectedError{Reason: reason, Message: fmt.Sprintf(format, args...)}
}
