#!/bin/bash
# Installs the VPA admission controller (webhook only) without recommender or updater.
# This generates self-signed TLS certs and deploys the webhook to kube-system.
set -o errexit
set -o nounset
set -o pipefail

NAMESPACE="${NAMESPACE:-kube-system}"
VPA_VERSION="${VPA_VERSION:-1.7.1}"
TMP_DIR=$(mktemp -d)
trap "rm -rf ${TMP_DIR}" EXIT

echo "Generating TLS certs for VPA admission controller..."

# Generate CA
openssl genrsa -out "${TMP_DIR}/ca.key" 2048
openssl req -x509 -new -nodes -key "${TMP_DIR}/ca.key" -days 365 \
  -out "${TMP_DIR}/ca.crt" -subj "/CN=vpa-webhook-ca"

# Generate server cert
cat > "${TMP_DIR}/server.conf" << EOF
[req]
req_extensions = v3_req
distinguished_name = req_distinguished_name
[req_distinguished_name]
[ v3_req ]
basicConstraints = CA:FALSE
keyUsage = nonRepudiation, digitalSignature, keyEncipherment
extendedKeyUsage = clientAuth, serverAuth
subjectAltName = DNS:vpa-webhook.${NAMESPACE}.svc,DNS:vpa-webhook.${NAMESPACE}.svc.cluster.local
EOF

openssl genrsa -out "${TMP_DIR}/server.key" 2048
openssl req -new -key "${TMP_DIR}/server.key" -out "${TMP_DIR}/server.csr" \
  -subj "/CN=vpa-webhook.${NAMESPACE}.svc" -config "${TMP_DIR}/server.conf"
openssl x509 -req -in "${TMP_DIR}/server.csr" -CA "${TMP_DIR}/ca.crt" \
  -CAkey "${TMP_DIR}/ca.key" -CAcreateserial -out "${TMP_DIR}/server.crt" \
  -days 365 -extensions v3_req -extfile "${TMP_DIR}/server.conf"

# Create secret with file names expected by the admission controller
kubectl delete secret vpa-tls-certs -n "${NAMESPACE}" --ignore-not-found
kubectl create secret generic vpa-tls-certs -n "${NAMESPACE}" \
  --from-file=caCert.pem="${TMP_DIR}/ca.crt" \
  --from-file=serverCert.pem="${TMP_DIR}/server.crt" \
  --from-file=serverKey.pem="${TMP_DIR}/server.key"

CA_BUNDLE=$(base64 < "${TMP_DIR}/ca.crt" | tr -d '\n')

echo "Deploying VPA admission controller..."

# Apply RBAC, deployment, and service
kubectl apply -f https://raw.githubusercontent.com/kubernetes/autoscaler/vertical-pod-autoscaler-${VPA_VERSION}/vertical-pod-autoscaler/deploy/vpa-rbac.yaml
kubectl apply -f https://raw.githubusercontent.com/kubernetes/autoscaler/vertical-pod-autoscaler-${VPA_VERSION}/vertical-pod-autoscaler/deploy/admission-controller-deployment.yaml
kubectl apply -f https://raw.githubusercontent.com/kubernetes/autoscaler/vertical-pod-autoscaler-${VPA_VERSION}/vertical-pod-autoscaler/deploy/admission-controller-service.yaml

# Create MutatingWebhookConfiguration
cat <<EOF | kubectl apply -f -
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingWebhookConfiguration
metadata:
  name: vpa-webhook-config
webhooks:
- name: vpa.k8s.io
  admissionReviewVersions: ["v1"]
  clientConfig:
    service:
      name: vpa-webhook
      namespace: ${NAMESPACE}
      path: "/"
    caBundle: ${CA_BUNDLE}
  rules:
  - apiGroups: [""]
    apiVersions: ["v1"]
    operations: ["CREATE"]
    resources: ["pods"]
  sideEffects: None
  failurePolicy: Ignore
  matchPolicy: Equivalent
  timeoutSeconds: 10
EOF

# Add toleration so VPA can run on tainted control plane nodes (CI uses CriticalAddonsOnly taint)
kubectl patch deployment vpa-admission-controller -n "${NAMESPACE}" --type=json \
  -p '[{"op":"add","path":"/spec/template/spec/tolerations","value":[{"key":"CriticalAddonsOnly","operator":"Exists"}]},{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--feature-gates=CPUStartupBoost=true"}]'


echo "Waiting for VPA admission controller to be ready..."
kubectl rollout status deployment/vpa-admission-controller -n "${NAMESPACE}" --timeout=60s
echo "VPA admission controller installed successfully."
