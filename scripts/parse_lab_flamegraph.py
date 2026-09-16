#!/usr/bin/env python3
"""parse_lab_flamegraph.py — fold a pprof profile into stacks and render a
standalone flame graph (rmp #2844, sprint 362 parsing laboratory).

This host has no graphviz, so `go tool pprof -svg` cannot run. The flame graph
is the view the campaign actually needs — it reads the hot call stacks at a
glance and it shows where the ANTLR runtime sits inside them — so it is built
here from `go tool pprof -traces`, which needs nothing but the Go toolchain.

Usage:
    scripts/parse_lab_flamegraph.py PROFILE OUT_PREFIX [--sample=IDX] [--focus=RE]

--focus is passed straight to `go tool pprof -focus`, which keeps only the
samples whose stack contains a matching frame. Use it to restrict the split to
the benchmark's own working stack: a Go CPU profile also carries the samples of
the scheduler, the netpoller, the GC and the scavenger, which are NOT inside
the front end's call stack and would otherwise dilute every share. Capture both
views — focused for "what is the front end made of", unfocused for "what does
the front end cost the runtime around it".

Writes:
    OUT_PREFIX.folded   collapsed stacks, "root;...;leaf <value>" per line
                        (the standard flame-graph interchange format)
    OUT_PREFIX.svg      a self-contained flame graph, no external assets
    OUT_PREFIX.pkg.txt  SELF (flat) value bucketed by owning package

Frames are coloured by owning package, because the campaign's decisive
question is how much of the parse is the ANTLR runtime rather than GoGraph:

    red     github.com/antlr4-go/antlr/v4       the ANTLR runtime
    orange  cypher/parser/gen                   GoGraph's ANTLR-GENERATED code
    blue    cypher/parser (hand-written)        GoGraph's own front-end code
    green   other GoGraph packages
    grey    runtime, standard library, everything else
"""

import html
import os
import re
import subprocess
import sys

SEP = re.compile(r"^-+\+-+$")
# "      10ms   runtime.usleep"  /  "     512kB   ..."  /  "         3   ..."
HEAD = re.compile(r"^\s*([0-9.]+)([a-zA-Z]*)\s\s+(\S.*)$")
CONT = re.compile(r"^\s+(\S.*)$")

TIME = {"ns": 1, "us": 1e3, "µs": 1e3, "ms": 1e6, "s": 1e9,
        "m": 60e9, "h": 3600e9}
