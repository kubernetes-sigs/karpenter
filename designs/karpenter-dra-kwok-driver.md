# Karpenter DRA KWOK Driver 

## Summary
The upstream kubernetes/perf-tests repository includes a [DRA KWOK Driver](https://github.com/kubernetes/perf-tests/pull/3491/files), but it's designed for **ClusterLoader2 scale testing** with pre-created static nodes that cannot be used for Karpenter testing.

This design introduces a **Karpenter DRA KWOK Driver** - a mock DRA driver that acts on behalf of KWOK nodes created by Karpenter. When KWOK nodes register with the cluster, the driver creates ResourceSlices advertising fake GPU/device resources. This simulates what a real DRA driver (like NVIDIA GPU Operator) would do, but with fake devices for testing purposes. The driver watches for KWOK nodes and creates corresponding ResourceSlices based on either Node Overlay or ConfigMap configuration. The driver acts independently as a standard Kubernetes controller, ensuring ResourceSlices exist on the API server for both the scheduler and Karpenter's cluster state to discover.

### Workflow
1. **Test creates ResourceClaim** with device attribute selectors
2. **Test creates DRA pod** referencing the ResourceClaim
3. **Karpenter provisions KWOK node** in response to unschedulable pod
4. **Node registration triggers ResourceSlice creation** based on:
   - **Case 1:** Check for matching NodeOverlay with DRA annotations
   - **Case 2:** Use ConfigMap mappings if no NodeOverlay matches
5. **Kubernetes scheduler discovers ResourceSlices** and binds pod to node
6. **Pod successfully schedules** to the node with available DRA resources
7. **Test validates** node creation, ResourceSlice creation, pod scheduling, and Karpenter behavior
8. **Cleanup automatically removes** ResourceSlices when nodes are deleted

## Implementation

### Case 1: Node Overlay Integration
Tests **Karpenter's integrated DRA scheduling** where DRA resources are known during the scheduling simulation. NodeOverlay informs Karpenter about expected DRA resources during scheduling, Provisioner includes DRA resources in NodeClaim templates, and Karpenter provisions nodes knowing they will have specific DRA devices. The driver then creates ResourceSlices matching what Karpenter expected.

**Example Node Overlay with DRA** (using existing API):
```yaml
apiVersion: karpenter.sh/v1alpha1
kind: NodeOverlay
metadata:
  name: gpu-dra-config
  annotations:
    dra.karpenter.sh/device-config: |
      driver: "fake-gpu.kwok.x-k8s.io"
      devices:
      - name: "nvidia-h100"
        count: 8
        attributes:
          memory: "80Gi"
          compute-capability: "9.0"
          vendor: "nvidia"
spec:
  requirements:
  - key: node.kubernetes.io/instance-type
    operator: In
    values: ["g5.48xlarge"]
  capacity:
    nvidia.com/gpu: "8"  # DRA device capacity
```

**How it works**:
1. **Driver discovers NodeOverlay resources**: Watches for NodeOverlay resources with DRA annotations and capacity
2. **Node Overlay defines DRA intent**: Uses `capacity` for device resources and `annotations` for device metadata
3. **Karpenter scheduling integration**: Provisioner includes DRA resources in scheduling simulation via NodeOverlay
4. **KWOK node creation triggers driver**: When Karpenter provisions a KWOK node, driver finds matching NodeOverlay and extracts DRA configuration
5. **Fake ResourceSlice creation**: Acts as mock DRA driver for the KWOK node, creating fake ResourceSlices for testing
6. **Test validation**: Validates that Karpenter's DRA-aware scheduling worked correctly with fake GPU resources

### Case 2: ConfigMap Fallback Configuration
Tests **DRA resource provisioning when no NodeOverlay configuration is found**. When the driver cannot find matching NodeOverlay resources with DRA annotations for a KWOK node, it falls back to ConfigMap-based device configuration. This validates that the driver can handle nodes that weren't pre-configured with DRA resources in NodeOverlay and ensures consistent DRA device availability across different provisioning scenarios.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: kwok-dra-config
data:
  config.yaml: |
    driver: "fake-gpu.kwok.x-k8s.io"
    mappings:
    - name: "h100-nodes"
      nodeSelector:
        matchLabels:
          node.kubernetes.io/instance-type: "g5.48xlarge"
          kwok.x-k8s.io/node: "fake"
      resourceSlice:
        devices:
        - name: "nvidia-h100"
          count: 8
          attributes:
            memory: "80Gi"
            compute-capability: "9.0" 
            device-type: "gpu"
            vendor: "nvidia"
    - name: "fpga-nodes"
      nodeSelector:
        matchLabels:
          node.kubernetes.io/instance-type: "f1.2xlarge"
          kwok.x-k8s.io/node: "fake"
      resourceSlice:
        devices:
        - name: "xilinx-u250"
          count: 1
          attributes:
            memory: "16Gi"
            device-type: "fpga"
            vendor: "xilinx"
```

**How it works**:
1. **Test author defines manual fallback mappings**: "g5.48xlarge KWOK nodes should have 8x fake H100 GPUs when no NodeOverlay is found"
2. **Driver watches for KWOK nodes**: When Karpenter creates a KWOK node with `instance-type: g5.48xlarge`
3. **No NodeOverlay match found**: Driver checks for NodeOverlay with DRA annotations, finds none, falls back to ConfigMap
4. **Karpenter KWOK DRA driver creates ResourceSlice**: Acts as fake DRA driver using ConfigMap configuration
5. **Scheduler sees configured devices**: ResourceSlices with fake devices become available for DRA pod scheduling
6. **Test validation**: Validates that the driver correctly provides DRA resources even without NodeOverlay configuration

## Possible Directory Structure

```
karpenter/
├── pkg/
│   └── controllers/
│       ├── controllers.go                # MODIFIED: Register DRAKWOK driver
│       └── dra/                          # NEW: DRA controller package
│           ├── dra_kwok_driver.go        # NEW: Main DRA driver logic
│           ├── node_overlay.go           # NEW: Node Overlay parsing (Case 1)
│           ├── config.go                 # NEW: ConfigMap parsing (Case 2)
│           └── resourceslice.go          # NEW: ResourceSlice operations
│
└── test/suites/
    └── integration/
        └── dra_kwok_integration_test.go  # Our DRA KWOK integration tests
```