#!/bin/bash
# Script to run performance tests with different decision ratio thresholds
# This allows testing the WhenCostJustifiesDisruption consolidation policy
# with various threshold values to analyze their impact on consolidation behavior.

set -e

# Default values
OUTPUT_BASE_DIR="${OUTPUT_DIR:-./performance_results}"
TEST_PATTERN="${TEST_PATTERN:-basic_test.go}"

# Decision ratio thresholds to test
# Format: "threshold_value:description"
THRESHOLDS=(
    "0.5:aggressive"
    "1.0:balanced"
    "1.5:conservative"
    "2.0:very_conservative"
)

echo "=== Decision Ratio Threshold Performance Testing ==="
echo "Output directory: ${OUTPUT_BASE_DIR}"
echo "Test pattern: ${TEST_PATTERN}"
echo ""

# Create base output directory
mkdir -p "${OUTPUT_BASE_DIR}"

# Run tests for each threshold
for threshold_config in "${THRESHOLDS[@]}"; do
    IFS=':' read -r threshold description <<< "$threshold_config"
    
    echo "----------------------------------------"
    echo "Testing with Decision Ratio Threshold: ${threshold} (${description})"
    echo "----------------------------------------"
    
    # Create output directory for this threshold
    test_output_dir="${OUTPUT_BASE_DIR}/threshold_${threshold}_${description}"
    mkdir -p "${test_output_dir}"
    
    # Export the decision ratio threshold
    export DECISION_RATIO_THRESHOLD="${threshold}"
    
    # Run the test
    echo "Running test with threshold ${threshold}..."
    OUTPUT_DIR="${test_output_dir}" go test -v -timeout 60m -run "Performance" ./${TEST_PATTERN} 2>&1 | tee "${test_output_dir}/test_output.log"
    
    echo ""
    echo "Results saved to: ${test_output_dir}"
    echo ""
done

echo "=== All tests completed ==="
echo ""
echo "Summary of results:"
for threshold_config in "${THRESHOLDS[@]}"; do
    IFS=':' read -r threshold description <<< "$threshold_config"
    test_output_dir="${OUTPUT_BASE_DIR}/threshold_${threshold}_${description}"
    
    echo ""
    echo "Threshold ${threshold} (${description}):"
    
    # Check for JSON reports
    if [ -f "${test_output_dir}/consolidation_performance_report.json" ]; then
        echo "  - Consolidation report: ${test_output_dir}/consolidation_performance_report.json"
        
        # Extract key metrics if jq is available
        if command -v jq &> /dev/null; then
            total_time=$(jq -r '.total_time' "${test_output_dir}/consolidation_performance_report.json" 2>/dev/null || echo "N/A")
            pods_disrupted=$(jq -r '.pods_disrupted' "${test_output_dir}/consolidation_performance_report.json" 2>/dev/null || echo "N/A")
            rounds=$(jq -r '.rounds' "${test_output_dir}/consolidation_performance_report.json" 2>/dev/null || echo "N/A")
            
            echo "    Total Time: ${total_time}"
            echo "    Pods Disrupted: ${pods_disrupted}"
            echo "    Consolidation Rounds: ${rounds}"
        fi
    else
        echo "  - No consolidation report found"
    fi
done

echo ""
echo "To compare results, examine the JSON files in each threshold directory."
