# DRA (Dynamic Resource Allocation) Test Suite

This directory contains integration tests for Karpenter's Dynamic Resource Allocation (DRA) functionality.

## Overview

The DRA test suite validates that Karpenter can properly integrate with Kubernetes DRA (Dynamic Resource Allocation) to provision nodes with specialized hardware resources like GPUs, FPGAs, and other accelerators.

## Test Structure

### `suite_test.go`
- Sets up the test environment for DRA integration tests
- Configures standard NodePool and NodeClass for DRA testing
- Manages test lifecycle (BeforeSuite, AfterSuite, BeforeEach, AfterEach)

### `dra_kwok_test.go`
- **DRA KWOK Driver Integration Tests**: Validates the mock DRA driver that creates fake ResourceSlices
- **ConfigMap-Driven Configuration**: Tests dynamic configuration through ConfigMaps
- **Multi-Device Scenarios**: GPU, FPGA, and custom accelerator testing
- **End-to-End Workflows**: From ConfigMap → ResourceSlice → Pod Scheduling

## What These Tests Validate

### 1. **ConfigMap-Based Configuration**
- DRA KWOK driver watches specific ConfigMap (`dra-kwok-config`)
- Configuration parsing and validation (YAML structure, device mappings)
- Dynamic configuration updates without driver restarts
- Error handling for invalid configurations

### 2. **Node Provisioning with DRA Resources**
- Karpenter provisions nodes based on DRA resource requests
- Correct instance types selected based on device requirements
- Node labels and selectors work with device mappings

### 3. **ResourceSlice Management**
- ResourceSlices created when nodes become Ready
- Device attributes match ConfigMap specification
- ResourceSlices updated when configuration changes
- ResourceSlices cleaned up when nodes terminate

### 4. **Pod Scheduling with DRA**
- Pods requesting DRA resources get scheduled to appropriate nodes
- Multiple device types (GPU, FPGA) work correctly
- Multi-device scenarios (requesting multiple GPUs)
- Resource constraints and limits respected

### 5. **Device Type Support**
- **GPU Devices**: NVIDIA T4, V100, A100 with proper attributes
- **FPGA Devices**: Xilinx and Intel FPGAs with hardware specifications
- **Custom Devices**: Extensible configuration for any device type
- **Device Attributes**: Memory, compute capability, interface details

## Test Scenarios

### Basic DRA Integration
```go
It("should create and manage ResourceSlices based on ConfigMap", func() {
    // 1. Create ConfigMap with GPU device mappings
    // 2. Deploy workload requesting GPU resources  
    // 3. Verify node provisioned with GPU instance type
    // 4. Verify ResourceSlice created with correct device attributes
    // 5. Verify pod scheduled successfully
})
```

### Dynamic Configuration Updates
```go
It("should handle ConfigMap updates dynamically", func() {
    // 1. Create initial ConfigMap with single GPU
    // 2. Deploy workload and verify ResourceSlice creation
    // 3. Update ConfigMap to add second GPU
    // 4. Verify ResourceSlices updated with additional devices
})
```

### Multi-Device Scenarios
```go
It("should support multi-GPU workloads", func() {
    // 1. Configure multi-GPU node mapping
    // 2. Deploy workload requesting 4 GPUs
    // 3. Verify node with appropriate multi-GPU instance type
    // 4. Verify ResourceSlice advertises all GPUs
})
```

### Error Handling
```go
It("should handle invalid ConfigMap gracefully", func() {
    // 1. Create ConfigMap with invalid YAML
    // 2. Verify no ResourceSlices created
    // 3. Verify driver continues working after correction
})
```

## Configuration Examples

### GPU Configuration
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: dra-kwok-config
  namespace: karpenter
data:
  config.yaml: |
    driver: gpu.example.com
    mappings:
      - name: gpu-t4-mapping
        nodeSelector:
          matchLabels:
            node.kubernetes.io/instance-type: g4dn.xlarge
        resourceSlice:
          devices:
            - name: nvidia-t4-0
              count: 1
              attributes:
                type: nvidia-tesla-t4
                memory: 16Gi
                compute-capability: "7.5"
```

### FPGA Configuration  
```yaml
driver: fpga.example.com
mappings:
  - name: fpga-xilinx-mapping
    nodeSelector:
      matchLabels:
        accelerator-type: xilinx-fpga
    resourceSlice:
      devices:
        - name: xilinx-u250-0
          count: 1
          attributes:
            type: xilinx-alveo-u250
            memory: 64Gi
            dsp-slices: "12288"
```

## Running DRA Tests

### Run All DRA Tests
```bash
go test ./test/suites/dra/... -v
```

### Run Specific Test
```bash
go test ./test/suites/dra/... -v --ginkgo.focus="GPU Configuration"
```

### Run with Detailed Output
```bash
go test ./test/suites/dra/... -v --ginkgo.v --ginkgo.show-node-events
```

## Prerequisites

### 1. **DRA KWOK Driver Running**
The DRA KWOK driver must be deployed and running in the test cluster:
```bash
# Deploy DRA KWOK driver
kubectl apply -f dra-kwok-driver/examples/
```

### 2. **KWOK Provider**
Tests use KWOK (Kubernetes WithOut Kubelet) for mock nodes:
```bash
make install-kwok
```

### 3. **Test Environment**
Ensure test environment is properly configured:
```bash
export CLUSTER_NAME=test-cluster
export KARPENTER_NAMESPACE=karpenter
```

## Integration Points

### With Karpenter Core
- Tests validate Karpenter's DRA resource request handling  
- Node provisioning based on DRA resource requirements
- Integration with NodePool and NodeClass configurations

### With DRA KWOK Driver
- ConfigMap-driven device configuration
- ResourceSlice lifecycle management
- Mock device attribute handling

### With Kubernetes DRA
- Standard DRA ResourceSlice API usage
- DRA resource request/allocation patterns
- Integration with Kubernetes scheduler

## Troubleshooting

### No ResourceSlices Created
1. Check DRA KWOK driver is running: `kubectl logs -n karpenter deployment/dra-kwok-driver`
2. Verify ConfigMap exists: `kubectl get configmap dra-kwok-config -n karpenter`  
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