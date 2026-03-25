#!/usr/bin/env bash
set -euo pipefail

# Deploy Karpenter to EKS with configurable pod deletion cost management
#
# Usage:
#   ./scripts/deploy-karpenter.sh [options]
#
# Options:
#   --enable-pod-deletion-cost          Enable pod deletion cost management (default: false)
#   --ranking-strategy <strategy>       Set ranking strategy: Random, LargestToSmallest, 
#                                       SmallestToLargest, UnallocatedVCPUPerPodCost (default: Random)
#   --change-detection <true|false>     Enable change detection optimization (default: true)
#   --namespace <namespace>             Karpenter namespace (default: kube-system)
#   --skip-build                        Skip building new image
#   --help                              Show this help message

# Default values
POD_DELETION_COST_ENABLED="false"
POD_DELETION_COST_RANKING_STRATEGY="Random"
POD_DELETION_COST_CHANGE_DETECTION="true"
KARPENTER_NAMESPACE="${KARPENTER_NAMESPACE:-kube-system}"
SKIP_BUILD="false"

# Parse arguments
while [[ $# -gt 0 ]]; do
  case $1 in
    --enable-pod-deletion-cost)
      POD_DELETION_COST_ENABLED="true"
      shift
      ;;
    --ranking-strategy)
      POD_DELETION_COST_RANKING_STRATEGY="$2"
      shift 2
      ;;
    --change-detection)
      POD_DELETION_COST_CHANGE_DETECTION="$2"
      shift 2
      ;;
    --namespace)
      KARPENTER_NAMESPACE="$2"
      shift 2
      ;;
    --skip-build)
      SKIP_BUILD="true"
      shift
      ;;
    --help)
      grep '^#' "$0" | grep -v '#!/' | sed 's/^# //'
      exit 0
      ;;
    *)
      echo "Unknown option: $1"
      echo "Run with --help for usage information"
      exit 1
      ;;
  esac
done

# Validate ranking strategy
case "$POD_DELETION_COST_RANKING_STRATEGY" in
  Random|LargestToSmallest|SmallestToLargest|UnallocatedVCPUPerPodCost)
    ;;
  *)
    echo "Error: Invalid ranking strategy: $POD_DELETION_COST_RANKING_STRATEGY"
    echo "Valid options: Random, LargestToSmallest, SmallestToLargest, UnallocatedVCPUPerPodCost"
    exit 1
    ;;
esac

echo "=== Karpenter Deployment Configuration ==="
echo "Namespace: $KARPENTER_NAMESPACE"
echo "Pod Deletion Cost Enabled: $POD_DELETION_COST_ENABLED"
echo "Ranking Strategy: $POD_DELETION_COST_RANKING_STRATEGY"
echo "Change Detection: $POD_DELETION_COST_CHANGE_DETECTION"
echo "Skip Build: $SKIP_BUILD"
echo "=========================================="

# Build image if not skipped
if [ "$SKIP_BUILD" = "false" ]; then
  echo "Building Karpenter image..."
  make verify build
else
  echo "Skipping build..."
fi

# Extract image details from build output
if [ -f ".build-output" ]; then
  source .build-output
else
  echo "Warning: .build-output not found, using environment variables"
fi

# Apply CRDs
echo "Applying CRDs..."
kubectl apply -f kwok/charts/crds

# Build Helm command with proper image settings
HELM_CMD="helm upgrade --install karpenter kwok/charts \
  --namespace $KARPENTER_NAMESPACE \
  --skip-crds \
  --set logLevel=debug \
  --set controller.resources.requests.cpu=1 \
  --set controller.resources.requests.memory=1Gi \
  --set controller.resources.limits.cpu=1 \
  --set controller.resources.limits.memory=1Gi \
  --set settings.featureGates.nodeRepair=true \
  --set settings.featureGates.staticCapacity=true \
  --set settings.preferencePolicy=Ignore"

# Add image settings if available
if [ -n "${IMG_REPOSITORY:-}" ]; then
  HELM_CMD="$HELM_CMD --set controller.image.repository=$IMG_REPOSITORY"
fi
if [ -n "${IMG_TAG:-}" ]; then
  HELM_CMD="$HELM_CMD --set controller.image.tag=$IMG_TAG"
fi
if [ -n "${IMG_DIGEST:-}" ]; then
  HELM_CMD="$HELM_CMD --set controller.image.digest=$IMG_DIGEST"
fi

# Add environment variables
HELM_CMD="$HELM_CMD \
  --set-string controller.env[0].name=ENABLE_PROFILING \
  --set-string controller.env[0].value=true \
  --set-string controller.env[1].name=FEATURE_GATES \
  --set-string controller.env[1].value=PodDeletionCostManagement=$POD_DELETION_COST_ENABLED \
  --set-string controller.env[2].name=POD_DELETION_COST_RANKING_STRATEGY \
  --set-string controller.env[2].value=$POD_DELETION_COST_RANKING_STRATEGY \
  --set-string controller.env[3].name=POD_DELETION_COST_CHANGE_DETECTION \
  --set-string controller.env[3].value=$POD_DELETION_COST_CHANGE_DETECTION"

# Deploy with Helm
echo "Deploying Karpenter with Helm..."
eval "$HELM_CMD"

echo ""
echo "=== Deployment Complete ==="
echo ""
echo "To verify the deployment:"
echo "  kubectl logs -n $KARPENTER_NAMESPACE deployment/karpenter | grep 'pod.deletioncost'"
echo ""
echo "To check pod annotations:"
echo "  kubectl get pods -o jsonpath='{range .items[*]}{.metadata.name}{\"\t\"}{.metadata.annotations.controller\\.kubernetes\\.io/pod-deletion-cost}{\"\n\"}{end}'"
