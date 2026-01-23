# Add Consolidation Pipeline Logging and Events

## Overview

This PR adds structured logging and Kubernetes event emissions for the consolidation pipeline, improving observability and debuggability of consolidation decisions.

## Changes

### 1. Consolidation Pipeline Logging

Added structured logging at key decision points in the consolidation pipeline:

- **Move Generation**: Logs when a consolidation move is successfully generated, including:
  - Command description (delete vs replace operations)
  - Estimated cost savings
  - Number of candidates and replacements

The logging occurs in three scenarios:
1. Standard consolidation (delete or replace)
2. Multi-node spot-to-spot consolidation
3. Single-node spot-to-spot consolidation

### 2. Event Emissions

Added `ConsolidationMoveGenerated` events that are emitted alongside logs when consolidation moves are generated. This provides:

- **Better testability**: Tests can verify events rather than parsing logs
- **User visibility**: Events appear in `kubectl describe` output for affected nodes/nodeclaims
- **Audit trail**: Events are recorded in the cluster's event stream

Events include:
- The consolidation command (e.g., "delete: [node-1]" or "replace: [node-1] -> [1 replacement]")
- Estimated cost savings

### 3. Helper Functions

Added utility functions to support logging and testing:

- `Command.String()`: Returns human-readable representation of consolidation commands
- `Command.EstimatedSavings()`: Calculates cost savings, handling edge cases like:
  - Multiple replacement nodes (future-proofing for N->M consolidation)
  - Empty offerings or instance type options
  - Delete vs replace operations

### 4. Validation Failure Logging

Added `getValidationFailureReason()` helper to categorize validation failures:
- Budget violations
- Scheduling changes
- Pod churn detection
- Unknown failures

This enables better debugging when consolidation moves fail validation.

## Consolidation Pipeline Phases

The consolidation algorithm has four main phases:

1. **Candidate Selection**: Which nodes are considered for consolidation?
2. **Move Generation**: What consolidation actions could improve the cluster? *(Logging added here)*
3. **Plan Generation**: Which moves are valid given budgets and constraints? *(Logging added here)*
4. **Execution**: Actually perform the consolidation

This PR focuses on phases 2 and 3, where understanding why moves are generated or rejected is most valuable for debugging.

## Testing

- Added comprehensive unit tests for all new functionality
- Tests verify both logging behavior and event emissions
- Edge case testing for cost calculations and command formatting

## Example Output

### Log Output
```
consolidation move generated command="replace: [node-1] -> [1 replacement]" estimated_savings=0.15
```

### Event Output
```
kubectl describe node node-1
...
Events:
  Type    Reason                        Message
  ----    ------                        -------
  Normal  ConsolidationMoveGenerated    Consolidation move generated: replace: [node-1] -> [1 replacement], estimated savings: $0.15/hour
```

## Future Work

- Consider adding events for validation failures (currently only logged)
- Add metrics for consolidation move generation rates
- Expand logging to cover candidate selection phase
