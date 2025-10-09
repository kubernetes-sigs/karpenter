# DRA KWOK Driver

A Kubernetes Dynamic Resource Allocation (DRA) driver that creates ResourceSlices on Karpenter KWOK nodes. This enables comprehensive DRA integration testing without requiring actual device/resources.

## Architecture Overview

The DRA KWOK driver consists of 4 main components:

```
ConfigMap ──▶ ConfigMapController ──▶ ResourceSliceController ──▶ ResourceSlices
  (YAML)        (Parser/Watcher)        (Node Event Handler)
```

## Breakdown 

### **`main.go`** - Application Bootstrap
Sets up the controller-runtime manager and initializes both controllers.
- Creates Kubernetes client and controller-runtime manager
- Registers DRA ResourceSlice API types in the scheme  
- Initializes ConfigMapController and ResourceSliceController
- Single manager for both controllers to share Kubernetes client and event system
- Sets up health checks and metrics endpoints
- Handles graceful shutdown

### **`pkg/config/types.go`** - Configuration Schema
Defines the complete data structure for ConfigMap configuration with validation.
**Key Structures:**
- **Config**: Top-level configuration with driver name and mappings array
- **Mapping**: Links node selectors to device configurations  
- **ResourceSliceConfig**: Defines devices to create for matching nodes
- **DeviceConfig**: Individual device specification with attributes

### **`pkg/controllers/configmap.go`** - Configuration Management
Watches ConfigMap changes, parses YAML configuration, validates it, and makes it available to other controllers.
- Watches only the specific ConfigMap (`dra-kwok-configmap` in `karpenter` namespace)
- Parses `config.yaml` data from ConfigMap into Go structs
- Validates configuration structure and business rules
- Stores validated configuration and notifies ResourceSliceController of changes
- Handles ConfigMap deletion by clearing configuration
  
 Single-threaded reconciliation prevents race conditions during configuration updates. Invalid configurations are logged but don't crash the driver.

### **`pkg/controllers/resourceslice.go`** - ResourceSlice Lifecycle
Watches Node events, detects KWOK nodes, matches them against configuration, and manages ResourceSlice CRUD operations.
- **Node Filtering**: Only processes nodes with `kwok.x-k8s.io/node` annotation (Karpenter KWOK nodes)
- **Label Matching**: Uses Kubernetes label selectors to find configuration mappings for each node
- **ResourceSlice Creation**: Creates real Kubernetes ResourceSlice objects with device specifications from config
- **Automatic Cleanup**: Uses owner references so ResourceSlices are deleted when nodes are removed

## End-to-End Workflow

### **Initialization**
1. **main.go** creates controller-runtime manager with Kubernetes client
2. **ConfigMapController** and **ResourceSliceController** register for event watching
3. **ConfigMapController** attempts to load initial configuration from existing ConfigMap
4. Manager starts watching Kubernetes API for ConfigMap and Node events

### **Configuration** 
When ConfigMap is created or updated:
1. **ConfigMapController** receives ConfigMap event
2. Extracts `config.yaml` data and parses YAML into Go structs
3. Validates configuration structure and rules  
4. Stores validated configuration internally
5. **ResourceSliceController** can now access configuration via shared reference

### **Node Processing**
When Karpenter creates a KWOK node:
1. **ResourceSliceController** receives Node event
2. Checks for `kwok.x-k8s.io/node` annotation to identify KWOK nodes
3. Gets current configuration from **ConfigMapController**
4. Iterates through configuration mappings to find matching nodeSelector
5. If match found: Creates ResourceSlice with devices from mapping configuration
6. ResourceSlice includes proper metadata, owner references, and device specifications

### **Cleanup**
When KWOK node is deleted automatically removes ResourceSlices

## Test Coverage

### **Unit Tests** (`dra-kwok-driver/pkg/...`)

