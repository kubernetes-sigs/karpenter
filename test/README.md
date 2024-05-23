# E2E Testing

Karpenter leverages Github Actions to run our E2E test suites. 

## Directories
- `./.github/workflows`: Workflow files run within this repository. Relevant files for E2E testing are prefixed with `e2e-`
- `./.github/actions/e2e`: Composite actions utilized by the E2E workflows
- `./test/suites`: Directories defining test suites
- `./test/pkg`: Common utilities and expectations
- `./test/hack`: Testing scripts
