#!/usr/bin/env python3
"""
Exhaustive check of consolidation scoring properties.

Decision rule: approved iff k * savings_fraction >= disruption_fraction
Cross-multiplied: k * savings * pool_disruption >= move_disruption * pool_cost

State space:
  - 1-6 nodes
  - Prices from c7i, m7i, r7i on-demand (us-east-1), scaled to integers
  - 0-4 pods per node, disruption cost in {1, 2, 5, 10}
  - Actions: Delete(node), Replace(node, new_price)
  - k: the decision constant (1 = break-even, 2 = savings count 2x)
"""

from itertools import product as cartesian
from collections import defaultdict

# On-demand prices in us-east-1, $/hr × 10000 for exact integer arithmetic.
# c7i (compute-optimized)
#   c7i.medium  $0.0446/hr  →  446
#   c7i.large   $0.0892/hr  →  892
#   c7i.xlarge  $0.1785/hr  → 1785
#   c7i.2xlarge $0.3570/hr  → 3570
#   c7i.4xlarge $0.7140/hr  → 7140
# m7i (general-purpose)
#   m7i.medium  $0.0504/hr  →  504
#   m7i.large   $0.1008/hr  → 1008
#   m7i.xlarge  $0.2016/hr  → 2016
#   m7i.2xlarge $0.4032/hr  → 4032
#   m7i.4xlarge $0.8064/hr  → 8064
# r7i (memory-optimized)
#   r7i.medium  $0.0661/hr  →  661
#   r7i.large   $0.1323/hr  → 1323
#   r7i.xlarge  $0.2646/hr  → 2646
#   r7i.2xlarge $0.5292/hr  → 5292
#   r7i.4xlarge $1.0584/hr  → 10584
PRICES = sorted([
    446, 892, 1785, 3570, 7140,     # c7i
    504, 1008, 2016, 4032, 8064,    # m7i
    661, 1323, 2646, 5292, 10584,   # r7i
])
DCOSTS = [1, 2, 5, 10]
K_VALUES = [1, 2, 3]


def approved(k, savings, move_disruption, pool_cost, pool_disruption):
    """Cross-multiplied decision rule. Handles zero-disruption edge case."""
    if move_disruption == 0:
        return savings > 0
    if pool_disruption == 0:
        return savings > 0
    if pool_cost == 0:
        return False
    return k * savings * pool_disruption >= move_disruption * pool_cost


def score(savings, move_disruption, pool_cost, pool_disruption):
    """Actual score (float). For reporting only, not for decisions."""
    if pool_cost == 0 or pool_disruption == 0:
        return float('inf') if savings > 0 else 0.0
    sf = savings / pool_cost
    df = move_disruption / pool_disruption
    if df == 0:
        return float('inf') if sf > 0 else 0.0
    return sf / df


# ─── P1: Monotonicity in savings ───

def check_p1():
    """If replace(n, q) is approved and p < q (more savings), then replace(n, p) is approved."""
    violations = []
    for k in K_VALUES:
        for n_nodes in range(1, 5):
            for node_price in PRICES:
                for pod_config in pod_configs(max_pods=4):
                    move_disruption = sum(pod_config)
                    # Other nodes: try a few representative configs
                    for other_price in PRICES[:3]:
                        for other_pods in [0, 2, 4]:
                            other_disruption = other_pods * 1  # default cost
                            pool_cost = node_price + (n_nodes - 1) * other_price
                            pool_disruption = move_disruption + (n_nodes - 1) * other_disruption
                            for q in PRICES:
                                if q >= node_price:
                                    continue
                                savings_q = node_price - q
                                if not approved(k, savings_q, move_disruption, pool_cost, pool_disruption):
                                    continue
                                # q is approved. Check all p < q.
                                for p in PRICES:
                                    if p >= q:
                                        continue
                                    savings_p = node_price - p
                                    if not approved(k, savings_p, move_disruption, pool_cost, pool_disruption):
                                        violations.append((k, node_price, q, p, n_nodes))
    return violations


# ─── P2: Monotonicity in disruption ───

def check_p2():
    """Higher disruption on a node should make approval at least as hard."""
    violations = []
    for k in K_VALUES:
        for n_nodes in range(2, 5):
            for price in PRICES:
                for other_price in PRICES[:3]:
                    for other_pods in [0, 2, 4]:
                        other_disruption = other_pods * 1
                        base_pool_cost = price + (n_nodes - 1) * other_price
                        base_other_disruption = (n_nodes - 1) * other_disruption
                        for d_high in range(1, 41):
                            for d_low in range(0, d_high):
                                pool_d_high = d_high + base_other_disruption
                                pool_d_low = d_low + base_other_disruption
                                # Delete savings = price for both
                                app_high = approved(k, price, d_high, base_pool_cost, pool_d_high)
                                app_low = approved(k, price, d_low, base_pool_cost, pool_d_low)
                                if app_high and not app_low:
                                    violations.append((k, price, d_high, d_low, n_nodes))
    return violations


# ─── P3: Empty nodes always approved for delete ───