#### **`pkg/config/types_test.go`** - Configuration Validation
- **Config Validation**
  - Should validate a complete config
  - Should return error for empty driver
  - Should return error for no mappings

- **DeviceConfig** 
  - Should support device configuration with attributes
  - Should validate device name is required
  - Should validate device count is positive

- **Mapping**
  - Should validate mapping correctly
  - Should return error for empty name
  - Should return error for empty node selector

- **ResourceSliceConfig**
  - Should return error for no devices

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
- **GPU Configuration**
  - Should create and manage ResourceSlices based on ConfigMap
  - Should handle ConfigMap updates dynamically

- **FPGA Configuration**
  - Should support FPGA device types

- **Configuration Validation**
  - Should handle invalid ConfigMap gracefully

- **DRA Pod Scheduling in KWOK Environment**
  - Should ignore DRA pods when Karpenter DRA support is disabled (current behavior)
  - Should prepare DRA testing infrastructure for future Karpenter DRA tests

## Running the Tests

### **Unit Tests** (No cluster required)
```bash
# Run DRA KWOK driver unit tests
go test ./dra-kwok-driver/pkg/... -v
# Run specific tests
go test ./dra-kwok-driver/pkg/config/... -v
go test ./dra-kwok-driver/pkg/controllers/... -v
```

### **Integration Tests** (Requires Kubernetes cluster) IN PROGRESS

<!-- ```bash
kind create cluster --config kind-config.yaml --name <cluster-name>
export KWOK_REPO=kind.local
export KIND_CLUSTER_NAME=<cluster-name> make install-kwok  
make apply
kubectl taint nodes <cluster-name>-control-plane CriticalAddonsOnly=true:NoSchedule
kubectl taint nodes <cluster-name>-worker CriticalAddonsOnly=true:NoSchedule  
kubectl taint nodes <cluster-name>-worker2 CriticalAddonsOnly=true:NoSchedulecd dra-kwok-driver
cd dra-kwok-driver
go build -o dra-kwok-driver main.go
./dra-kwok-driver &
cd ..
make e2etests TEST_SUITE=dra
``` -->

```bash
# Create Kind cluster with Kubernetes 1.34 (required for DRA v1 GA)
kind create cluster --image kindest/node:v1.34.0 --name labelfixed

#tells build tools where to store images and which cluster to use
export KWOK_REPO=kind.local
export KIND_CLUSTER_NAME=labelfixed 

# Install KWOK infra
make install-kwok  

# Build Karpenter image and load into Kind cluster
make build-with-kind

# Apply CRDs first
kubectl apply -f kwok/charts/crds

# Deploy Karpenter with KWOK provider using helm directly (disable ServiceMonitor to avoid CRD dependency issue)
helm upgrade --install karpenter kwok/charts --namespace kube-system --skip-crds \
  --set logLevel=debug \
  --set controller.resources.requests.cpu=1 \
  --set controller.resources.requests.memory=1Gi \
  --set controller.resources.limits.cpu=1 \
  --set controller.resources.limits.memory=1Gi \
  --set settings.featureGates.nodeRepair=true \
  --set settings.featureGates.staticCapacity=true \
  --set controller.image.repository=kind.local/karpenter-kwok \
  --set controller.image.tag=latest \
  --set serviceMonitor.enabled=false \
  --set-string 'controller.env[0].name=ENABLE_PROFILING' \
  --set-string 'controller.env[0].value=true'

# Taint control plane node to prevent scheduling (Karpenter deploys to kube-system, not separate karpenter namespace)
kubectl taint nodes labelfixed-control-plane CriticalAddonsOnly=true:NoSchedule

# Create karpenter namespace for DRA ConfigMaps (tests expect this namespace)
kubectl create namespace karpenter

# Build and start DRA KWOK driver
cd dra-kwok-driver
go build -o dra-kwok-driver main.go
./dra-kwok-driver &
cd ..

# Run DRA integration tests
make e2etests TEST_SUITE=dra
```