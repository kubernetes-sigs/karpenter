#!/usr/bin/env python3
"""
Generate ranking-strategies charts for the consolidation cost threshold RFC.

Simulates a realistic cluster by:
1. Sampling pod workloads from AWS Fargate task sizes (vCPU, memory pairs)
2. Bin-packing pods onto EC2 instance types (c7i, m7i, r7i families)
3. Killing a variable fraction of pods per node to simulate scale-down
4. Generating REPLACE moves (reprovision remaining pods on cheapest instance)
5. Generating DELETE moves (scatter pods onto existing spare capacity)

Produces two charts showing cumulative savings vs. cumulative disruption
under four ranking strategies: score, savings-only, disruption-only, random.

Usage:
    python3 designs/scripts/consolidation-cost-threshold-ranking.py

Output:
    designs/ranking-strategies-replace.png
    designs/ranking-strategies-delete.png
"""

import numpy as np
import matplotlib.pyplot as plt
from dataclasses import dataclass, field
from typing import List
from pathlib import Path

SEED = 42
OUTPUT_DIR = Path(__file__).parent.parent

# ---------------------------------------------------------------------------
# EC2 instance types: c7i, m7i, r7i families (us-east-1, Linux, on-demand)
# Source: https://instances.vantage.sh
# ---------------------------------------------------------------------------
EC2_INSTANCES = [
    # c7i: compute optimized (2:1 memory:vCPU)
    ("c7i.large",     2,    4,  0.0893),
    ("c7i.xlarge",    4,    8,  0.1785),
    ("c7i.2xlarge",   8,   16,  0.3570),
    ("c7i.4xlarge",  16,   32,  0.7140),
    ("c7i.8xlarge",  32,   64,  1.4280),
    ("c7i.12xlarge", 48,   96,  2.1420),
    ("c7i.16xlarge", 64,  128,  2.8560),
    ("c7i.24xlarge", 96,  192,  4.2840),
    # m7i: general purpose (4:1 memory:vCPU)
    ("m7i.large",     2,    8,  0.1008),
    ("m7i.xlarge",    4,   16,  0.2016),
    ("m7i.2xlarge",   8,   32,  0.4032),
    ("m7i.4xlarge",  16,   64,  0.8064),
    ("m7i.8xlarge",  32,  128,  1.6128),
    ("m7i.12xlarge", 48,  192,  2.4190),
    ("m7i.16xlarge", 64,  256,  3.2260),
    ("m7i.24xlarge", 96,  384,  4.8384),
    ("m7i.48xlarge",192,  768,  9.6768),
    # r7i: memory optimized (8:1 memory:vCPU)
    ("r7i.large",     2,   16,  0.1323),
    ("r7i.xlarge",    4,   32,  0.2646),
    ("r7i.2xlarge",   8,   64,  0.5292),
    ("r7i.4xlarge",  16,  128,  1.0584),
    ("r7i.8xlarge",  32,  256,  2.1168),
    ("r7i.12xlarge", 48,  384,  3.1750),
    ("r7i.16xlarge", 64,  512,  4.2336),
    ("r7i.24xlarge", 96,  768,  6.3500),
    ("r7i.48xlarge",192, 1536, 12.7008),
]

# Fixed overhead of consolidating any node (independent of pod count)
NODE_BASELINE_DISRUPTION = 1.0


@dataclass
class Pod:
    cpu: float
    mem: float
    disruption_cost: float = 1.0


@dataclass
class Node:
    name: str
    cpu_cap: float
    mem_cap: float
    cost_per_hr: float
    pods: list = field(default_factory=list)

    @property
    def cpu_used(self):
        return sum(p.cpu for p in self.pods)

    @property
    def mem_used(self):
        return sum(p.mem for p in self.pods)

    def fits(self, pod):
        return (pod.cpu <= self.cpu_cap - self.cpu_used and
                pod.mem <= self.mem_cap - self.mem_used)

    @property
    def disruption_cost(self):
        return NODE_BASELINE_DISRUPTION + sum(
            max(0, p.disruption_cost) for p in self.pods)


def find_cheapest_fitting(cpu_needed, mem_needed):
    """Find the cheapest EC2 instance that fits the given resources."""
    best = None
    for name, vcpu, mem, cost in EC2_INSTANCES:
        if vcpu >= cpu_needed and mem >= mem_needed:
            if best is None or cost < best[3]:
                best = (name, vcpu, mem, cost)
    return best


