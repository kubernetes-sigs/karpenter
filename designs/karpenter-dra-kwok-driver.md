# KWOK DRA Driver Design

## Summary
The upstream kubernetes/perf-tests repository includes a [DRAKWOKDriver](https://github.com/kubernetes/perf-tests/pull/3491/files), but it's designed for scale performance testing with cluster loader. 

This design introduces a **Karpenter DRA KWOK Driver** - a stateless controller that creates ResourceSlices on KWOK nodes for DRA testing. The controller simply watches for KWOK nodes and creates corresponding ResourceSlices based on ConfigMap configuration. No communication with Karpenter is needed - the controller just ensures ResourceSlices exist on the API server for the Kubernetes scheduler to discover.

## Example Use Case
Mock KWOK test creates ResourceSlice for H100 GPU with all associated device attributes. ResourceClaim selects on those attributes. With Node Overlay integration: Karpenter brings up node → controller creates ResourceSlice → Kubernetes scheduler binds pod to node. End condition: did we create a node, did we create the right node, and did the pod schedule?

## Architecture

### ConfigMap Examples

#### Case 1: Manual Configuration
For when you need direct control over which nodes get which devices, custom test scenarios

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
1. **Test author defines mappings**: "g5.48xlarge nodes should have 8x H100 GPUs"
2. **Controller watches for nodes**: When KWOK creates a node with `instance-type: g5.48xlarge`
3. **Controller creates ResourceSlice**: Advertises 8x H100 GPUs with specified attributes
4. **Scheduler sees devices**: ResourceSlices make GPUs appear "available" for scheduling

#### Case 2: Node Overlay Integration (Future implementation)
**When to use**: Automatic device detection based on existing Node Overlay configurations

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: kwok-dra-config
data:
  config.yaml: |
    driver: "fake-gpu.kwok.x-k8s.io" 
    nodeOverlayIntegration: true
    # Controller automatically detects Node Overlays and creates
    # ResourceSlices matching the overlay configuration
```

**How it works**:
1. **Node Overlay exists**: KWOK Node Overlay already defines what a "g5.48xlarge" node looks like
2. **Controller discovers overlay**: Reads Node Overlay configuration to understand node capabilities  
3. **Controller infers devices**: Maps instance types to expected devices (g5.48xlarge → H100 GPUs)
4. **Automatic ResourceSlice creation**: No manual mapping needed

**Example Node Overlay it would read**:
```yaml
apiVersion: kwok.x-k8s.io/v1alpha1
kind: NodeOverlay
metadata:
  name: g5-48xlarge-overlay
spec:
  nodeSelector:
    matchLabels:
      node.kubernetes.io/instance-type: "g5.48xlarge"
  template:
    metadata:
      labels:
        nvidia.com/gpu.present: "true"
        nvidia.com/gpu.count: "8"
    # Controller would read these labels and create appropriate ResourceSlices
```

### Implementation

Inside `kwok/controllers/dra/` with files:
- `controller.go` - Node watching and ResourceSlice creation/deletion
- `config.go` - ConfigMap parsing and validation  
- `resourceslice.go` - ResourceSlice CRUD operations

**Integration**: Modified `kwok/main.go` to register the DRA controller

### Workflow

1. **Controller loads ConfigMap** defining node selector → ResourceSlice mappings
2. **Node registration triggers** ResourceSlice creation for matching nodes
3. **Test creates ResourceClaim** with device attribute selectors
4. **Test creates DRA pod** referencing the ResourceClaim
5. **Kubernetes scheduler** discovers ResourceSlices and attempts pod binding
6. **Test validates** node creation, pod scheduling, and Karpenter behavior
7. **Cleanup automatically removes** ResourceSlices when nodes are deleted