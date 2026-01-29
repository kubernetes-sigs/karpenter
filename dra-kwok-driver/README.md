# DRA KWOK Driver

A Kubernetes Dynamic Resource Allocation (DRA) driver that creates ResourceSlices on Karpenter KWOK nodes. This enables comprehensive DRA integration testing without requiring actual device/resources.

## Architecture Overview

The DRA KWOK driver consists of 5 main components:

```
ConfigMap ──▶ ConfigMapController ──▶ Config Store ◀── ResourceSliceController ──▶ ResourceSlices
  (YAML)        (Parser/Watcher)       (Shared State)     (Node Event Handler)
```

The **Config Store** is a thread-safe shared data structure that decouples the controllers, avoiding circular dependencies while enabling both controllers to access the current configuration.

## Breakdown 

### **`main.go`** - Application Bootstrap
Sets up the controller-runtime manager and initializes both controllers with a shared config store.
- Creates Kubernetes client and controller-runtime manager
- Registers DRA ResourceSlice API types in the scheme
- Creates shared Config Store for thread-safe configuration access
- Initializes ConfigMapController and ResourceSliceController with the shared store
- Single manager for both controllers to share Kubernetes client and event system
- Sets up health checks and metrics endpoints
- Handles graceful shutdown

### **`pkg/config/types.go`** - Configuration Schema
Defines the complete data structure for ConfigMap configuration with validation.
**Key Structures:**
- **Config**: Top-level configuration with driver name and mappings array
- **Mapping**: Links node selectors to upstream ResourceSlice specifications
- **ResourceSliceSpec**: Uses upstream Kubernetes ResourceSlice spec directly for complete API coverage

### **`pkg/config/store.go`** - Shared Configuration Store
Thread-safe store for driver configuration that allows for ConfigMapController and ResourceSliceController to share the configuration instead of interacting with eachother.
- **Thread-safe access**: Uses `sync.Mutex` for concurrent read/write operations
- **API**: `Get()`, `Set()`, and `Clear()` methods for configuration management

### **`pkg/controllers/configmap.go`** - Configuration Management
Watches ConfigMap changes, parses YAML configuration, validates it, and stores it in the shared config store.
- Watches only the specific ConfigMap (`dra-kwok-configmap` in `karpenter` namespace)
- Parses `config.yaml` data from ConfigMap into Go structs
- Validates configuration structure and business rules
- Stores validated configuration in the shared Config Store
- Triggers optional callback to notify other components of configuration changes
- Handles ConfigMap deletion by clearing configuration from store

Single-threaded reconciliation prevents race conditions during configuration updates. Invalid configurations are logged but don't crash the driver.

### **`pkg/controllers/resourceslice.go`** - ResourceSlice Lifecycle
Watches Node events, detects KWOK nodes, reads configuration from the shared store, matches nodes against configuration, and manages ResourceSlice CRUD operations.
- **Node Filtering**: Only processes nodes with `kwok.x-k8s.io/node` annotation (Karpenter KWOK nodes)
- **Configuration Access**: Reads current configuration from the shared Config Store
- **Label Matching**: Uses Kubernetes label selectors to find configuration mappings for each node
- **ResourceSlice Creation**: Creates real Kubernetes ResourceSlice objects with device specifications from config
- **Automatic Cleanup**: Uses owner references so ResourceSlices are deleted when nodes are removed

## End-to-End Workflow

### **Initialization**
1. **main.go** creates controller-runtime manager with Kubernetes client
2. **main.go** creates shared Config Store instance
3. **ConfigMapController** and **ResourceSliceController** are initialized with the shared store
4. Both controllers register for event watching (ConfigMap and Node events respectively)
5. **ConfigMapController** attempts to load initial configuration from existing ConfigMap into the store
6. Manager starts watching Kubernetes API for ConfigMap and Node events

### **Configuration**
When ConfigMap is created or updated:
1. **ConfigMapController** receives ConfigMap event
2. Extracts `config.yaml` data and parses YAML into Go structs using upstream ResourceSlice spec
3. Validates configuration structure and rules
4. Stores validated configuration in the shared Config Store using `store.Set(config)`
5. Triggers callback to notify **ResourceSliceController** to reconcile all existing nodes
6. **ResourceSliceController** can now access the new configuration via `store.Get()`

### **Node Processing**
When Karpenter creates a KWOK node:
1. **ResourceSliceController** receives Node event
2. Checks for `kwok.x-k8s.io/node` annotation to identify KWOK nodes
3. Gets current configuration from the shared Config Store via `store.Get()`
4. Iterates through configuration mappings to find matching nodeSelector
5. If match found: Creates ResourceSlice
6. Only adds node-specific metadata (NodeName, owner references, labels) - all other fields come from config

### **Cleanup**
When KWOK node is deleted automatically removes ResourceSlices

## ConfigMap Examples

### **GPU Configuration**
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: dra-kwok-configmap
  namespace: karpenter
data:
  config.yaml: |
    driver: karpenter.sh/dra-kwok-driver
    mappings:
      - name: gpu-t4-mapping
        nodeSelectorTerms:
          - matchExpressions:
              - key: node.kubernetes.io/instance-type
                operator: In
                values: ["c-4x-amd64-linux"]
        resourceSlice:
          driver: karpenter.sh/dra-kwok-driver
          pool:
            name: t4-gpu-pool
            resourceSliceCount: 1
          devices:
            - name: nvidia-t4-0
              attributes:
                type:
                  String: nvidia-tesla-t4
                memory:
                  String: 16Gi
                compute_capability:
                  String: "7.5"
                cuda_cores:
                  String: "2560"