def cheapest_instance_for(pods):
    """Find the cheapest instance type that fits all pods."""
    total_cpu = sum(p.cpu for p in pods)
    total_mem = sum(p.mem for p in pods)
    return find_cheapest_fitting(total_cpu, total_mem)


def build_cluster(rng, n_pods=20000):
    """
    Build a cluster by bin-packing pods onto EC2 instances.

    Uses first-fit-decreasing: sort all pods by resource footprint (cpu * mem)
    descending, then greedily place each pod onto the first existing node that
    fits. When no node fits, provision a new one -- picking the cheapest
    instance that fits the pod, but upsizing to a ~4x bigger instance if it
    costs less than 2x as much. This produces denser packing and a more
    realistic instance type distribution.
    """
    # Generate pods with random resource requests. CPU and memory are drawn
    # independently from log-normal distributions, producing a realistic mix
    # of compute-heavy, memory-heavy, and balanced pods. This naturally
    # spreads pods across different instance families (c7i for compute-heavy,
    # r7i for memory-heavy, m7i for balanced), creating cost diversity.
    cpu_raw = rng.lognormal(mean=0.0, sigma=1.0, size=n_pods)
    mem_raw = rng.lognormal(mean=1.5, sigma=1.2, size=n_pods)

    # Clamp to reasonable ranges and round to Fargate-like granularity
    all_pods = []
    for c, m in zip(cpu_raw, mem_raw):
        cpu = float(np.clip(round(c * 2) / 2, 0.25, 16))    # 0.25 to 16, step 0.5
        mem = float(np.clip(round(m), 1, 120))                # 1 to 120 GiB, step 1
        dc = float(rng.choice([0, 1, 1, 1, 1, 1, 10]))
        all_pods.append(Pod(cpu=cpu, mem=mem, disruption_cost=dc))

    # First-fit-decreasing bin packing: sort all pods by resource footprint
    # descending, then greedily place each onto the first existing node that
    # fits. When no node fits, provision the cheapest instance that can hold
    # the pod. This mirrors Karpenter's provisioning behavior.
    all_pods.sort(key=lambda p: p.cpu * p.mem, reverse=True)

    nodes = []
    for pod in all_pods:
        placed = False
        for node in nodes:
            if node.fits(pod):
                node.pods.append(pod)
                placed = True
                break
        if not placed:
            chosen = find_cheapest_fitting(pod.cpu, pod.mem)
            if chosen is None:
                raise ValueError(
                    f"No instance fits pod ({pod.cpu}, {pod.mem})")
            new_node = Node(
                name=chosen[0],
                cpu_cap=chosen[1], mem_cap=chosen[2],
                cost_per_hr=chosen[3],
            )
            new_node.pods.append(pod)
            nodes.append(new_node)

    return nodes


def churn_pods(rng, nodes):
    """Simulate workload churn: kill some pods, add some new ones.

    For each node, kill 0-80% of pods (simulating scale-down or
    redeployment), then add 0-3 new randomly-sized pods if they fit
    (simulating new deployments landing on nodes with spare capacity).
    This changes the resource mix on each node over time, creating
    mismatches between the node's instance type and its actual workload
    -- exactly the situation that consolidation is designed to fix.
    """
    for node in nodes:
        # Kill phase: remove a random fraction of pods
        if len(node.pods) > 1:
            kill_fraction = rng.uniform(0.0, 0.8)
            n_kill = int(len(node.pods) * kill_fraction)
            if n_kill > 0:
                kill_indices = set(
                    rng.choice(len(node.pods), size=n_kill, replace=False))
                node.pods = [p for i, p in enumerate(node.pods)
                             if i not in kill_indices]

        # Add phase: place 0-3 new pods if they fit
        n_add = rng.integers(0, 4)
        for _ in range(n_add):
            cpu = float(np.clip(round(rng.lognormal(0.0, 1.0) * 2) / 2, 0.25, 16))
            mem = float(np.clip(round(rng.lognormal(1.5, 1.2)), 1, 120))
            dc = float(rng.choice([0, 1, 1, 1, 1, 1, 10]))
            pod = Pod(cpu=cpu, mem=mem, disruption_cost=dc)
            if node.fits(pod):
                node.pods.append(pod)