def check_p3():
    """Node with 0 pods, positive price: delete always approved."""
    violations = []
    for k in K_VALUES:
        for n_nodes in range(1, 7):
            for price in PRICES:
                for other_price in PRICES:
                    for other_pods in range(0, 5):
                        other_disruption = other_pods * 1
                        pool_cost = price + (n_nodes - 1) * other_price
                        pool_disruption = 0 + (n_nodes - 1) * other_disruption
                        if not approved(k, price, 0, pool_cost, pool_disruption):
                            violations.append((k, price, n_nodes))
    return violations


# ─── P4: Zero-savings moves never approved ───

def check_p4():
    """Replace with same price: savings = 0, never approved."""
    violations = []
    for k in K_VALUES:
        for price in PRICES:
            for pod_config in pod_configs(max_pods=4):
                move_disruption = sum(pod_config)
                for n_nodes in range(1, 5):
                    pool_cost = n_nodes * price
                    pool_disruption = n_nodes * move_disruption
                    if approved(k, 0, move_disruption, pool_cost, pool_disruption):
                        violations.append((k, price, pod_config))
    return violations


# ─── P5: Single-node pool replaces ───

def check_p5():
    """For each k, find all approved replaces in single-node pools."""
    results = defaultdict(list)
    for k in K_VALUES:
        for price in PRICES:
            for pod_config in pod_configs(max_pods=4):
                if not pod_config:
                    continue  # empty node, trivial
                move_disruption = sum(pod_config)
                pool_cost = price
                pool_disruption = move_disruption
                for np in PRICES:
                    if np >= price:
                        continue
                    savings = price - np
                    if approved(k, savings, move_disruption, pool_cost, pool_disruption):
                        s = score(savings, move_disruption, pool_cost, pool_disruption)
                        results[k].append({
                            'price': price, 'replacement': np,
                            'pods': pod_config, 'disruption': move_disruption,
                            'savings_pct': 100 * savings / price,
                            'score': s,
                        })
    return results


# ─── P6: Skewed disruption cost ───

def check_p6():
    """Find pools where high-disruption node is rejected but low-disruption is approved."""
    examples = []
    for k in [1, 2, 3]:
        for n_nodes in range(2, 5):
            for price in PRICES:
                for d_high_pods in pod_configs(max_pods=4):
                    for d_low_pods in pod_configs(max_pods=4):
                        d_high = sum(d_high_pods)
                        d_low = sum(d_low_pods)
                        if d_high <= d_low or d_low == 0:
                            continue
                        other_disruption = (n_nodes - 1) * 4 * 1  # other nodes: 4 default pods
                        pool_cost = n_nodes * price
                        # Both nodes are in the pool
                        pool_disruption = d_high + d_low + (n_nodes - 2) * 4 * 1 if n_nodes > 2 else d_high + d_low
                        app_high = approved(k, price, d_high, pool_cost, pool_disruption)
                        app_low = approved(k, price, d_low, pool_cost, pool_disruption)
                        if app_low and not app_high:
                            examples.append({
                                'k': k, 'price': price, 'n_nodes': n_nodes,
                                'high_pods': d_high_pods, 'low_pods': d_low_pods,
                                'd_high': d_high, 'd_low': d_low,
                            })
                            if len(examples) >= 3:
                                return examples
    return examples


# ─── P7: Fleet size independence ───

def check_p7():
    """For uniform pools, check that the decision doesn't change with pool size."""
    violations = []
    for k in K_VALUES:
        for price in PRICES:
            for pod_config in pod_configs(max_pods=4):
                if not pod_config:
                    continue
                move_disruption = sum(pod_config)
                for np in PRICES:
                    if np >= price:
                        continue
                    savings = price - np
                    decisions = {}
                    for n_nodes in range(1, 7):
                        pool_cost = n_nodes * price
                        pool_disruption = n_nodes * move_disruption
                        decisions[n_nodes] = approved(k, savings, move_disruption, pool_cost, pool_disruption)
                    vals = set(decisions.values())
                    if len(vals) > 1:
                        violations.append((k, price, np, pod_config, decisions))
    return violations


# ─── P8: Churn chains ───

def check_p8():
    """Find the longest replace chain via exhaustive DFS over all approved next-steps.

    At each step, every cheaper price that the scoring formula approves is a valid
    next node in the chain.  DFS explores all of them and returns the true maximum
    chain length — not just the greedy "least aggressive step" path, which could
    miss longer cross-family zigzag paths."""
    results = defaultdict(list)
    for k in K_VALUES:
        for pod_config in pod_configs(max_pods=4):
            if not pod_config:
                continue
            move_disruption = sum(pod_config)
            for n_nodes in range(1, 5):
                for start_price in PRICES:
                    other_cost = (n_nodes - 1) * start_price
                    other_disruption = (n_nodes - 1) * move_disruption
                    longest_chain = _dfs_longest_chain(
                        k, start_price, [start_price],
                        move_disruption, other_cost, other_disruption,
                    )
                    if len(longest_chain) > 1:
                        results[k].append({
                            'n_nodes': n_nodes, 'pods': pod_config,
                            'chain': longest_chain, 'steps': len(longest_chain) - 1,
                        })
    # Keep only the longest chains per k
    for k in results:
        results[k].sort(key=lambda x: -x['steps'])
        results[k] = results[k][:5]
    return results