```

### **FPGA Configuration**
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: dra-kwok-configmap
  namespace: karpenter
data:
  config.yaml: |
    driver: karpenter.sh/dra-kwok-driver
    mappings:
      - name: fpga-mapping
        nodeSelectorTerms:
          - matchExpressions:
              - key: node.kubernetes.io/instance-type
                operator: In
                values: ["f1.2xlarge"]
        resourceSlice:
          driver: karpenter.sh/dra-kwok-driver
          pool:
            name: fpga-pool
            resourceSliceCount: 1
          devices:
            - name: xilinx-u250-0
              attributes:
                type:
                  String: xilinx-alveo-u250
                memory:
                  String: 64Gi
                dsp_slices:
                  String: "12288"
                interface:
                  String: pcie-gen3
```

### **Multiple Device Types**
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: dra-kwok-configmap
  namespace: karpenter
data:
  config.yaml: |
    driver: karpenter.sh/dra-kwok-driver
    mappings:
      # GPU devices
      - name: gpu-mapping
        nodeSelectorTerms:
          - matchExpressions:
              - key: accelerator-type
                operator: In
                values: ["gpu"]
        resourceSlice:
          driver: karpenter.sh/dra-kwok-driver
          pool:
            name: gpu-pool
            resourceSliceCount: 1
          devices:
            - name: nvidia-gpu-0
              attributes:
                type:
                  String: nvidia-tesla-v100
            - name: nvidia-gpu-1
              attributes:
                type:
                  String: nvidia-tesla-v100

      # FPGA devices on same nodes
      - name: fpga-mapping
        nodeSelectorTerms:
          - matchExpressions:
              - key: accelerator-type
                operator: In
                values: ["gpu"]  # Same selector - creates multiple ResourceSlices per node
        resourceSlice:
          driver: karpenter.sh/dra-kwok-driver
          pool:
            name: fpga-pool
            resourceSliceCount: 1
          devices:
            - name: fpga-0
              attributes:
                type:
                  String: xilinx-fpga
```

### **Cluster-Wide Resources**
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: dra-kwok-configmap
  namespace: karpenter
data:
  config.yaml: |
    driver: karpenter.sh/dra-kwok-driver
    mappings:
      - name: cluster-wide-resource
        nodeSelectorTerms:
          - matchExpressions:
              - key: has-shared-storage
                operator: In
                values: ["true"]
        resourceSlice:
          driver: karpenter.sh/dra-kwok-driver
          allNodes: true  # Available cluster-wide
          pool:
            name: shared-storage-pool
            resourceSliceCount: 3
          devices:
            - name: shared-nvme-0
              attributes:
                type:
                  String: shared-nvme
                capacity:
                  String: 1TB
```

## Test Coverage

### **Unit Tests** (`dra-kwok-driver/pkg/...`)

#### **`pkg/config/types_test.go`** - Configuration Validation
- **Config Validation**
  - Should return error for empty driver
  - Should return error for no mappings

#### **`pkg/controllers/configmap_test.go`** - ConfigMap Controller
- **parseConfigFromConfigMap**
  - Should parse valid YAML configuration
  - Should handle different config key names
  - Should return error for missing configuration keys
  - Should return error for invalid YAML

- **GetConfig**
  - Should return nil when no configuration is loaded
  - Should return current configuration after setting

- **Constructor**
  - Should create controller with correct parameters

#### **`pkg/controllers/resourceslice_test.go`** - ResourceSlice Controller
- **isKWOKNode**
  - Should identify KWOK nodes by Karpenter KWOK annotation
  - Should not identify non-KWOK nodes
  - Should not identify nodes with other KWOK-like labels

- **findMatchingMapping**
  - Should find matching mapping by single label
  - Should find matching mapping by multiple labels  
  - Should return nil for non-matching node
  - Should return first matching mapping when multiple match

- **Reconcile**
  - Should skip non-KWOK nodes
  - Should handle missing configuration gracefully
  - Should create ResourceSlices for matching KWOK node
  - Should clean up ResourceSlices when node is deleted
  - Should clean up ResourceSlices when no mapping matches

- **GetResourceSlicesForNode**
  - Should return empty slice when no ResourceSlices exist
  - Should return ResourceSlices for specified node

### **Integration Tests** (`test/suites/dra/...`)

#### **`configmap_integration_test.go`** - ConfigMap CRUD Operations
- **DRA ConfigMap Integration**
  - Should create and read ConfigMap successfully
  - Should update ConfigMap configuration
  - Should handle ConfigMap deletion

#### **`dra_kwok_test.go`** - E2E DRA Integration Tests
**Context: GPU Configuration**
- Should create and manage ResourceSlices based on ConfigMap
- Should handle ConfigMap updates dynamically

**Context: Advanced Device Configuration**
- Should support FPGA device types
- Should support multiple ResourceSlices for single instance type

**Context: Configuration Validation**
- Should handle invalid ConfigMap gracefully
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

# Create karpenter namespace for DRA ConfigMaps (tests expect this namespace)
kubectl create namespace karpenter

# Build and start DRA KWOK driver with proper logging 
# pkill -f dra-kwok-driver do this if you had driver running before 
cd dra-kwok-driver
go build -o dra-kwok-driver main.go
./dra-kwok-driver

# Run DRA integration tests seperate terminal under karpenter/
make e2etests TEST_SUITE=dra
```