context_name="karpenter-test-kind-cluster"
prom_ns="prometheus"
kind create cluster --name ${context_name}
cd ..
cd ..
cd ..
make apply-with-kind
cat <<EOF | envsubst | kubectl apply -f -
apiVersion: karpenter.sh/v1beta1
kind: NodePool
metadata:
  name: default
spec:
  template:
    spec:
      requirements:
        - key: kubernetes.io/arch
          operator: In
          values: ["amd64"]
        - key: kubernetes.io/os
          operator: In
          values: ["linux"]
        - key: karpenter.sh/capacity-type
          operator: In
          values: ["spot"]
      nodeClassRef:
        name: nil
  limits:
    cpu: 1500
  disruption:
    consolidationPolicy: WhenUnderutilized
    expireAfter: 720h # 30 * 24h = 720h
EOF
kubectl create namespace "$prom_ns"
helm upgrade --values valuesv3.yaml --install prometheus prometheus-community/kube-prometheus-stack -n "$prom_ns" \
  --set "kubelet.serviceMonitor.cAdvisorRelabelings[0].targetLabel=metrics_path" \
  --set "kubelet.serviceMonitor.cAdvisorRelabelings[0].action=replace" \
  --set "kubelet.serviceMonitor.cAdvisorRelabelings[0].sourceLabels[0]=__metrics_path__" \
  --set "kubelet.serviceMonitor.cAdvisorRelabelings[1].targetLabel=clusterName" \
  --set "kubelet.serviceMonitor.cAdvisorRelabelings[1].replacement=$CLUSTER_NAME" \
  --set "kubelet.serviceMonitor.cAdvisorRelabelings[2].targetLabel=gitRef" \
  --set "kubelet.serviceMonitor.cAdvisorRelabelings[2].replacement=$(git rev-parse HEAD)" \
  --set "kubelet.serviceMonitor.cAdvisorRelabelings[3].targetLabel=mostRecentTag" \
  --set "kubelet.serviceMonitor.cAdvisorRelabelings[3].replacement=$(git describe --abbrev=0 --tags)" \
  --set "kubelet.serviceMonitor.cAdvisorRelabelings[4].targetLabel=commitsAfterTag" \
  --set "kubelet.serviceMonitor.cAdvisorRelabelings[4].replacement=\"$(git describe --tags | cut -d '-' -f 2)\"" \
  --wait
# Testing out pyroscope for profiling
helm repo add grafana https://grafana.github.io/helm-charts
helm repo update
# This only creates one replica for testing, but not multiple backends
#kubectl create namespace pyroscope-test
helm -n karpenter install pyroscope grafana/pyroscope
make test
read -p "press enter to delete cluster" temp_var
kind delete cluster --name ${context_name}