def _dfs_longest_chain(k, current_price, chain, move_disruption, other_cost, other_disruption):
    """Return the longest chain reachable from current_price via any sequence of approved replaces."""
    best = list(chain)
    for np in PRICES:
        if np >= current_price:
            continue
        savings = current_price - np
        pool_cost = current_price + other_cost
        pool_disruption = move_disruption + other_disruption
        if approved(k, savings, move_disruption, pool_cost, pool_disruption):
            candidate = _dfs_longest_chain(
                k, np, chain + [np],
                move_disruption, other_cost, other_disruption,
            )
            if len(candidate) > len(best):
                best = candidate
    return best


# ─── Utility ───

def pod_configs(max_pods=4):
    """Generate representative pod configurations (tuples of disruption costs)."""
    configs = [()]  # empty
    for n in range(1, max_pods + 1):
        # Don't enumerate all combos — use representative configs
        for d in DCOSTS:
            configs.append(tuple([d] * n))  # uniform
        if n >= 2:
            configs.append((1, 10))  # mixed
            configs.append((1, 1, 10))  # mostly low, one high
            configs.append((10, 10, 1))  # mostly high, one low
        if n >= 3:
            configs.append((1, 5, 10))  # spread
        if n == 4:
            configs.append((1, 1, 1, 10))
            configs.append((1, 2, 5, 10))
            configs.append((10, 10, 10, 1))
    return configs


# ─── Main ───

def main():
    print("=" * 70)
    print("Consolidation Scoring Properties — Exhaustive Check")
    print("=" * 70)

    print("\n── P1: Monotonicity in savings ──")
    v = check_p1()
    if v:
        print(f"  FAIL: {len(v)} violations found!")
        for x in v[:3]:
            print(f"    k={x[0]} price={x[1]} approved_at={x[2]} rejected_at={x[3]} nodes={x[4]}")
    else:
        print("  PASS: no violations")

    print("\n── P2: Monotonicity in disruption ──")
    v = check_p2()
    if v:
        print(f"  FAIL: {len(v)} violations found!")
        for x in v[:3]:
            print(f"    k={x[0]} price={x[1]} d_high={x[2]} d_low={x[3]} nodes={x[4]}")
    else:
        print("  PASS: no violations")

    print("\n── P3: Empty nodes always approved for delete ──")
    v = check_p3()
    if v:
        print(f"  FAIL: {len(v)} violations found!")
        for x in v[:3]:
            print(f"    k={x[0]} price={x[1]} nodes={x[2]}")
    else:
        print("  PASS: no violations")

    print("\n── P4: Zero-savings moves never approved ──")
    v = check_p4()
    if v:
        print(f"  FAIL: {len(v)} violations found!")
        for x in v[:3]:
            print(f"    k={x[0]} price={x[1]} pods={x[2]}")
    else:
        print("  PASS: no violations")

    print("\n── P5: Single-node pool replaces ──")
    results = check_p5()
    for k in K_VALUES:
        moves = results.get(k, [])
        pairs = set((m['price'], m['replacement']) for m in moves)
        print(f"  k={k}: {len(pairs)} unique price pairs, {len(moves)} configurations")
        for m in moves[:5]:
            print(f"    {m['price']} → {m['replacement']}  "
                  f"saves {m['savings_pct']:.0f}%  "
                  f"disruption={m['disruption']}  "
                  f"score={m['score']:.2f}")

    print("\n── P6: Skewed disruption cost ──")
    examples = check_p6()
    if examples:
        for ex in examples[:3]:
            print(f"  k={ex['k']} price={ex['price']}¢ nodes={ex['n_nodes']}")
            print(f"    high-disruption pods {ex['high_pods']} (d={ex['d_high']}): REJECTED")
            print(f"    low-disruption pods {ex['low_pods']} (d={ex['d_low']}): APPROVED")
    else:
        print("  No examples found")

    print("\n── P7: Fleet size independence (uniform pools) ──")
    v = check_p7()
    if v:
        print(f"  FAIL: {len(v)} violations!")
        for x in v[:3]:
            print(f"    k={x[0]} price={x[1]} replacement={x[2]} pods={x[3]}")
            print(f"    decisions by pool size: {x[4]}")
    else:
        print("  PASS: decision is identical across pool sizes 1-6")

    print("\n── P8: Churn chains (with actual post-replace pool updates) ──")
    results = check_p8()
    for k in K_VALUES:
        chains = results.get(k, [])
        if chains:
            longest = chains[0]
            print(f"  k={k}: longest chain = {longest['steps']} steps")
            for c in chains[:3]:
                arrow = " → ".join(f"{p}¢" for p in c['chain'])
                print(f"    nodes={c['n_nodes']} pods={c['pods']} chain: {arrow}")
        else:
            print(f"  k={k}: no churn (no replaces approved)")

    print("\n" + "=" * 70)
    print("Done.")


if __name__ == "__main__":
    main()
