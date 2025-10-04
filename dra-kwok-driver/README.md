# DRA KWOK Driver - Architecture & Workflow

The DRA KWOK driver is a **mock Dynamic Resource Allocation driver** for testing Karpenter's DRA integration with simulated hardware resources (GPUs, FPGAs, accelerators) on KWOK nodes.

## Overview

The DRA KWOK driver creates **fake ResourceSlices** on KWOK nodes based on **ConfigMap-driven rules**, enabling end-to-end testing of DRA functionality without real hardware. It bridges YAML configuration with Kubernetes DRA APIs through a two-layer architecture.

## DRA KWOK Driver Workflow

### Driver Initialization (`main.go`)

1. **Scheme Setup**: Registers Kubernetes core and DRA v1 API types
2. **Controller Creation**:
   - `ConfigMapController`: Watches `dra-kwok-configmap` in `karpenter` namespace
   - `ResourceSliceController`: Watches Node events with driver name `karpenter.sh/dra-kwok-driver`
3. **Registration**: Both controllers registered with controller-runtime manager
4. **Bootstrap**: Calls `LoadInitialConfig()` to load existing ConfigMap if present

### Workflow

**ConfigMapController sees ConfigMap change:** (`pkg/controllers/configmap.go`)
- Uses event filtering to only watch specific ConfigMap (`dra-kwok-configmap` in karpenter namespace)
- Parses YAML from the `config.yaml` key in the ConfigMap data
- Unmarshals YAML into `config.Config` struct (`pkg/config/types.go`)
- Validates configuration structure via `cfg.Validate()` (`pkg/config/types.go`)
- Stores parsed config internally

**When Node Appears/Changes:** (`pkg/controllers/resourceslice.go`)
- ResourceSliceController watches all Node events
- Checks if it's a KWOK node via labels/annotations (`kwok.x-k8s.io/provider=kwok`, `type=kwok`, or `kwok.x-k8s.io/node`)
- Gets current config from ConfigMapController via `GetConfig()` (`pkg/controllers/configmap.go`)
- Matches node labels against each mapping's nodeSelector using label selectors (`pkg/config/types.go`)
- If match found: Creates/updates ResourceSlice(s) with specified devices and attributes
- If no match or node deleted: Cleans up existing ResourceSlices for that node

**ResourceSlice Management:** (`pkg/controllers/resourceslice.go`)
- Creates one ResourceSlice per device type in matching mapping
- Each ResourceSlice owned by the Node (for automatic cleanup)
- Devices get generated names (`deviceName-0`, `deviceName-1`, etc.)
- Device attributes converted from config map to QualifiedName keys with StringValue (`pkg/config/types.go`)
- Uses labels for tracking (`kwok.x-k8s.io/managed-by=dra-kwok-driver`, `kwok.x-k8s.io/node=nodeName`)

### Phase 4: Device Generation & ResourceSlice Creation

**Single ResourceSlice Per Node Architecture:**
```yaml
# One ResourceSlice contains ALL device types for a node
ResourceSlice: kwok-node-1-devices
├── Device: nvidia-t4-0-0
├── Device: nvidia-t4-0-1  
├── Device: nvidia-v100-0-0
└── Device: xilinx-fpga-0-0
```

**Device Generation Process:**
1. **Resource Pool Creation**: Single ResourceSlice named `{nodeName}-devices` with pool `{nodeName}-device-pool`
2. **Device Aggregation**: Collects ALL devices from ALL `DeviceConfig` entries in matching mapping
3. **Instance Generation**: For each `DeviceConfig`, creates `Count` device instances with names `{deviceConfig.Name}-{index}`
4. **Attribute Conversion**: Transforms simple `map[string]string` to complex `map[QualifiedName]DeviceAttribute` with `StringValue` pointers
5. **Ownership Setup**: ResourceSlice owned by Node for automatic cleanup on node deletion
6. **Labels Applied**: `kwok.x-k8s.io/managed-by=dra-kwok-driver` and `kwok.x-k8s.io/node={nodeName}`
7. **Kubernetes API**: Creates new or updates existing ResourceSlice via controller-runtime client

## Configuration Structure (`pkg/config/types.go`)

