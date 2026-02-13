# DRA KWOK Driver

A Kubernetes Dynamic Resource Allocation (DRA) driver that creates ResourceSlices on Karpenter KWOK nodes. Supports simulating multiple DRA drivers (e.g., gpu.nvidia.com, fpga.intel.com) simultaneously. This enables comprehensive DRA integration testing without requiring actual devices/resources.

## Architecture Overview

The DRA KWOK driver uses a **single-controller architecture** that manages multiple drivers dynamically:

```
┌─────────────────────────────────────────────────────┐
│              Kubernetes API Server                  │
│  ┌────────────────────────────────────────────────┐ │
│  │    DRAConfig CRDs (multiple drivers)           │ │
│  │    KWOK nodes created by Karpenter             │ │
│  │    ResourceSlices (created by controller)      │ │
│  └────────────────────────────────────────────────┘ │
└──────────────────────┬──────────────────────────────┘
                       │
                       ▼
        ┌────────────────────────────┐
        │  ResourceSlice Controller  │
        │                            │
        │  Every 30 seconds:         │
        │  1. LIST all DRAConfigs    │
        │  2. Group by driver name   │
        │  3. List KWOK nodes        │
        │  4. Create ResourceSlices  │
        │  5. Cleanup orphaned       │
        └────────────────────────────┘
```

**Multi-Driver Support**
- One DRAConfig per driver (e.g., `gpu-config` for `gpu.nvidia.com`)
- Single controller discovers and manages all drivers automatically
- Dynamic driver discovery - no restart needed when adding/removing drivers
- Warns when multiple DRAConfigs use the same driver (uses first one alphabetically)

**CRD-based configuration** provides API server validation and structured types without YAML parsing.

## Breakdown 

### **`main.go`** - Application Bootstrap
Sets up the controller-runtime manager and initializes the ResourceSlice controller.
- Creates Kubernetes client and controller-runtime manager
- Registers DRA ResourceSlice and DRAConfig CRD API types in the scheme
- Initializes ResourceSliceController with Kubernetes client and namespace (no driver name - manages all drivers)
- Sets up health checks and metrics endpoints (ports 8082/8083)
- Handles graceful shutdown

### **`pkg/apis/v1alpha1/draconfig_types.go`** - CRD Types
Defines the DRAConfig CRD types used for configuration.
**Key Structures:**
- **DRAConfig**: CRD resource containing driver name and device mappings
- **Mapping**: Links node selectors to ResourceSlice templates
- **ResourceSliceTemplate**: Template for creating ResourceSlices, with ToResourceSliceSpec() conversion method

### **`pkg/controllers/resourceslice.go`** - ResourceSlice Lifecycle
Periodically polls nodes and DRAConfigs (every 30 seconds), discovers drivers dynamically, matches nodes against mappings, and manages ResourceSlice CRUD operations.
- **Polling Loop**: Runs every 30 seconds to reconcile all nodes and configurations, ensuring eventual consistency
- **Multi-Driver Discovery**: LISTs all DRAConfig CRDs, groups by driver name, warns on duplicates
- **Node Filtering**: Only processes nodes with `kwok.x-k8s.io/node` annotation (Karpenter KWOK nodes)
- **Label Matching**: Uses Kubernetes label selectors to find configuration mappings for each node
- **ResourceSlice Management**: Creates, updates, or deletes ResourceSlices to match desired state using ToResourceSliceSpec()
- **Error Handling**: Continues processing other nodes/drivers if one fails; failed ones are retried in the next cycle
- **Cleanup**: Removes ResourceSlices that shouldn't exist (orphaned slices, deleted nodes, deleted CRD)

## End-to-End Workflow

### **Initialization**
1. **main.go** creates controller-runtime manager with Kubernetes client
2. **ResourceSliceController** is initialized with namespace (`karpenter`)
3. **ResourceSliceController** registers and starts polling loop (30-second interval)
4. Manager starts, controller begins discovering drivers and reconciling nodes

### **Configuration**
When DRAConfig CRDs are created or updated:
1. **User creates DRAConfigs** with user-chosen names (e.g., `gpu-config`, `fpga-config`)
2. **Each DRAConfig specifies its driver** in `spec.driver` field (e.g., `gpu.nvidia.com`)
3. **ResourceSliceController** detects them in next polling cycle (within 30 seconds)
4. LISTs all DRAConfigs in `karpenter` namespace
5. Groups configs by driver name (warns if duplicates found)
6. Reconciles nodes and creates ResourceSlices for each driver