SIZE = {"B": 1, "kB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12}

ANTLR = "github.com/antlr4-go/antlr/"
GEN = "github.com/FlavioCFOliveira/GoGraph/cypher/parser/gen."
PARSER = "github.com/FlavioCFOliveira/GoGraph/cypher/parser."
GOGRAPH = "github.com/FlavioCFOliveira/GoGraph/"

BUCKETS = [
    ("antlr-runtime", ANTLR, "#d62728"),
    ("gograph-parser-gen", GEN, "#ff7f0e"),
    ("gograph-parser-handwritten", PARSER, "#1f77b4"),
    ("gograph-other", GOGRAPH, "#2ca02c"),
]
OTHER = ("runtime-stdlib-other", "#9e9e9e")


def parse_value(num, unit):
    """Return the sample value in its base unit (ns, bytes, or a count)."""
    n = float(num)
    if unit in TIME:
        return n * TIME[unit]
    if unit in SIZE:
        return n * SIZE[unit]
    return n


def bucket(frame):
    for name, prefix, colour in BUCKETS:
        if frame.startswith(prefix):
            return name, colour
    return OTHER


def fold(traces_text):
    """Collapse `go tool pprof -traces` output into {folded_stack: value}."""
    stacks, cur, val = {}, [], None
    unit = None
    for line in traces_text.splitlines():
        if SEP.match(line.strip()):
            if cur and val is not None:
                stacks[";".join(reversed(cur))] = stacks.get(";".join(reversed(cur)), 0) + val
            cur, val = [], None
            continue
        m = HEAD.match(line)
        if m and val is None and not line.startswith("    " * 3):
            val = parse_value(m.group(1), m.group(2))
            if unit is None:
                unit = m.group(2)
            cur = [m.group(3).strip()]
            continue
        m = CONT.match(line)
        if m and val is not None:
            cur.append(m.group(1).strip())
    if cur and val is not None:
        key = ";".join(reversed(cur))
        stacks[key] = stacks.get(key, 0) + val
    return stacks, unit or ""


def build_tree(stacks):
    root = {"name": "all", "value": 0, "children": {}}
    for stack, value in stacks.items():
        root["value"] += value
        node = root
        for frame in stack.split(";"):
            child = node["children"].get(frame)
            if child is None:
                child = {"name": frame, "value": 0, "children": {}}
                node["children"][frame] = child
            child["value"] += value
            node = child
    return root


def self_by_bucket(stacks):
    """SELF (flat) value per bucket: the leaf frame owns the sample."""
    totals, total = {}, 0.0
    for stack, value in stacks.items():
        leaf = stack.split(";")[-1]
        name, _ = bucket(leaf)
        totals[name] = totals.get(name, 0.0) + value
        total += value
    return totals, total


TOPHDR = re.compile(r"^\s*flat\s+flat%\s+sum%\s+cum\s+cum%")
# "     0.03s  1.80%  1.80%      0.03s  1.80%  runtime.foo (inline)"
TOPROW = re.compile(
    r"^\s*([0-9.]+)([a-zA-Z]*)\s+[0-9.]+%\s+[0-9.]+%\s+([0-9.]+)([a-zA-Z]*)\s+[0-9.]+%\s+(\S.*)$"
)


def self_by_bucket_from_top(top_text):
    """SELF value per bucket, read from `go tool pprof -top`'s FLAT column.

    This is an INDEPENDENT second route to the same answer as
    self_by_bucket: that one folds `-traces` and attributes each sample to its
    leaf frame, this one reads the flat column pprof computed itself. They must
    agree; the .pkg.txt file prints both and the delta, because a bucketing
    error is exactly the kind of mistake that produces a plausible wrong number.
    """
    totals, total, in_table = {}, 0.0, False
    for line in top_text.splitlines():
        if TOPHDR.match(line):
            in_table = True
            continue
        if not in_table:
            continue
        m = TOPROW.match(line)
        if not m:
            continue
        flat = parse_value(m.group(1), m.group(2))
        name = m.group(5).strip()
        for suffix in (" (inline)", " (partial-inline)"):
            if name.endswith(suffix):
                name = name[: -len(suffix)]
        bname, _ = bucket(name)
        totals[bname] = totals.get(bname, 0.0) + flat
        total += flat
    return totals, total


ROW = 16
PAD = 3


def render(root, title, unit, width=1400):
    rows = []

    def walk(node, x, depth, total):
        w = node["value"] / total * (width - 20) if total else 0
        if w >= 0.12:  # keep hairlines: a 0.12px frame is still a real stack
            rows.append((node, x, depth, w))
        cx = x
        for child in sorted(node["children"].values(), key=lambda c: -c["value"]):
            walk(child, cx, depth + 1, total)
            cx += child["value"] / total * (width - 20) if total else 0

    walk(root, 10, 0, root["value"])
    depth_max = max((d for _, _, d, _ in rows), default=0)
    height = (depth_max + 2) * ROW + 60

    def fmt(v):
        if unit in TIME:
            return f"{v / 1e6:.2f} ms"
        if unit in SIZE:
            return f"{v / 1e6:.3f} MB"
        return f"{v:.0f}"

    out = [
        f'<svg xmlns="http://www.w3.org/2000/svg" width="{width}" height="{height}" '
        f'viewBox="0 0 {width} {height}" font-family="Menlo,monospace" font-size="11">',
        f'<rect width="{width}" height="{height}" fill="#ffffff"/>',
        f'<text x="{width // 2}" y="20" text-anchor="middle" font-size="14">'
        f'{html.escape(title)}</text>',
        f'<text x="{width // 2}" y="36" text-anchor="middle" font-size="10" fill="#555">'
        f'total {fmt(root["value"])} — red=antlr runtime, orange=generated parser, '
        f'blue=hand-written parser, green=other GoGraph, grey=runtime/stdlib</text>',
    ]
    for node, x, depth, w in rows:
        y = 48 + depth * ROW
        _, colour = bucket(node["name"])
        if node["name"] == "all":
            colour = "#cccccc"
        pct = node["value"] / root["value"] * 100 if root["value"] else 0
        tip = f'{node["name"]} — {fmt(node["value"])} ({pct:.2f}%)'
        out.append(
            f'<g><title>{html.escape(tip)}</title>'
            f'<rect x="{x:.2f}" y="{y}" width="{max(w - 0.5, 0.1):.2f}" height="{ROW - 1}" '
            f'fill="{colour}" fill-opacity="0.85" stroke="#ffffff" stroke-width="0.3"/>'
        )
        if w > 40:
            label = node["name"].split("/")[-1]
            maxc = int((w - 6) / 6.0)
            if len(label) > maxc:
                label = label[: max(maxc - 1, 0)] + "…"
            out.append(
                f'<text x="{x + PAD:.2f}" y="{y + ROW - 5}" fill="#000">'
                f'{html.escape(label)}</text>'
            )
        out.append("</g>")
    out.append("</svg>")
    return "\n".join(out)


def main():
    if len(sys.argv) < 3:
        sys.exit(__doc__)
    profile, prefix = sys.argv[1], sys.argv[2]
    sample, focus = "", ""
    for arg in sys.argv[3:]:
        if arg.startswith("--sample="):
            sample = arg.split("=", 1)[1]
        elif arg.startswith("--focus="):
            focus = arg.split("=", 1)[1]
        else:
            sys.exit(f"unknown argument {arg!r}")

    cmd = ["go", "tool", "pprof"]
    if sample:
        cmd.append(f"-sample_index={sample}")
    if focus:
        cmd.append(f"-focus={focus}")
    cmd += ["-traces", profile]
    res = subprocess.run(cmd, capture_output=True, text=True, check=False)
    if res.returncode != 0:
        sys.exit(f"go tool pprof -traces failed ({res.returncode}): {res.stderr}")

    stacks, unit = fold(res.stdout)
    if not stacks:
        sys.exit(f"no stacks folded from {profile}: the profile is empty")

    os.makedirs(os.path.dirname(os.path.abspath(prefix)) or ".", exist_ok=True)
    with open(prefix + ".folded", "w") as f:
        for stack, value in sorted(stacks.items(), key=lambda kv: -kv[1]):
            f.write(f"{stack} {value:.0f}\n")

    totals, total = self_by_bucket(stacks)

    # -nodefraction=0 is MANDATORY. pprof's default is 0.005, which silently
    # drops every node below 0.5% of the total: measured here, that alone put
    # 16.04s against the folded route's 20.70s on the same profile. With it at
    # zero the two routes agree to the last sample.
    topcmd = ["go", "tool", "pprof", "-top", "-nodecount=100000", "-nodefraction=0"]
    if sample:
        topcmd.append(f"-sample_index={sample}")
    if focus:
        topcmd.append(f"-focus={focus}")
    topcmd.append(profile)
    topres = subprocess.run(topcmd, capture_output=True, text=True, check=False)
    ttotals, ttotal = self_by_bucket_from_top(topres.stdout if topres.returncode == 0 else "")

    order = [b[0] for b in BUCKETS] + [OTHER[0]]
    with open(prefix + ".pkg.txt", "w") as f:
        f.write(f"profile={profile}\nsample_index={sample or 'default'}\n")
        f.write(f"focus={focus or 'none'}\nunit={unit}\n")
        f.write(f"total_self_folded={total:.0f}\ntotal_self_top={ttotal:.0f}\n")
        f.write(f"stacks={len(stacks)}\n\n")
        f.write("SELF (flat) value per owning package, by two independent routes:\n")
        f.write("  folded = `pprof -traces`, each sample charged to its leaf frame\n")
        f.write("  top    = `pprof -top -nodecount=100000 -nodefraction=0`, pprof's flat column\n")
        f.write("They must agree. A non-zero delta is a bucketing error, not a finding.\n\n")
        f.write(f"{'bucket':<30}{'folded':>15}{'share':>9}{'top':>15}{'share':>9}{'delta':>12}\n")
        for name in order:
            v = totals.get(name, 0.0)
            t = ttotals.get(name, 0.0)
            f.write(
                f"{name:<30}{v:>15.0f}{(v / total * 100 if total else 0):>8.2f}%"
                f"{t:>15.0f}{(t / ttotal * 100 if ttotal else 0):>8.2f}%{t - v:>12.0f}\n"
            )
        f.write(f"\n{'TOTAL':<30}{total:>15.0f}{'':>9}{ttotal:>15.0f}\n")
        if topres.returncode != 0:
            f.write(f"\nWARNING: pprof -top failed ({topres.returncode}); the cross-check is absent.\n")
            f.write(topres.stderr)

    title = os.path.basename(prefix)
    if sample:
        title += f" [{sample}]"
    if focus:
        title += f" [focus {focus}]"
    with open(prefix + ".svg", "w") as f:
        f.write(render(build_tree(stacks), title, unit))

    print(f"{prefix}.folded  {prefix}.svg  {prefix}.pkg.txt  ({len(stacks)} stacks, unit={unit})")


if __name__ == "__main__":
    main()
