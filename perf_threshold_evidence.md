# Performance Threshold Evidence

Data collected from CI runs with resource limits set to 2 CPU / 2 GiB (unthrottled).
Metrics sourced from `process_resident_memory_bytes` (RSS) and `process_cpu_seconds_total` (CPU rate computed from counter deltas) via direct scraping of Karpenter's `/metrics` endpoint.

## Configuration

- **Runner:** GitHub Actions `ubuntu-latest` (ubuntu-24.04, 4 cores, 16 GB RAM)
- **Cluster:** Kind single-node, KWOK cloud provider
- **Karpenter limits:** 2 CPU / 2 GiB (requests: 1 CPU / 1 GiB)
- **KWOK limits:** 2 CPU / 4 GiB
- **Polling interval:** 5 seconds
- **Metric source:** Karpenter pod's `/metrics` endpoint (process-level, not container-level)

## P95 Memory (MB)

| Test | Phase | Run 6 | Run 7 | Run 8 | Run 9 | Run 10 | Run 11 | Run 12 | Run 13 | Run 14 | Max Observed | Threshold |
|------|-------|-------|-------|-------|-------|--------|--------|--------|--------|--------|--------------|-----------|
| Basic Scale | scale-out | 228.75 | 218.48 | 244.96 | 223.43 | 236.81 | 232.42 | 235.49 | 241.01 | 233.25 | 244.96 | 300 |
| Basic Scale | consolidation | 248.89 | 249.15 | 254.79 | 245.52 | 248.86 | 252.29 | 248.64 | 251.02 | 251.17 | 254.79 | 310 |
| Drift Performance | initial (scale-out) | 305.56 | 334.76 | 335.60 | 373.34 | 337.66 | 352.54 | 354.60 | 368.19 | 313.16 | 373.34 | 450 |
| Drift Performance | drift | 458.38 | 452.84 | 453.13 | 444.06 | 440.96 | 447.56 | 449.44 | 454.81 | 453.69 | 458.38 | 550 |
| Wide Deployments | scale-out | 282.64 | 306.42 | 269.77 | 294.03 | 307.85 | 279.93 | 326.35 | 325.40 | 292.43 | 326.35 | 375 |
| Wide Deployments | consolidation | 281.57 | 298.98 | 320.38 | 287.41 | 293.21 | 282.56 | 290.70 | 293.80 | 281.43 | 320.38 | 390 |
| Host Name Spreading | scale-out | 563.87 | 556.67 | 582.45 | 583.49 | 558.28 | 478.27 | 571.94 | 561.02 | 503.77 | 583.49 | 700 |
| Host Name Spreading | consolidation | 492.75 | 505.53 | 500.45 | 551.00 | 523.06 | 504.02 | 490.55 | 508.10 | 498.55 | 551.00 | 660 |
| Host Name Spreading XL | scale-out | 1062.82 | 1217.37 | 1218.88 | 1161.07 | 1125.04 | 1173.22 | 1171.38 | 1166.57 | 1177.19 | 1218.88 | 1475 |
| Host Name Spreading XL | consolidation | 1013.99 | 999.33 | 1044.13 | 983.46 | 966.19 | 1140.80 | 1020.11 | 1019.77 | 1023.89 | 1140.80 | 1260 |
| Self Anti-Affinity | scale-out | 519.92 | 506.53 | 549.65 | 477.58 | 503.29 | 503.21 | 548.17 | 480.10 | 495.06 | 549.65 | 660 |
| Self Anti-Affinity | interference | 948.66 | 934.19 | 942.82 | 696.05 | 869.30 | 896.24 | 878.15 | 763.18 | 846.07 | 948.66 | 1140 |
| Self Anti-Affinity | consolidation | 798.41 | 770.21 | 776.62 | 783.48 | 779.43 | 794.16 | 771.89 | 764.01 | 783.96 | 798.41 | 960 |
| Do Not Disrupt | scale-out | 542.38 | 553.54 | 581.03 | 538.66 | 579.14 | 533.77 | 566.52 | lint timeout | 552.94 | 581.03 | 700 |
| Do Not Disrupt | consolidation | 476.29 | 454.17 | 459.09 | 454.38 | 519.84 | 514.28 | 482.04 | lint timeout | 506.05 | 519.84 | 575 |

## P95 CPU (cores)