def generate_replace_moves(rng, nodes):
    """
    For each node, find the cheapest instance that fits its current pods.
    If cheaper than the current node, that's a REPLACE move.
    """
    moves = []
    nodepool_cost = sum(n.cost_per_hr for n in nodes)
    nodepool_disruption = sum(n.disruption_cost for n in nodes)

    for node in nodes:
        if len(node.pods) == 0:
            continue

        result = cheapest_instance_for(node.pods)
        if result is None:
            continue

        _, _, _, new_cost = result
        savings = node.cost_per_hr - new_cost
        disruption = node.disruption_cost

        if savings <= 0 or disruption <= 0:
            continue
        if nodepool_cost <= 0 or nodepool_disruption <= 0:
            continue

        savings_frac = savings / nodepool_cost
        disruption_frac = disruption / nodepool_disruption
        score = savings_frac / disruption_frac

        moves.append({
            "savings": savings,
            "disruption": disruption,
            "savings_frac": savings_frac,
            "disruption_frac": disruption_frac,
            "score": score,
        })

    return moves


def generate_delete_moves(rng, nodes):
    """
    For each node, check if its pods could fit on other nodes' spare capacity.
    If so, generate a DELETE move (full node cost saved, all pods disrupted).
    """
    moves = []
    nodepool_cost = sum(n.cost_per_hr for n in nodes)
    nodepool_disruption = sum(n.disruption_cost for n in nodes)

    for i, node in enumerate(nodes):
        if len(node.pods) == 0:
            continue

        # Check if all pods fit on spare capacity of other nodes
        sorted_pods = sorted(node.pods, key=lambda p: (p.cpu, p.mem),
                             reverse=True)

        remaining = {}
        for j in range(len(nodes)):
            if j != i:
                remaining[j] = (
                    nodes[j].cpu_cap - nodes[j].cpu_used,
                    nodes[j].mem_cap - nodes[j].mem_used,
                )

        can_fit = True
        for pod in sorted_pods:
            best_j = None
            best_waste = float("inf")
            for j, (cpu_f, mem_f) in remaining.items():
                if cpu_f >= pod.cpu and mem_f >= pod.mem:
                    waste = (cpu_f - pod.cpu) + (mem_f - pod.mem)
                    if waste < best_waste:
                        best_waste = waste
                        best_j = j
            if best_j is not None:
                cpu_f, mem_f = remaining[best_j]
                remaining[best_j] = (cpu_f - pod.cpu, mem_f - pod.mem)
            else:
                can_fit = False
                break

        if not can_fit:
            continue

        savings = node.cost_per_hr
        disruption = node.disruption_cost

        if savings <= 0 or disruption <= 0:
            continue
        if nodepool_cost <= 0 or nodepool_disruption <= 0:
            continue

        savings_frac = savings / nodepool_cost
        disruption_frac = disruption / nodepool_disruption
        score = savings_frac / disruption_frac

        moves.append({
            "savings": savings,
            "disruption": disruption,
            "savings_frac": savings_frac,
            "disruption_frac": disruption_frac,
            "score": score,
        })

    return moves


def plot_ranking(moves, title, output_path):
    """Plot cumulative savings vs disruption for four ranking strategies."""
    if not moves:
        print(f"  No moves for {title}, skipping")
        return

    rng = np.random.default_rng(SEED + 1)
    n = len(moves)

    savings = np.array([m["savings_frac"] for m in moves])
    disruption = np.array([m["disruption_frac"] for m in moves])
    scores = np.array([m["score"] for m in moves])

    strategies = {
        "Score (benefit/cost)": np.argsort(-scores),
        "Savings only": np.argsort(-savings),
        "Disruption only (asc)": np.argsort(disruption),
        "Random": rng.permutation(n),
    }

    fig, ax = plt.subplots(figsize=(8, 6))

    for label, order in strategies.items():
        cum_disruption = np.cumsum(disruption[order])
        cum_savings = np.cumsum(savings[order])
        if cum_disruption[-1] > 0:
            cum_disruption = cum_disruption / cum_disruption[-1]
        if cum_savings[-1] > 0:
            cum_savings = cum_savings / cum_savings[-1]
        ax.plot(cum_disruption, cum_savings, label=label, linewidth=2)

    ax.set_xlabel("Cumulative disruption (fraction of total)")
    ax.set_ylabel("Cumulative savings (fraction of total)")
    ax.set_title(title)
    ax.legend(loc="lower right")
    ax.set_xlim(0, 1)
    ax.set_ylim(0, 1)
    ax.set_aspect("equal")
    ax.grid(True, alpha=0.3)

    fig.tight_layout()
    fig.savefig(output_path, dpi=150)
    print(f"  Saved {output_path}")


