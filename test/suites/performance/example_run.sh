#!/bin/bash
# Example: Run a single test with a specific decision ratio threshold
# This demonstrates how to test the WhenCostJustifiesDisruption policy

set -e

echo "=== Example: Testing Decision Ratio Threshold ==="
echo ""
echo "This example runs the basic performance test with:"
echo "  - Consolidate When: WhenCostJustifiesDisruption"
echo "  - Decision Ratio Threshold: 1.5 (conservative)"
echo ""

# Set the decision ratio threshold
export DECISION_RATIO_THRESHOLD=1.5

# Set output directory
export OUTPUT_DIR="./example_results"

# Create output directory
mkdir -p "${OUTPUT_DIR}"

echo "Running test..."
echo "Output will be saved to: ${OUTPUT_DIR}"
echo ""

# Run the test
go test -v -timeout 60m -run "Performance" ./basic_test.go 2>&1 | tee "${OUTPUT_DIR}/test_output.log"

echo ""
echo "=== Test Complete ==="
echo ""

# Display results if JSON report exists
if [ -f "${OUTPUT_DIR}/consolidation_performance_report.json" ]; then
    echo "Consolidation Report:"
    
    if command -v jq &> /dev/null; then
        echo ""
        jq '{
            test_name,
            consolidate_when,
            decision_ratio_threshold,
            total_time,
            pods_disrupted,
            rounds,
            change_in_node_count,
            total_reserved_cpu_utilization,
            total_reserved_memory_utilization
        }' "${OUTPUT_DIR}/consolidation_performance_report.json"
    else
        echo "(Install jq to see formatted output)"
        cat "${OUTPUT_DIR}/consolidation_performance_report.json"
    fi
    
    echo ""
    echo "Full report: ${OUTPUT_DIR}/consolidation_performance_report.json"
else
    echo "No consolidation report generated (test may have failed)"
fi

echo ""
echo "To test with a different threshold, run:"
echo "  DECISION_RATIO_THRESHOLD=0.5 ./example_run.sh  # More aggressive"
echo "  DECISION_RATIO_THRESHOLD=2.0 ./example_run.sh  # More conservative"
