# Karpenter DRA KWOK Driver 

## Summary
The upstream kubernetes/perf-tests repository includes a [DRA KWOK Driver](https://github.com/kubernetes/perf-tests/pull/3491/files), but it's designed for **ClusterLoader2 scale testing** with pre-created static nodes that cannot be used for Karpenter testing.

This design introduces a **Karpenter DRA KWOK Driver** - a mock DRA driver that acts on behalf of KWOK nodes created by Karpenter. When KWOK nodes register with the cluster, the driver creates ResourceSlices advertising fake GPU/device resources. This simulates what a real DRA driver (like NVIDIA GPU Operator) would do, but with fake devices for testing purposes. The driver watches for KWOK nodes and creates corresponding ResourceSlices based on either Node Overlay or ConfigMap configuration. The driver acts independently as a standard Kubernetes controller, ensuring ResourceSlices exist on the API server for both the scheduler and Karpenter's cluster state to discover.

### Workflow
1. **Test creates ResourceClaim** with device attribute selectors
2. **Test creates DRA pod** referencing the ResourceClaim
3. **Karpenter provisions KWOK node** in response to unschedulable pod
4. **Node registration triggers ResourceSlice creation** based on:
   - **Case 1:** Check for matching NodeOverlay with embedded ResourceSlice objects (future enhancement)
   - **Case 2:** Use ConfigMap mappings if no NodeOverlay matches
5. **Kubernetes scheduler discovers ResourceSlices** and binds pod to node
6. **Pod successfully schedules** to the node with available DRA resources
7. **Test validates** node creation, ResourceSlice creation, pod scheduling, and Karpenter behavior
8. **Cleanup automatically removes** ResourceSlices when nodes are deleted

## Implementation

### Case 1: Node Overlay Integration
Tests **Karpenter's integrated DRA scheduling** where DRA device counts are known during the scheduling simulation via extended resources. NodeOverlay informs Karpenter about expected DRA device capacity during scheduling through extended resources, Provisioner includes these extended resources in NodeClaim templates, and Karpenter provisions nodes knowing they will have specific device counts. The driver then creates ResourceSlices with detailed device information matching the NodeOverlay's extended resource count.

**Example Node Overlay with DRA** (future API extension):
```yaml
apiVersion: karpenter.sh/v1alpha1
kind: NodeOverlay
metadata:
  name: gpu-dra-config
spec:
  weight: 10  # Higher weight for conflict resolution
  requirements:
  - key: node.kubernetes.io/instance-type
    operator: In
    values: ["g5.48xlarge"]
  capacity:
    fake-gpu.kwok.x-k8s.io/device: "8"  # Custom extended resource for DRA devices
  # TODO: Extend NodeOverlay API to embed ResourceSlice templates
  resourceSlices:  # FUTURE: Embedded ResourceSlice objects (not yet implemented)
  - apiVersion: resource.k8s.io/v1alpha3
    kind: ResourceSlice
    spec:
      # nodeName will be filled in by driver when node is created
      driver: "fake-gpu.kwok.x-k8s.io"
      devices:
      - name: "nvidia-h100-0"
        driver: "fake-gpu.kwok.x-k8s.io"
        attributes:
          memory: "80Gi"
          compute-capability: "9.0"
          vendor: "nvidia"
      - name: "nvidia-h100-1"
        driver: "fake-gpu.kwok.x-k8s.io"
        attributes:
          memory: "80Gi"
          compute-capability: "9.0"
          vendor: "nvidia"
      # ... (6 more devices for total of 8)
```

**How it works**:
1. **Test author defines NodeOverlay configuration**: "g5.48xlarge KWOK nodes should have 8x fake H100 GPUs" via ResourceSlices
2. **Driver watches for KWOK nodes**: When Karpenter creates a KWOK node with `instance-type: g5.48xlarge`
3. **NodeOverlay match found**: Driver checks for NodeOverlay with embedded ResourceSlice objects, finds matching configuration
4. **Driver creates ResourceSlice**: Acts as fake DRA driver using embedded ResourceSlice objects from NodeOverlay
5. **Scheduler sees configured devices**: ResourceSlices with fake devices become available for DRA pod scheduling
6. **Test validation**: Validates that the driver correctly provides DRA resources and enables successful pod scheduling

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
1. **Test author defines ConfigMap configuration**: "g5.48xlarge KWOK nodes should have 8x fake H100 GPUs when no NodeOverlay is found"
2. **Driver watches for KWOK nodes**: When Karpenter creates a KWOK node with `instance-type: g5.48xlarge`
3. **No NodeOverlay match found**: Driver checks for NodeOverlay with embedded ResourceSlice objects, finds none, falls back to ConfigMap
4. **Driver creates ResourceSlice**: Acts as fake DRA driver using ConfigMap configuration
5. **Scheduler sees configured devices**: ResourceSlices with fake devices become available for DRA pod scheduling
6. **Test validation**: Validates that the driver correctly provides DRA resources and enables successful pod scheduling

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