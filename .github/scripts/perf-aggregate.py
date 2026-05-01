"""Aggregate multi-iteration benchmark results and emit benchmark-action JSON.

Reads per-iteration performance_report.json files, computes batch statistics
for multiple metrics (duration, memory, CPU, efficiency), and writes a
customSmallerIsBetter JSON file for benchmark-action/github-action-benchmark.
"""
import json, math, os, glob

output_dir = os.environ["OUTPUT_DIR"]
iterations = int(os.environ.get("ITERATIONS", "1"))

# Metrics to extract. (json_field, display_name, unit)
# All emitted as smallerIsBetter for the primary pass/fail metrics (duration,
# memory, CPU). Utilization and efficiency are informational context.
METRICS = [
    ("total_time", "Duration", "seconds"),
    ("karpenter_memory_mb", "Controller Peak Memory", "MB"),
    ("karpenter_cpu_nanos", "Controller CPU", "cpu-ms"),
    ("total_nodes", "Final Nodes", "nodes"),
    ("total_reserved_cpu_utilization", "CPU Utilization", "percent"),
    ("resource_efficiency_score", "Efficiency Score", "score"),
    ("rounds", "Consolidation Rounds", "rounds"),
]


def compute_stats(values):
    n = len(values)
    if n == 0:
        return None
    mean = sum(values) / n
    sorted_v = sorted(values)
    median = sorted_v[n // 2] if n % 2 else (sorted_v[n // 2 - 1] + sorted_v[n // 2]) / 2
    variance = sum((x - mean) ** 2 for x in values) / n
    stddev = math.sqrt(variance)
    cv = (stddev / mean * 100) if mean > 0 else 0
    return {
        "n": n, "mean": round(mean, 2), "median": round(median, 2),
        "stddev": round(stddev, 2), "cv_pct": round(cv, 1),
        "min": round(min(values), 2), "max": round(max(values), 2),
    }


# Collect per-iteration reports grouped by test name
reports_by_test = {}
for i in range(1, iterations + 1):
    pattern = os.path.join(output_dir, f"iter_{i}", "*_performance_report.json")
    for path in glob.glob(pattern):
        test_key = os.path.basename(path)
        reports_by_test.setdefault(test_key, []).append(path)

summary = {}
benchmark_results = []

for test_key, paths in reports_by_test.items():
    all_data = []
    for path in paths:
        try:
            with open(path) as f:
                all_data.append(json.load(f))
        except Exception:
            pass

    if not all_data:
        continue

    name = test_key.replace("_performance_report.json", "").replace("_", " ").title()
    test_summary = {}

    for json_field, metric_name, unit in METRICS:
        values = []
        for data in all_data:
            if json_field in data and data[json_field] is not None:
                v = float(data[json_field])
                # Convert nanosecond duration to seconds
                if json_field == "total_time" and v > 1e9:
                    v = v / 1e9
                # Convert cpu nanos to milliseconds for readability
                if json_field == "karpenter_cpu_nanos":
                    v = v / 1e6
                values.append(v)

        if not values:
            continue

        stats = compute_stats(values)
        test_summary[metric_name] = stats

        benchmark_results.append({
            "name": f"{name} - {metric_name} (median, n={stats['n']})",
            "unit": unit,
            "value": stats["median"],
            "range": str(stats["stddev"]),
            "extra": (
                f"mean={stats['mean']} stddev={stats['stddev']} "
                f"cv={stats['cv_pct']}% min={stats['min']} max={stats['max']} n={stats['n']}"
            ),
        })

    summary[test_key] = test_summary

# Write benchmark-action JSON
bench_path = os.path.join(output_dir, "benchmark-results.json")
with open(bench_path, "w") as f:
    json.dump(benchmark_results, f, indent=2)

# Write detailed summary
summary_path = os.path.join(output_dir, "aggregated_summary.json")
with open(summary_path, "w") as f:
    json.dump(summary, f, indent=2)

# Print table
print(f"\n{'Test / Metric':<55} {'n':>3} {'Median':>10} {'Mean':>10} {'Stddev':>10} {'CV':>6}")
print("-" * 100)
for test_key, metrics in summary.items():
    test_name = test_key.replace("_performance_report.json", "")
    for metric_name, stats in metrics.items():
        label = f"  {test_name} / {metric_name}"
        print(f"{label:<55} {stats['n']:>3} {stats['median']:>10.1f} {stats['mean']:>10.1f} {stats['stddev']:>10.1f} {stats['cv_pct']:>5.1f}%")

print(f"\nEmitted {len(benchmark_results)} metrics to {bench_path}")
