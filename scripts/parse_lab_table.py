#!/usr/bin/env python3
"""parse_lab_table.py — turn the parsing laboratory's raw benchmark logs into a
per-stage table.

Reads OUT_DIR/stage.txt and OUT_DIR/prepare.txt (the verbatim `go test -bench`
output, which is also benchstat's input format) and writes:

  OUT_DIR/stages.csv  — one row per (statement, stage): median ns/op, B/op, allocs/op
  OUT_DIR/stages.md   — the same, as a readable table, plus a corpus-wide summary

Lex, parse and visit are measured as cumulative prefixes because each consumes
what the previous one built (see cypher/parser/stage_bench_test.go). Their own
costs are therefore reported here as differences between consecutive prefixes,
and labelled as such. Every other stage is measured directly.

The median across the -count repetitions is used, never the mean: a single
descheduled repetition moves a mean and does not move a median.
"""
import csv
import json
import os
import re
import statistics
import sys

LINE = re.compile(
    r"^Benchmark(?P<bench>\S+?)-(?P<procs>\d+)\s+"
    r"(?P<n>\d+)\s+"
    r"(?P<ns>[\d.]+) ns/op\s+"
    r"(?P<bytes>\d+) B/op\s+"
    r"(?P<allocs>\d+) allocs/op\s*$"
)

# Stages measured directly, in the order the front end runs them.
DIRECT = ["Strip", "Guard", "Normalize", "Lex"]
# Stages that exist only as a difference between two cumulative prefixes.
DERIVED = [("Parse", "LexParse", "Lex"), ("Visit", "LexParseVisit", "LexParse")]
PREFIX = ["LexParse", "LexParseVisit"]


def read(path):
    """samples[(series, statement)] = list of (ns, bytes, allocs)."""
    out = {}
    if not os.path.exists(path):
        return out
    with open(path, encoding="utf-8", errors="replace") as fh:
        for raw in fh:
            m = LINE.match(raw.strip())
            if not m:
                continue
            parts = m.group("bench").split("/")
            if len(parts) == 3:          # FrontEndStage/<Stage>/<id>
                series, stmt = parts[1], parts[2]
            elif len(parts) == 2:        # PrepareCold/<id>
                series, stmt = parts[0], parts[1]
            else:
                continue
            if series == "StageSema":
                series = "Sema"
            out.setdefault((series, stmt), []).append(
                (float(m.group("ns")), int(m.group("bytes")), int(m.group("allocs")))
            )
    return out


def med(samples, key, idx):
    vals = [s[idx] for s in samples.get(key, [])]
    return statistics.median(vals) if vals else None


def main():
    out_dir = sys.argv[1]
    samples = read(os.path.join(out_dir, "stage.txt"))
    samples.update(read(os.path.join(out_dir, "prepare.txt")))

    corpus_path = "cypher/parser/testdata/examples-corpus.json"
    with open(corpus_path, encoding="utf-8") as fh:
        corpus = json.load(fh)["statements"]
    order = [(s["id"].replace("/", "-"), s) for s in corpus]

    cols = DIRECT + [d[0] for d in DERIVED] + ["Full", "Sema", "PrepareCold", "PrepareWarm"]
    rows = []
    for name, meta in order:
        row = {
            "statement": name,
            "example": meta["example"],
            "source": meta["source"],
            "bytes": meta["bytes"],
            "shape": " ".join(meta["shape"]),
        }
        for c in DIRECT + PREFIX + ["Full", "Sema", "PrepareCold", "PrepareWarm"]:
            for label, idx in (("ns", 0), ("B", 1), ("allocs", 2)):
                row[f"{c}.{label}"] = med(samples, (c, name), idx)
        for dname, hi, lo in DERIVED:
            for label, idx in (("ns", 0), ("B", 1), ("allocs", 2)):
                a, b = row.get(f"{hi}.{label}"), row.get(f"{lo}.{label}")
                row[f"{dname}.{label}"] = None if a is None or b is None else a - b
        row["reps"] = len(samples.get(("Full", name), []))
        rows.append(row)

    field_order = ["statement", "example", "source", "bytes", "shape", "reps"]
    for c in cols:
        field_order += [f"{c}.ns", f"{c}.B", f"{c}.allocs"]
    for c in PREFIX:
        field_order += [f"{c}.ns", f"{c}.B", f"{c}.allocs"]

    csv_path = os.path.join(out_dir, "stages.csv")
    with open(csv_path, "w", newline="", encoding="utf-8") as fh:
        w = csv.DictWriter(fh, fieldnames=field_order, extrasaction="ignore")
        w.writeheader()
        for r in rows:
            w.writerow(r)

    def fmt(v, prec=1):
        return "—" if v is None else f"{v:,.{prec}f}"

    md = []
    md.append("# Cypher front end — per-stage cost over the examples statement corpus\n")
    with open(os.path.join(out_dir, "env.txt"), encoding="utf-8") as fh:
        md.append("```\n" + fh.read().rstrip() + "\n```\n")
    md.append("Median of the `-count` repetitions. `Parse` and `Visit` are differences "
              "between cumulative prefixes (`LexParse - Lex`, `LexParseVisit - LexParse`); "
              "every other column is measured directly.\n")

    md.append("\n## Corpus-wide summary (median over the %d statements)\n" % len(rows))
    md.append("| stage | ns/op | B/op | allocs/op | share of Full |")
    md.append("|---|---:|---:|---:|---:|")
    full_ns = [r["Full.ns"] for r in rows if r["Full.ns"] is not None]
    full_med = statistics.median(full_ns) if full_ns else None
    for c in cols:
        ns = [r[f"{c}.ns"] for r in rows if r.get(f"{c}.ns") is not None]
        by = [r[f"{c}.B"] for r in rows if r.get(f"{c}.B") is not None]
        al = [r[f"{c}.allocs"] for r in rows if r.get(f"{c}.allocs") is not None]
        if not ns:
            continue
        m = statistics.median(ns)
        share = "—" if not full_med else f"{100.0 * m / full_med:.1f}%"
        if c in ("Full", "PrepareCold", "PrepareWarm", "Sema"):
            share = "—" if c != "Full" else "100.0%"
        md.append(f"| {c} | {fmt(m,1)} | {fmt(statistics.median(by),0)} | "
                  f"{fmt(statistics.median(al),1)} | {share} |")

    md.append("\n## Per statement\n")
    head = "| statement | bytes | " + " | ".join(
        f"{c} ns/B/allocs" for c in cols) + " | source |"
    md.append(head)
    md.append("|---|---:|" + "---:|" * len(cols) + "---|")
    for r in rows:
        cells = []
        for c in cols:
            cells.append(f"{fmt(r.get(f'{c}.ns'),0)} / {fmt(r.get(f'{c}.B'),0)} / {fmt(r.get(f'{c}.allocs'),1)}")
        md.append(f"| {r['statement']} | {r['bytes']} | " + " | ".join(cells) + f" | {r['source']} |")

    with open(os.path.join(out_dir, "stages.md"), "w", encoding="utf-8") as fh:
        fh.write("\n".join(md) + "\n")

    missing = [r["statement"] for r in rows if r["Full.ns"] is None]
    print(f"stages.csv and stages.md written for {len(rows)} statements; "
          f"{len(missing)} without a Full measurement")
    if missing:
        print("missing:", ", ".join(missing[:10]))


if __name__ == "__main__":
    main()
