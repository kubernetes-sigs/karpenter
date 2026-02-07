# DRA KWOK Driver

A Kubernetes Dynamic Resource Allocation (DRA) driver that creates ResourceSlices on Karpenter KWOK nodes. This enables comprehensive DRA integration testing without requiring actual device/resources.

## Architecture Overview

The DRA KWOK driver uses a **single-controller architecture** that reads THE DRADriverConfig CRD directly from the Kubernetes API server:

```
┌─────────────────────────────────────────────────────┐
│              Kubernetes API Server                  │
│  ┌────────────────────────────────────────────────┐ │
│  │    DRADriverConfig CRD (name = driver name)    │ │
│  │    - karpenter/karpenter.sh                    │ │
│  │    Nodes (KWOK nodes created by Karpenter)     │ │
│  │    ResourceSlices (created by controller)      │ │
│  └────────────────────────────────────────────────┘ │
└──────────────────────┬──────────────────────────────┘
                       │
                       ▼
          ┌────────────────────────────┐
          │  ResourceSlice Controller  │
          │                            │
          │  Every 30 seconds:         │
          │  1. GET karpenter/karpenter.sh CRD   │
          │  2. Convert to config      │
          │  3. List KWOK nodes        │
          │  4. Create ResourceSlices  │
          │  5. Cleanup orphaned       │
          └────────────────────────────┘
```

**Design A: One CRD Per Driver**
- CRD name must match driver name (`karpenter.sh`)
- Single DRADriverConfig contains all device mappings (GPU, FPGA, etc.)
- Controller reads CRD directly (no merging, no config store)
- Kubernetes API server is the single source of truth

**CRD-based configuration** provides API server validation and structured types without YAML parsing.

## Breakdown 

### **`main.go`** - Application Bootstrap
Sets up the controller-runtime manager and initializes the ResourceSlice controller.
- Creates Kubernetes client and controller-runtime manager
- Registers DRA ResourceSlice and DRADriverConfig CRD API types in the scheme
- Initializes ResourceSliceController with Kubernetes client and driver name
- Sets up health checks and metrics endpoints (ports 8082/8083)
- Handles graceful shutdown

### **`pkg/config/types.go`** - Configuration Schema
Defines the internal data structure for configuration after CRD conversion.
**Key Structures:**
- **Config**: Top-level configuration with driver name and mappings array
- **Mapping**: Links node selectors to upstream ResourceSlice specifications
- **ResourceSliceSpec**: Uses upstream Kubernetes ResourceSlice spec directly for complete API coverage

This internal format is used by the ResourceSlice controller after converting from THE DRADriverConfig CRD.

### **`pkg/controllers/resourceslice.go`** - ResourceSlice Lifecycle
Periodically polls nodes and THE DRADriverConfig CRD (every 30 seconds), reads configuration directly from the API server, matches nodes against mappings, and manages ResourceSlice CRUD operations.
- **Polling Loop**: Runs every 30 seconds to reconcile all nodes and configuration, ensuring eventual consistency
- **CRD Reading**: GETs THE single DRADriverConfig CRD from API server (`karpenter/karpenter.sh`)
- **Configuration Conversion**: Converts CRD format to internal config (no merging needed)
- **Node Filtering**: Only processes nodes with `kwok.x-k8s.io/node` annotation (Karpenter KWOK nodes)
- **Label Matching**: Uses Kubernetes label selectors to find configuration mappings for each node
- **ResourceSlice Management**: Creates, updates, or deletes ResourceSlices to match desired state
- **Error Handling**: Continues processing other nodes if one fails; failed nodes are retried in the next cycle
- **Cleanup**: Removes ResourceSlices that shouldn't exist (orphaned slices, deleted nodes, deleted CRD)

## End-to-End Workflow

### **Initialization**
1. **main.go** creates controller-runtime manager with Kubernetes client
2. **ResourceSliceController** is initialized with driver name (`karpenter.sh`) and namespace (`karpenter`)
3. **ResourceSliceController** registers and starts polling loop (30-second interval)
4. Manager starts, controller begins reconciling nodes and DRADriverConfig CRD

### **Configuration**
When DRADriverConfig CRD is created or updated:
1. **User creates THE DRADriverConfig** with name matching driver name (`karpenter.sh`)
2. **ResourceSliceController** detects it in next polling cycle (within 30 seconds)
3. GETs `karpenter/karpenter.sh` DRADriverConfig from API server
4. Converts CRD to internal config format (already validated by API server)
5. Uses config to reconcile nodes and create ResourceSlices