def _plot_panel(ax, moves, title):
    """Plot one panel of cumulative savings vs disruption."""
    rng = np.random.default_rng(SEED + 1)
    n = len(moves)

    savings = np.array([m["savings_frac"] for m in moves])
    disruption = np.array([m["disruption_frac"] for m in moves])
    scores = np.array([m["score"] for m in moves])

    strategies = {
        "Score (benefit/cost)": np.argsort(-scores),
        "Savings only": np.argsort(-savings),
        "Disruption only (asc)": np.argsort(disruption),
        "Random": rng.permutation(n),
    }

    for label, order in strategies.items():
        cum_disruption = np.cumsum(disruption[order])
        cum_savings = np.cumsum(savings[order])
        if cum_disruption[-1] > 0:
            cum_disruption = cum_disruption / cum_disruption[-1]
        if cum_savings[-1] > 0:
            cum_savings = cum_savings / cum_savings[-1]
        ax.plot(cum_disruption, cum_savings, label=label, linewidth=2)

    ax.set_xlabel("Cumulative disruption (fraction of total)")
    ax.set_ylabel("Cumulative savings (fraction of total)")
    ax.set_title(title)
    ax.legend(loc="lower right", fontsize=8)
    ax.set_xlim(0, 1)
    ax.set_ylim(0, 1)
    ax.set_aspect("equal")
    ax.grid(True, alpha=0.3)


def plot_ranking_sidebyside(replace_moves, delete_moves, output_path):
    """Plot REPLACE and DELETE ranking charts side by side."""
    fig, (ax1, ax2) = plt.subplots(1, 2, figsize=(14, 6))

    if replace_moves:
        _plot_panel(ax1, replace_moves, "REPLACE moves")
    if delete_moves:
        _plot_panel(ax2, delete_moves, "DELETE moves")

    fig.tight_layout()
    fig.savefig(output_path, dpi=150)
    print(f"  Saved {output_path}")


def main():
    rng = np.random.default_rng(SEED)

    print("Building cluster...")
    nodes = build_cluster(rng, n_pods=5000)
    total_pods = sum(len(n.pods) for n in nodes)
    print(f"  {len(nodes)} nodes, {total_pods} pods, "
          f"{total_pods/len(nodes):.1f} pods/node")
    print(f"  NodePool cost: ${sum(n.cost_per_hr for n in nodes):.2f}/hr")

    # Run multiple rounds of churn to accumulate consolidation candidates.
    # Each round kills some pods and adds new ones, then we collect moves.
    # This simulates a cluster that has been running for a while with
    # ongoing deployments and scale-downs.
    all_replace_moves = []
    all_delete_moves = []
    n_rounds = 10
    print(f"Simulating {n_rounds} rounds of workload churn...")
    for round_num in range(n_rounds):
        churn_pods(rng, nodes)
        nodes = [n for n in nodes if len(n.pods) > 0]
        replace_moves = generate_replace_moves(rng, nodes)
        delete_moves = generate_delete_moves(rng, nodes)
        all_replace_moves.extend(replace_moves)
        all_delete_moves.extend(delete_moves)
        if round_num == 0 or round_num == n_rounds - 1:
            total_pods = sum(len(n.pods) for n in nodes)
            print(f"  Round {round_num+1}: {len(nodes)} nodes, {total_pods} pods, "
                  f"{len(replace_moves)} REPLACE, {len(delete_moves)} DELETE")

    replace_moves = all_replace_moves
    delete_moves = all_delete_moves
    print(f"  Total: {len(replace_moves)} REPLACE, {len(delete_moves)} DELETE")

    # Combine all moves -- this is what the consolidation controller evaluates
    all_moves = replace_moves + delete_moves
    print(f"  Combined: {len(all_moves)} moves")

    print("Plotting...")
    plot_ranking_sidebyside(
        replace_moves, delete_moves,
        OUTPUT_DIR / "ranking-strategies.png",
    )

    # Write CSVs for inspection
    for label, moves in [("all", all_moves),
                         ("replace", replace_moves),
                         ("delete", delete_moves)]:
        csv_path = Path(__file__).parent / f"ranking-strategies-{label}.csv"
        with open(csv_path, "w") as f:
            f.write("savings_per_hr,disruption,savings_frac,disruption_frac,score\n")
            for m in sorted(moves, key=lambda m: -m["score"]):
                f.write(f"{m['savings']:.4f},{m['disruption']:.1f},"
                        f"{m['savings_frac']:.6f},{m['disruption_frac']:.6f},"
                        f"{m['score']:.4f}\n")
        print(f"  Saved {csv_path}")

    print("Done.")


if __name__ == "__main__":
    main()