### **Node Processing (Polling Loop)**
Every 30 seconds, the ResourceSliceController:
1. LISTs all DRAConfig CRDs in the namespace
2. If none found: cleanup all ResourceSlices and return
3. Groups DRAConfigs by driver name (one config per driver, warns on duplicates)
4. Lists all nodes in the cluster
5. For each driver:
   - For each KWOK node (has `kwok.x-k8s.io/node` annotation):
     - Iterates through driver's mappings to find matching nodeSelectors
     - If match found: Uses ResourceSliceTemplate.ToResourceSliceSpec() to create or update ResourceSlice
     - ResourceSlice naming: `<driver-sanitized>-<node>-<mapping>`
       - Example: `gpu-nvidia-com-node1-h100-mapping`
6. Cleans up any orphaned ResourceSlices (nodes deleted, configs deleted, etc.)
7. Logs summary of reconciliation (drivers, nodes processed, errors encountered)

This polling approach ensures eventual consistency - any changes to nodes or configuration are reconciled within 30 seconds.

### **Cleanup**
ResourceSlices are automatically cleaned up when:
- KWOK node is deleted (detected in next polling cycle)
- Node no longer matches any configuration mapping (labels changed)
- DRAConfig CRD is deleted
- Mapping is removed from DRAConfig CRD
- Driver is changed in DRAConfig (old driver's slices cleaned up)

## DRAConfig CRD Examples

### **Multi-Driver Setup (Recommended)**
Simulate multiple DRA drivers simultaneously - GPU and FPGA drivers on the same nodes:

```yaml
# GPU driver simulation
apiVersion: test.karpenter.sh/v1alpha1
kind: DRAConfig
metadata:
  name: gpu-config  # User-chosen name
  namespace: karpenter
spec:
  driver: gpu.nvidia.com  # Simulated driver name
  mappings:
    - name: h100-mapping
      nodeSelectorTerms:
        - matchExpressions:
            - key: node.kubernetes.io/instance-type
              operator: In
              values: ["g5.xlarge"]
      resourceSlice:
        pool:
          name: nvidia-gpu-pool
          resourceSliceCount: 1
        devices:
          - name: h100-0
            attributes:
              type: {stringValue: "nvidia-h100"}
              memory: {stringValue: "80Gi"}

---
# FPGA driver simulation
apiVersion: test.karpenter.sh/v1alpha1
kind: DRAConfig
metadata:
  name: fpga-config
  namespace: karpenter
spec:
  driver: fpga.intel.com  # Different driver
  mappings:
    - name: arria-mapping
      nodeSelectorTerms:
        - matchExpressions:
            - key: node.kubernetes.io/instance-type
              operator: In
              values: ["g5.xlarge"]  # Same instance type as above
      resourceSlice:
        pool:
          name: intel-fpga-pool
          resourceSliceCount: 1
        devices:
          - name: arria-0
            attributes:
              type: {stringValue: "intel-arria10"}
```

**Result:** A single `g5.xlarge` node will have ResourceSlices from both drivers:
- `gpu-nvidia-com-node1-h100-mapping` (driver: `gpu.nvidia.com`)
- `fpga-intel-com-node1-arria-mapping` (driver: `fpga.intel.com`)

### **Basic: Single Driver, Single Device Type**
```yaml
apiVersion: test.karpenter.sh/v1alpha1
kind: DRAConfig
metadata:
  name: my-config
  namespace: karpenter
spec:
  driver: test.karpenter.sh
  mappings:
    - name: gpu-mapping
      nodeSelectorTerms:
        - matchExpressions:
            - key: node.kubernetes.io/instance-type
              operator: In
              values: ["g5.xlarge"]
      resourceSlice:
        pool:
          name: gpu-pool
          resourceSliceCount: 1
        devices:
          - name: nvidia-a10g-0
            attributes:
              type: {stringValue: "nvidia-a10g"}
              memory: {stringValue: "24Gi"}
```

### **Single Driver: Multiple Device Types on Different Nodes**
```yaml
apiVersion: test.karpenter.sh/v1alpha1
kind: DRAConfig
metadata:
  name: device-config
  namespace: karpenter
spec:
  driver: test.karpenter.sh
  mappings:
    # GPUs on GPU instance types
    - name: gpu-mapping
      nodeSelectorTerms:
        - matchExpressions:
            - key: node.kubernetes.io/instance-type
              operator: In
              values: ["g5.xlarge", "g5.2xlarge"]
      resourceSlice:
        pool:
          name: gpu-pool
          resourceSliceCount: 1
        devices:
          - name: nvidia-a10g-0
            attributes:
              type: {stringValue: "nvidia-a10g"}

    # FPGAs on FPGA instance types
    - name: fpga-mapping
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
          - name: xilinx-fpga-0
            attributes:
              type: {stringValue: "xilinx-fpga"}
```

### **Multiple ResourceSlices Per Node**
Same node matches multiple mappings → creates multiple ResourceSlices per node.
```yaml
apiVersion: test.karpenter.sh/v1alpha1
kind: DRAConfig
metadata:
  name: test.karpenter.sh
  namespace: karpenter
spec:
  driver: test.karpenter.sh
  mappings:
    # GPU mapping
    - name: gpu-mapping
      nodeSelectorTerms:
        - matchExpressions:
            - key: accelerator-type
              operator: In
              values: ["mixed"]
      resourceSlice:
        pool:
          name: gpu-pool
          resourceSliceCount: 1
        devices:
          - name: nvidia-gpu-0
            attributes:
              type: {stringValue: "nvidia-t4"}

    # FPGA mapping - same selector!
    - name: fpga-mapping
      nodeSelectorTerms:
        - matchExpressions:
            - key: accelerator-type
              operator: In
              values: ["mixed"]  # Matches same nodes as above
      resourceSlice:
        pool:
          name: fpga-pool
          resourceSliceCount: 1
        devices:
          - name: fpga-0
            attributes:
              type: {stringValue: "intel-fpga"}
```

## Test Coverage

### **Unit Tests** (`dra-kwok-driver/pkg/...`)

#### **`pkg/controllers/resourceslice_test.go`** - ResourceSlice Controller
- **isKWOKNode**
  - Should identify KWOK nodes by Karpenter KWOK annotation
  - Should not identify non-KWOK nodes
  - Should not identify nodes with other KWOK-like labels

- **findMatchingMappings**
  - Should find single matching mapping by node label
  - Should find no mappings for non-matching nodes
  - Should find multiple mappings when node matches multiple selectors
  - Should handle OR logic with multiple NodeSelectorTerms

- **reconcileAllNodes**
  - Should create ResourceSlices for matching nodes
  - Should not create ResourceSlices for non-matching nodes
  - Should skip non-KWOK nodes
  - Should clean up orphaned ResourceSlices when node is deleted
  - Should clean up all ResourceSlices when configuration is cleared
  - Should handle errors gracefully and continue processing other nodes
  - Should update existing ResourceSlices when configuration changes

### **Integration Tests** (`test/suites/dra/...`)

#### **`dra_kwok_test.go`** - E2E DRA Integration Tests
**Context: GPU Configuration**
- Should create and manage ResourceSlices based on DRAConfig CRD
- Should handle DRAConfig CRD updates dynamically

**Context: Advanced Device Configuration**
- Should support FPGA device types
- Should support multiple ResourceSlices for single instance type

**Context: Configuration Validation**
- Should validate CRD schema at creation time (API server validation)
- Should ignore DRA pods when IgnoreDRARequests is enabled (default behavior)
- Should prepare DRA testing infrastructure for future Karpenter DRA implementation

## Running the Tests

### Unit Tests
```bash
# Run all DRA KWOK driver unit tests
go test ./dra-kwok-driver/pkg/... -v

# Run controller tests specifically
go test ./dra-kwok-driver/pkg/controllers/... -v
```

**Coverage:**
- Node selector matching logic
- KWOK node detection (annotation checking)
- ResourceSlice creation/update logic
- Configuration validation

---

### E2E Integration Tests

#### **Quick Start**
```bash
make setup-kind-dra     # Creates cluster, installs KWOK, deploys Karpenter
make e2etest-dra        # Runs full test suite (does all steps below automatically)
```

#### **What `make e2etest-dra` Does:**

1. **Installs DRAConfig CRD** to cluster
   ```bash
   kubectl apply -f dra-kwok-driver/pkg/apis/crds/test.karpenter.sh_draconfigs.yaml
   ```

2. **Cleans up leftover resources** from previous runs
   ```bash
   kubectl delete draconfigs --all -n karpenter
   kubectl delete resourceslices --all
   pkill -f dra-kwok-driver  # Kills running driver binary
   ```

3. **Builds fresh DRA driver binary**
   ```bash
   cd dra-kwok-driver && go build -o dra-kwok-driver main.go
   ```

4. **Starts DRA driver in background**
   ```bash
   ./dra-kwok-driver > /tmp/dra-driver.log 2>&1 &
   ```
   - Polls for nodes every 30 seconds
   - Creates ResourceSlices based on DRAConfig
   - Logs to `/tmp/dra-driver.log`

5. **Runs Ginkgo test suite**
   ```bash
   TEST_SUITE=dra make e2etests
   ```
   - Tests in `test/suites/dra/dra_kwok_test.go`
   - Creates nodes, DRAConfigs, ResourceSlices, Pods

6. **Cleans up after tests**
   ```bash
   kubectl delete draconfigs --all
   kubectl delete resourceslices --all
   kill <dra-driver-pid>
   ```

**Coverage:**
- GPU device configuration and ResourceSlice creation
- Dynamic DRAConfig updates
- FPGA device types
- Multiple ResourceSlices per node (multiple mappings)
- Invalid config rejection (API server validation)
- DRA pod scheduling (ignored by default)
- DRA infrastructure readiness for future Karpenter integration
- DRAConfig CRD status fields