### **Node Processing (Polling Loop)**
Every 30 seconds, the ResourceSliceController:
1. GETs THE DRADriverConfig CRD (`karpenter/karpenter.sh`)
2. If not found: cleanup all ResourceSlices and return
3. If found: convert to internal config
4. Lists all nodes in the cluster
5. For each node:
   - Checks for `kwok.x-k8s.io/node` annotation to identify KWOK nodes
   - Iterates through configuration mappings to find matching nodeSelectors
   - If match found: Creates or updates ResourceSlice with device specifications
   - If no match: Ensures no ResourceSlices exist for this node
6. Cleans up any orphaned ResourceSlices (nodes deleted, CRD deleted, etc.)
7. Logs summary of reconciliation (nodes processed, errors encountered)

This polling approach ensures eventual consistency - any changes to nodes or configuration are reconciled within 30 seconds.

### **Cleanup**
ResourceSlices are automatically cleaned up when:
- KWOK node is deleted (detected in next polling cycle)
- Node no longer matches any configuration mapping (labels changed)
- DRADriverConfig CRD is deleted or configuration is cleared
- Mapping is removed from DRADriverConfig CRD

## DRADriverConfig CRD Examples

**Design A: One CRD per driver - CRD name must match driver name**

### **Basic: Single Device Type**
```yaml
apiVersion: karpenter.sh/v1alpha1
kind: DRADriverConfig
metadata:
  name: karpenter.sh  # MUST match driver name
  namespace: karpenter
spec:
  driver: karpenter.sh
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

### **Multiple Device Types on Different Nodes**
```yaml
apiVersion: karpenter.sh/v1alpha1
kind: DRADriverConfig
metadata:
  name: karpenter.sh
  namespace: karpenter
spec:
  driver: karpenter.sh
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
apiVersion: karpenter.sh/v1alpha1
kind: DRADriverConfig
metadata:
  name: karpenter.sh
  namespace: karpenter
spec:
  driver: karpenter.sh
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

### **Checking CRD Status**
```bash
# List all DRADriverConfig CRDs
kubectl get dradriverconfig

# Get configuration details
kubectl get dradriverconfig karpenter.sh -o yaml

# Watch for ResourceSlices being created
kubectl get resourceslices -l kwok.x-k8s.io/managed-by=dra-kwok-driver -w
```

## Test Coverage

### **Unit Tests** (`dra-kwok-driver/pkg/...`)

#### **`pkg/config/types_test.go`** - Configuration Validation
- **Config Validation**
  - Should return error for empty driver
  - Should return error for no mappings

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
- Should create and manage ResourceSlices based on DRADriverConfig CRD
- Should handle DRADriverConfig CRD updates dynamically

**Context: Advanced Device Configuration**
- Should support FPGA device types
- Should support multiple ResourceSlices for single instance type

**Context: Configuration Validation**
- Should validate CRD schema at creation time (API server validation)
- Should ignore DRA pods when IgnoreDRARequests is enabled (default behavior)
- Should prepare DRA testing infrastructure for future Karpenter DRA implementation

## Running the Tests

### **Unit Tests** (No cluster required)
```bash
# Run DRA KWOK driver unit tests
go test ./dra-kwok-driver/pkg/... -v
# Run specific tests
go test ./dra-kwok-driver/pkg/config/... -v
go test ./dra-kwok-driver/pkg/controllers/... -v
```

### **Integration Tests**

```bash
# Create Kind cluster with Kubernetes 1.34 (required for DRA v1 GA)
kind create cluster --image kindest/node:v1.34.0 --name <your-cluster-name>

#tells build tools where to store images and which cluster to use
export KWOK_REPO=kind.local
export KIND_CLUSTER_NAME=<your-cluster-name> 

# Install KWOK infra
make install-kwok  

# Build image first
make build-with-kind

# Extract the actual image name from kind cluster
make get-kind-image

# Deploy Karpenter with DRA-friendly settings (uses extracted image variables from get-kind-image)
make dra-apply-with-kind

# Check Karpenter controller logs to ensure it started properly
kubectl logs -l app.kubernetes.io/name=karpenter -n kube-system --tail=50

# Taint control plane node to prevent scheduling (Karpenter deploys to kube-system, not separate karpenter namespace)
kubectl taint nodes <your-cluster-name>-control-plane CriticalAddonsOnly=true:NoSchedule

# Apply DRADriverConfig CRD to cluster
kubectl apply -f dra-kwok-driver/pkg/apis/crds/karpenter.sh_dradriverconfigs.yaml

# Build and start DRA KWOK driver with proper logging
# pkill -f dra-kwok-driver do this if you had driver running before
cd dra-kwok-driver
go build -o dra-kwok-driver main.go
./dra-kwok-driver

# Run DRA integration tests seperate terminal under karpenter/
make e2etests TEST_SUITE=dra
```