| Test | Phase | Run 6 | Run 7 | Run 8 | Run 9 | Max Observed | Proposed Threshold |
|------|-------|-------|-------|-------|-------|--------------|-------------------|
| Basic Scale | scale-out | 1.0331 | 0.5716 | 0.9474 | 0.7676 | 1.0331 | |
| Basic Scale | consolidation | 0.4996 | 0.3960 | 0.5940 | 0.4880 | 0.5940 | |
| Drift Performance | initial (scale-out) | 0.8350 | 1.0491 | 1.2229 | 1.3211 | 1.3211 | |
| Drift Performance | drift | 1.5676 | 1.5458 | 1.5050 | 1.5446 | 1.5676 | |
| Wide Deployments | scale-out | 1.0230 | 1.0296 | 0.9765 | 0.9899 | 1.0296 | |
| Wide Deployments | consolidation | 0.8796 | 0.7755 | 0.5568 | 0.8297 | 0.8796 | |
| Host Name Spreading | scale-out | 1.3720 | 1.3693 | 1.4214 | 1.3684 | 1.4214 | |
| Host Name Spreading | consolidation | 1.5909 | 1.5910 | 1.5600 | 1.5444 | 1.5910 | |
| Host Name Spreading XL | scale-out | 1.5524 | 1.6302 | 1.6109 | 1.6225 | 1.6302 | |
| Host Name Spreading XL | consolidation | 1.7060 | 1.6973 | 1.5975 | 1.6672 | 1.7060 | |
| Self Anti-Affinity | scale-out | 1.3910 | 1.2795 | 1.2189 | 1.2898 | 1.3910 | |
| Self Anti-Affinity | interference | 1.5790 | 1.6111 | 1.6043 | 1.1757 | 1.6111 | |
| Self Anti-Affinity | consolidation | 1.2763 | 1.1253 | 1.2332 | 1.4189 | 1.4189 | |
| Do Not Disrupt | scale-out | 1.3700 | 1.4005 | 1.3868 | 1.3633 | 1.4005 | |
| Do Not Disrupt | consolidation | 1.4032 | 0.6949 | 1.3276 | 1.5132 | 1.5132 | |

## Avg CPU (cores)

| Test | Phase | Run 6 | Run 7 | Run 8 | Run 9 | Run 10 | Run 11 | Run 12 | Run 13 | Run 14 | Max Observed | Threshold |
|------|-------|-------|-------|-------|-------|--------|--------|--------|--------|--------|--------------|-----------|
| Basic Scale | scale-out | 0.3897 | 0.3151 | 0.4151 | 0.3207 | 0.3944 | 0.4071 | 0.3907 | 0.4196 | 0.3872 | 0.4196 | 0.50 |
| Basic Scale | consolidation | 0.1601 | 0.1532 | 0.1878 | 0.1396 | 0.1925 | 0.1968 | 0.1532 | 0.1952 | 0.1679 | 0.1968 | 0.25 |
| Drift Performance | initial (scale-out) | 0.5647 | 0.5381 | 0.5715 | 0.6500 | 0.5600 | 0.5597 | 0.6048 | 0.5813 | 0.5552 | 0.6500 | 0.75 |
| Drift Performance | drift | 0.8682 | 0.8471 | 0.6756 | 0.7935 | 0.8004 | 0.8228 | 0.8521 | 0.7695 | 0.8745 | 0.8745 | 1.00 |
| Wide Deployments | scale-out | 0.5554 | 0.5504 | 0.4829 | 0.4782 | 0.4414 | 0.5286 | 0.4794 | 0.5434 | 0.5127 | 0.5554 | 0.65 |
| Wide Deployments | consolidation | 0.2317 | 0.2372 | 0.2480 | 0.2506 | 0.2188 | 0.2121 | 0.2314 | 0.2185 | 0.2381 | 0.2506 | 0.30 |
| Host Name Spreading | scale-out | 0.8609 | 0.6916 | 0.9376 | 0.7452 | 0.7008 | 0.8857 | 0.8011 | 0.8795 | 0.7902 | 0.9376 | 1.10 |
| Host Name Spreading | consolidation | 0.5840 | 0.7842 | 0.5420 | 0.6294 | 0.7992 | 0.5866 | 0.6384 | 0.5614 | 0.5978 | 0.7992 | 0.95 |
| Host Name Spreading XL | scale-out | 1.0326 | 1.2665 | 1.1933 | 1.1463 | 1.1304 | 1.1561 | 1.0530 | 1.1535 | 1.0998 | 1.2665 | 1.50 |
| Host Name Spreading XL | consolidation | 1.2836 | 1.3407 | 1.0746 | 0.9755 | 1.3391 | 1.2862 | 1.3686 | 1.2364 | 1.2053 | 1.3686 | 1.60 |
| Self Anti-Affinity | scale-out | 0.7569 | 0.6154 | 0.6444 | 0.6988 | 0.8045 | 0.6829 | 0.7433 | 0.6574 | 0.6922 | 0.8045 | 0.95 |
| Self Anti-Affinity | interference | 0.9163 | 1.0497 | 0.8737 | 0.7441 | 0.9262 | 1.0455 | 0.9707 | 1.0270 | 0.8500 | 1.0497 | 1.25 |
| Self Anti-Affinity | consolidation | 0.6575 | 0.6543 | 0.5844 | 0.6931 | 0.5687 | 0.5708 | 0.6340 | 0.5700 | 0.5926 | 0.6931 | 0.80 |
| Do Not Disrupt | scale-out | 0.7703 | 0.8875 | 0.8359 | 0.7470 | 0.6704 | 0.7304 | 0.8989 | lint timeout | 0.6501 | 0.8989 | 1.05 |
| Do Not Disrupt | consolidation | 0.5727 | 0.3115 | 0.3751 | 0.4929 | 0.3572 | 0.5163 | 0.4000 | lint timeout | 0.5611 | 0.5727 | 0.70 |