### Type Hierarchy & Purpose
```go
Config                    // Root configuration container
├── Driver string         // DRA driver name (karpenter.sh/dra-kwok-driver) 
└── Mappings []Mapping    // Array of node→device mapping rules

Mapping                   // Conditional device allocation rule
├── Name string           // Human-readable identifier
├── NodeSelector          // Kubernetes label selector for node matching
└── ResourceSlice         // Device template configuration

ResourceSliceConfig       // Template for ResourceSlice contents  
└── Devices []DeviceConfig  // Array of device type templates

DeviceConfig             // Individual device template with multiplicity
├── Name string          // Base device name (nvidia-t4-0)
├── Count int            // Number of instances to create
└── Attributes map       // Device properties (type, memory, etc.)
```

### Configuration Flow: YAML → Kubernetes API
```yaml
# ConfigMap YAML (Human-readable)
driver: karpenter.sh/dra-kwok-driver
mappings:
  - name: gpu-mapping
    nodeSelector:
      matchLabels:
        node.kubernetes.io/instance-type: c-4x-amd64-linux
    resourceSlice:
      devices:
        - name: nvidia-t4-0
          count: 2
          attributes:
            type: nvidia-tesla-t4
            memory: 16Gi
```

**↓ Converts To ↓**

```go
// Kubernetes ResourceSlice (API Objects)
resourceSlice := &resourcev1.ResourceSlice{
    Spec: resourcev1.ResourceSliceSpec{
        Driver: "karpenter.sh/dra-kwok-driver",
        Devices: []resourcev1.Device{
            {Name: "nvidia-t4-0-0", Attributes: {...}},
            {Name: "nvidia-t4-0-1", Attributes: {...}},
        },
    },
}
```

## End-to-End Workflow Example

### Complete Flow: ConfigMap → Pod Scheduling

**Step 1: ConfigMap Creation**
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
        nodeSelector:
          matchLabels:
            node.kubernetes.io/instance-type: c-4x-amd64-linux
        resourceSlice:
          devices:
            - name: nvidia-t4-0
              count: 1
              attributes:
                type: nvidia-tesla-t4
                memory: 16Gi
                compute-capability: "7.5"
```

**Step 2: DRA Driver Processing**
1. ConfigMapController detects ConfigMap change
2. Parses YAML and validates configuration structure  
3. Stores parsed config for ResourceSliceController access

**Step 3: Pod Deployment**
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: gpu-workload
spec:
  template:
    spec:
      containers:
      - name: gpu-app
        image: nvidia/cuda:11.2-runtime-ubuntu20.04
        resources:
          requests:
            karpenter.sh/dra-kwok-driver: "1"  # Request DRA resource
```

**Step 4: Karpenter Node Provisioning**
1. Karpenter sees unscheduled pod with DRA resource request
2. Provisions KWOK node with instance type `c-4x-amd64-linux`
3. Node gets KWOK annotation: `kwok.x-k8s.io/node: fake`

**Step 5: ResourceSlice Creation**
1. ResourceSliceController detects new KWOK node
2. Matches node labels against ConfigMap mappings
3. Creates ResourceSlice `kwok-node-1-devices` with:
   - Driver: `karpenter.sh/dra-kwok-driver`
   - Device: `nvidia-t4-0-0` with T4 attributes
   - Pool: `kwok-node-1-device-pool`

**Step 6: Pod Scheduling & Validation**
1. Kubernetes scheduler sees ResourceSlice advertising GPU
2. Schedules pod to node with available DRA resources  
3. Pod starts successfully on KWOK node
4. End-to-end DRA workflow validated

## Architecture Components

### Core Controllers

**ConfigMapController (`pkg/controllers/configmap.go`)**
- **Purpose**: Watches ConfigMap changes and parses DRA configuration
- **Event Filter**: `WithEventFilter()` for `dra-kwok-configmap` in `karpenter` namespace only
- **Key Methods**:
  - `parseConfigFromConfigMap()`: Extracts YAML from `config.yaml` key
  - `Validate()`: Validates configuration structure and rules
  - `GetConfig()`: Provides current config to ResourceSliceController

**ResourceSliceController (`pkg/controllers/resourceslice.go`)**  
- **Purpose**: Manages ResourceSlice lifecycle based on Node events
- **Watches**: All Node events via `For(&corev1.Node{})`
- **Key Methods**:
  - `isKWOKNode()`: Detects KWOK nodes by annotation
  - `findMatchingMapping()`: Matches node labels to configuration mappings
  - `reconcileResourceSlicesForNode()`: Creates/updates ResourceSlices with devices
  - `cleanupResourceSlicesForNode()`: Removes ResourceSlices on node deletion

### Configuration Examples

