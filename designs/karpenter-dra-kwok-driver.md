# KWOK DRA Driver Design

## Summary
The upstream kubernetes/perf-tests repository includes a [DRA KWOK Driver](https://github.com/kubernetes/perf-tests/pull/3491/files), but it's designed for **ClusterLoader2 scale testing** with pre-created static nodes that cannot be used for Karpenter testing.

This design introduces a **Karpenter DRA KWOK Driver** - a mock DRA driver that acts on behalf of KWOK nodes created by Karpenter. When KWOK nodes register with the cluster, the driver creates ResourceSlices advertising fake GPU/device resources. This simulates what a real DRA driver (like NVIDIA GPU Operator) would do, but with fake devices for testing purposes. The driver uses a polling approach (30-second interval) to periodically reconcile all KWOK nodes and creates corresponding ResourceSlices based on either Node Overlay or DRAConfig CRD. The driver acts independently as a standard Kubernetes controller, ensuring ResourceSlices exist on the API server for both the scheduler and Karpenter's cluster state to discover.

### Workflow
1. **Test creates ResourceClaim** with device attribute selectors
2. **Test creates DRA pod** referencing the ResourceClaim
3. **Karpenter provisions KWOK node** in response to unschedulable pod
4. **Driver polling loop detects new node** (within 30 seconds) and creates ResourceSlices based on:
   - **Case 1:** Check for matching NodeOverlay with embedded ResourceSlice objects (future enhancement)
   - **Case 2:** Use DRAConfig CRD mappings if no NodeOverlay matches
   - **Case 3:** Eventually cloudproviders will be able to provide potential ResourceSlice shapes through the InstanceType interface (Future TODO: implement a way for cloudproviders to inform our DRAKWOKDriver of those shapes).
5. **Kubernetes scheduler discovers ResourceSlices** and binds pod to node
6. **Pod successfully schedules** to the node with available DRA resources
7. **Test validates** node creation, ResourceSlice creation, pod scheduling, and Karpenter behavior
8. **Cleanup automatically removes** ResourceSlices in next polling cycle when nodes are deleted

## Implementation

### Case 1: Node Overlay Integration
Tests **Karpenter's integrated DRA scheduling** where DRA device counts are known during the scheduling simulation via extended resources. NodeOverlay informs Karpenter about expected DRA device capacity during scheduling through extended resources, Provisioner includes these extended resources in NodeClaim templates, and Karpenter provisions nodes knowing they will have specific device counts. The driver then creates ResourceSlices with detailed device information matching the NodeOverlay's extended resource count.

**Example Node Overlay with DRA** (future API extension):
```yaml
apiVersion: test.karpenter.sh/v1alpha1
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
    test.karpenter.sh/device: "8"  # Custom extended resource for DRA devices
  # TODO: Extend NodeOverlay API to embed ResourceSlice templates
  resourceSlices:  # FUTURE: Embedded ResourceSlice objects (not yet implemented)
  - apiVersion: resource.k8s.io/v1
    kind: ResourceSlice
    spec:
      # nodeName will be filled in by driver when node is created
      driver: "test.karpenter.sh"
      devices:
      - name: "nvidia-h100-0"
        driver: "test.karpenter.sh"
        attributes:
          memory: "80Gi"
          compute-capability: "9.0"
          vendor: "nvidia"
      - name: "nvidia-h100-1"
        driver: "test.karpenter.sh"
        attributes:
          memory: "80Gi"
          compute-capability: "9.0"
          vendor: "nvidia"
```

**How it works**:
1. **Test author defines NodeOverlay configuration**: "g5.48xlarge KWOK nodes should have 8x fake H100 GPUs" via ResourceSlices
2. **Karpenter creates KWOK node**: Node with `instance-type: g5.48xlarge` is created
3. **Driver polling detects new node**: Within 30 seconds, driver reconciliation loop discovers the node
4. **NodeOverlay match found**: Driver checks for NodeOverlay with embedded ResourceSlice objects, finds matching configuration
5. **Driver creates ResourceSlice**: Acts as fake DRA driver using embedded ResourceSlice objects from NodeOverlay
6. **Scheduler sees configured devices**: ResourceSlices with fake devices become available for DRA pod scheduling
7. **Test validation**: Validates that the driver correctly provides DRA resources and enables successful pod scheduling

### Case 2: CRD-Based Fallback Configuration
Tests **DRA resource provisioning via strongly-typed CRD when no NodeOverlay configuration is found** - simulating scenarios where ResourceSlices exist on nodes but weren't defined through NodeOverlay configuration. This addresses when other out of band components manage nodes, partial NodeOverlay coverage (only some instance types configured), and 3rd party DRA driver integration (GPU operators working independently). The driver falls back to DRAConfig CRD-based device configuration when no matching NodeOverlay is found, creating ResourceSlices that Karpenter must then discover and incorporate into future scheduling decisions. This ensures we correctly test that Karpenter successfully discovers ResourceSlices and schedules against them, even if they weren't defined on any NodeOverlays.

```yaml
apiVersion: test.karpenter.sh/v1alpha1
kind: DRAConfig
metadata:
  name: gpu-config  # User-chosen name
  namespace: karpenter
spec:
  driver: "test.karpenter.sh"  # Simulated driver name
  mappings:
  - name: "h100-nodes"
    nodeSelectorTerms:
    - matchExpressions:
      - key: node.kubernetes.io/instance-type
        operator: In
        values: ["g5.48xlarge"]
    resourceSlice:
      pool:
        name: gpu-pool
        resourceSliceCount: 1
      devices:
      - name: "nvidia-h100-0"
        attributes:
          memory: {stringValue: "80Gi"}
          compute-capability: {stringValue: "9.0"}
          device_class: {stringValue: "gpu"}
          vendor: {stringValue: "nvidia"}
  - name: "fpga-nodes"
    nodeSelectorTerms:
    - matchExpressions:
      - key: node.kubernetes.io/instance-type
        operator: In
        values: ["f1.2xlarge"]
    resourceSlice:
      pool:
        name: fpga-pool
        resourceSliceCount: 1
      devices:
      - name: "xilinx-u250-0"
        attributes:
          memory: {stringValue: "16Gi"}
          device_class: {stringValue: "fpga"}
          vendor: {stringValue: "xilinx"}

```

**How it works**:
1. **Test author defines DRAConfig CRD**: "g5.48xlarge KWOK nodes should have fake H100 GPUs when no NodeOverlay is found"
2. **Karpenter creates KWOK node**: Node with `instance-type: g5.48xlarge` is created
3. **Driver polling detects new node**: Within 30 seconds, driver reconciliation loop discovers the node
4. **No NodeOverlay match found**: Driver checks for NodeOverlay with embedded ResourceSlice objects, finds none, falls back to DRAConfig CRD
5. **Driver reads DRAConfig**: Gets `test.karpenter.sh` DRAConfig from `karpenter` namespace (checked during each 30s polling cycle)
6. **Driver creates ResourceSlices**: For each KWOK node matching the mapping's nodeSelectorTerms
7. **Scheduler sees configured devices**: ResourceSlices with fake devices become available for DRA pod scheduling
8. **Test validation**: Validates that the driver correctly provides DRA resources and enables successful pod scheduling

## Directory Structure
```
karpenter/
├── dra-kwok-driver/
│   ├── main.go                           # Driver entry point
│   └── pkg/
│       ├── apis/
│       │   ├── v1alpha1/                 # CRD Go types
│       │   │   ├── draconfig_types.go    # DRAConfig CRD types
│       │   │   └── zz_generated.deepcopy.go
│       │   └── crds/                     # CRD definitions
│       │       └── test.karpenter.sh_draconfigs.yaml
│       └── controllers/               
│           ├── resourceslice.go          # ResourceSlice lifecycle (manages all drivers)
│           └── resourceslice_test.go     # ResourceSlice controller tests
└── test/suites/dra/
    ├── suite_test.go                     # Test suite setup
    └── dra_kwok_test.go                  # DRA integration tests
```

**Architecture:**
1. `main.go` starts ResourceSlice controller with namespace
2. `resourceslice.go` polls nodes every 30 seconds, LISTs all DRAConfig CRDs, groups by driver, and creates ResourceSlices for each driver independently
3. `draconfig_types.go` defines CRD types with ResourceSliceTemplate.ToResourceSliceSpec() conversion method
4. **Multi-driver support:** Single controller manages multiple drivers dynamically (e.g., `gpu.nvidia.com`, `fpga.intel.com`)