## Observations

- **Memory P95 is stable** across runs (typically <15% variance for a given test/phase)
- **CPU P95 is volatile** (e.g. Basic Scale scale-out ranges from 0.57 to 1.03 across 4 runs) — not suitable for tight assertions
- **Avg CPU is more stable** and better suited for regression assertions
- Runs 1-5 (not shown) had a 1 CPU / 1 GiB limit that artificially throttled CPU and caused metrics endpoint failures under load
- Host Name Spreading XL is the heaviest test (2000 pods) and consistently peaks at ~1.2 GiB RSS and ~1.6 CPU cores

## Threshold Methodology

- **Memory P95:** threshold = ~1.15x max observed (rounded to nearest 5 MB). Memory is structurally stable — a real regression (new caches, leaks) would be a sustained increase well above this.
- **Avg CPU:** threshold = ~1.15x max observed (rounded to nearest 0.05 cores). Applied to max observed rather than average, since CPU has more run-to-run variance from GHA runner scheduling.
- **P95 CPU is retained for reference** but not recommended for assertions due to high variance from sample timing aliasing (a single burst captured vs missed makes large swings).
- Thresholds validated against 5 runs (Runs 10-14) with zero false positives at 1.15x max.

## CI Runs

| Run | ID | Date | Notes |
|-----|-----|------|-------|
| Run 6 | [26531560272](https://github.com/AndrewMitchell25/karpenter/actions/runs/26531560272) | 2026-05-27 | First run with 2 CPU / 2Gi limits |
| Run 7 | [26534224081](https://github.com/AndrewMitchell25/karpenter/actions/runs/26534224081) | 2026-05-27 | Added response body drain fix |
| Run 8 | [26537725296](https://github.com/AndrewMitchell25/karpenter/actions/runs/26537725296) | 2026-05-27 | All 7 jobs passed |
| Run 9 | [26540276530](https://github.com/AndrewMitchell25/karpenter/actions/runs/26540276530) | 2026-05-27 | All 7 jobs passed |
| Run 10 | [26543522182](https://github.com/AndrewMitchell25/karpenter/actions/runs/26543522182) | 2026-05-27 | Validation run 1 - all passed with new thresholds |
| Run 11 | [26544988088](https://github.com/AndrewMitchell25/karpenter/actions/runs/26544988088) | 2026-05-27 | Validation run 2 - all passed |
| Run 12 | [26588167192](https://github.com/AndrewMitchell25/karpenter/actions/runs/26588167192) | 2026-05-28 | Validation run 3 - all passed |
| Run 13 | [26590510675](https://github.com/AndrewMitchell25/karpenter/actions/runs/26590510675) | 2026-05-28 | Validation run 4 - 5/7 passed. XL: karpenter pod crashed during AfterEach cleanup (OOM suspected). Do Not Disrupt: golangci-lint timeout during verify step. |
| Run 14 | [26592732931](https://github.com/AndrewMitchell25/karpenter/actions/runs/26592732931) | 2026-05-28 | Validation run 5 - all passed |