**Multi-GPU Configuration:**
```yaml
driver: karpenter.sh/dra-kwok-driver
mappings:
  - name: gpu-t4-single
    nodeSelector:
      matchLabels:
        node.kubernetes.io/instance-type: c-4x-amd64-linux
    resourceSlice:
      devices:
        - name: nvidia-t4-0
          count: 1
          attributes:
            type: nvidia-tesla-t4
            memory: 16Gi
  
  - name: gpu-v100-multi  
    nodeSelector:
      matchLabels:
        node.kubernetes.io/instance-type: m-8x-amd64-linux
    resourceSlice:
      devices:
        - name: nvidia-v100-0
          count: 4
          attributes:
            type: nvidia-tesla-v100
            memory: 32Gi
            nvlink: "true"
```

**Mixed Device Types:**
```yaml
mappings:
  - name: mixed-accelerators
    nodeSelector:
      matchLabels:
        accelerator-type: mixed
    resourceSlice:
      devices:
        - name: nvidia-t4-0      # GPUs
          count: 2
          attributes:
            type: nvidia-tesla-t4
        - name: xilinx-fpga-0    # FPGAs  
          count: 1
          attributes:
            type: xilinx-alveo-u250
            dsp-slices: "12288"
```

## Testing & Validation

### Unit Tests (All Passing ✅)
- **Config Types** (`pkg/config/types_test.go`): 10/10 specs - YAML parsing, validation, struct mapping
- **ConfigMap Controller** (`pkg/controllers/configmap_test.go`): 7/7 specs - Event filtering, parsing, error handling  
- **ResourceSlice Controller** (`pkg/controllers/resourceslice_test.go`): 21/21 specs - Node detection, mapping, device generation

### Integration Tests
```bash
# Run all unit tests
go test ./... -v

# Run specific controller tests
go test ./pkg/controllers -v

# Run configuration type tests
go test ./pkg/config -v
```

### Running the Driver
```bash
# Build the driver
go build -o dra-kwok-driver main.go

# Run with development logging
./dra-kwok-driver --zap-log-level=debug

# Deploy to cluster (example)
kubectl apply -f deploy/
```

## Key Features & Capabilities

### ✅ **Implemented & Working**
- **Single ResourceSlice Per Node**: Pools all device types into one ResourceSlice
- **KWOK Instance Type Integration**: Uses real KWOK types (`c-4x-amd64-linux`, `m-8x-amd64-linux`)
- **Dynamic Configuration**: Live ConfigMap updates without driver restart
- **Multi-Device Support**: Multiple device types (GPU, FPGA) per node
- **Device Multiplicity**: Generate multiple instances from single template (`count: N`)
- **Proper Cleanup**: ResourceSlices automatically deleted when nodes removed
- **Type Safety**: Full validation chain with structured error messages

### 🔄 **Configuration Conversion**
```
YAML Template                 →  Kubernetes API Objects
--------------                   -------------------------
DeviceConfig{                   []resourcev1.Device{
  Name: "nvidia-t4-0",            {Name: "nvidia-t4-0-0", ...},  
  Count: 3,               →       {Name: "nvidia-t4-0-1", ...},
  Attributes: {...}               {Name: "nvidia-t4-0-2", ...},
}                               }
```

### 📋 **Driver Status**
- **Driver Name**: `karpenter.sh/dra-kwok-driver` (unified across all device types)
- **ConfigMap**: `dra-kwok-configmap` in `karpenter` namespace
- **Node Detection**: KWOK annotation `kwok.x-k8s.io/node: fake`
- **Resource Pool**: `{nodeName}-device-pool` (single pool per node)
- **Device Naming**: `{configName}-{instanceIndex}` (e.g., `nvidia-t4-0-0`)

## Troubleshooting

### No ResourceSlices Created
1. Check DRA KWOK driver is running: `kubectl logs -n karpenter deployment/dra-kwok-driver`
2. Verify ConfigMap exists: `kubectl get configmap dra-kwok-configmap -n karpenter`  
3. Check ConfigMap format and YAML syntax

### Pods Not Scheduling
1. Verify ResourceSlices exist: `kubectl get resourceslices`
2. Check pod resource requests match driver name
3. Verify node selectors and instance types are correct

### Configuration Not Updating
1. Check ConfigMap was actually updated
2. Monitor driver logs for configuration reload messages  
3. Verify YAML syntax is valid

## Future Enhancements

- **Multi-Driver Support**: Test multiple DRA drivers simultaneously  
- **Resource Sharing**: Test device sharing scenarios
- **Failure Recovery**: Test driver restart and recovery scenarios
- **Performance Testing**: Large-scale DRA resource allocation
- **Cloud Provider Integration**: Real accelerator hardware testing