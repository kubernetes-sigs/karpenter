# DRA Test Suite

This directory contains integration tests for Karpenter's Dynamic Resource Allocation (DRA) functionality.

## Test Overview

The DRA test suite validates end-to-end DRA workflows including:
- DRA KWOK driver integration  
- ResourceSlice lifecycle management
- Pod scheduling with DRA resource requests
- Multi-device scenarios and cleanup

## DRA KWOK Driver

For comprehensive documentation about the DRA KWOK driver architecture, configuration, and workflow, see:

**📖 [DRA KWOK Driver Documentation](../../../dra-kwok-driver/README.md)**

The driver documentation includes:
- Complete architecture and workflow explanation
- Configuration examples and type hierarchy  
- End-to-end usage examples
- Troubleshooting and development guidance

## Running DRA Tests

### Prerequisites

1. **DRA KWOK Driver**: Must be built and available
2. **KWOK Provider**: For mock node creation
3. **Test Environment**: Properly configured cluster

### Test Execution

```bash
# Run all DRA integration tests
go test ./test/suites/dra/... -v

# Run specific test scenarios
go test ./test/suites/dra/... -v --ginkgo.focus="GPU Configuration"

# Run with detailed output
go test ./test/suites/dra/... -v --ginkgo.v --ginkgo.show-node-events
```

### Test Scenarios

- **Basic DRA Workflows**: ConfigMap → ResourceSlice → Pod scheduling
- **Multi-Device Support**: GPU + FPGA combinations
- **Dynamic Configuration**: Live ConfigMap updates  
- **Error Handling**: Invalid configurations and cleanup
- **Integration Testing**: Full Karpenter + DRA KWOK driver integration

## Integration Points

### With Karpenter Core
- Node provisioning based on DRA resource requirements
- Integration with NodePool and NodeClass configurations
- DRA resource request handling and validation

### With DRA KWOK Driver  
- ConfigMap-driven device configuration testing
- ResourceSlice lifecycle validation
- Mock device attribute and allocation testing