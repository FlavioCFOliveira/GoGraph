#!/usr/bin/env python3
"""parse_lab_noise.py — the parsing laboratory's noise floor.

    python3 scripts/parse_lab_noise.py RUN_A RUN_B

Compares two runs of scripts/parse-lab.sh over a byte-identical tree. Same
source, same command, so every difference it reports is measurement noise, not
a change in the code. A later A/B delta smaller than the floor printed here is
not a finding.

It reports, per stage, the distribution of |B - A| / A over the per-statement
median ns/op, and names the worst statement.

Read the derived stages with care. `Parse` and `Visit` are differences between
cumulative prefixes, so their operands' noise compounds; a stage that is a
small difference between two large prefixes has a far wider floor than one
measured directly.
"""
import csv
import statistics
import sys

COLS = ["Strip", "Guard", "Normalize", "Lex", "Parse", "Visit",
        "Full", "Sema", "PrepareCold", "PrepareWarm"]


def load(path):
    with open(path, newline="", encoding="utf-8") as fh:
        return {r["statement"]: r for r in csv.DictReader(fh)}


def main():
    if len(sys.argv) != 3:
        print(__doc__)
        sys.exit(2)
    a = load(sys.argv[1] + "/stages.csv")
    b = load(sys.argv[2] + "/stages.csv")
    print("Noise floor: two runs of the same command over a byte-identical tree.")
    print(f"  A = {sys.argv[1]}")
    print(f"  B = {sys.argv[2]}")
    print("Per (stage, statement) median ns/op; |B-A|/A.")
    print()
    print(f"{'stage':<13}{'n':>4}{'median':>9}{'p90':>9}{'max':>9}   worst statement")
    everything = []
    for c in COLS:
        ps, worst = [], (0.0, None)
        for k in a:
            if k not in b:
                continue
            try:
                x, y = float(a[k][f"{c}.ns"]), float(b[k][f"{c}.ns"])
            except (KeyError, TypeError, ValueError):
                continue
            if x <= 0:
                continue
            p = abs(y - x) / x
            ps.append(p)
            if p > worst[0]:
                worst = (p, k)
        if not ps:
            continue
        everything += ps
        ps.sort()
        p90 = ps[int(0.9 * (len(ps) - 1))]
        print(f"{c:<13}{len(ps):>4}{100 * statistics.median(ps):>8.2f}%"
              f"{100 * p90:>8.2f}%{100 * max(ps):>8.2f}%   {worst[1]} ({100 * worst[0]:.2f}%)")
    everything.sort()
    print()
    print(f"{'ALL':<13}{len(everything):>4}{100 * statistics.median(everything):>8.2f}%"
          f"{100 * everything[int(0.9 * (len(everything) - 1))]:>8.2f}%"
          f"{100 * max(everything):>8.2f}%")


if __name__ == "__main__":
    main()
