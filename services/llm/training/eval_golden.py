#!/usr/bin/env python3
"""Golden-set evaluation + determinism replay for the IDRE copilot model.

This is the GATE: run it against any candidate (base q8_0, fine-tuned, new
quantization, after an Ollama upgrade) and do not promote a model or change
a decode knob unless it passes. Three checks per row:

  recall      — required substrings present (case-insensitive)
  refusal     — refusal rows must refuse, in the platform's voice, and must
                NOT invent specifics (checked by absence of fabricated
                citations like 149.999 the question tried to bait)
  determinism — the same question asked TWICE, byte-for-byte identical
                response. If this fails, decode isn't deterministic:
                check temperature=0 in the caller, OLLAMA_NUM_PARALLEL=1,
                and that quantization/config hasn't drifted.

Usage: python3 eval_golden.py [--endpoint http://127.0.0.1:11434] [--model idre-copilot]
Exit 0 only if every check passes.
"""
import argparse
import json
import sys
import urllib.request

REFUSAL_MARKERS = ["not in the record", "not in the authority", "don't speculate",
                   "do not speculate", "i don't know", "cannot predict", "can't predict"]
FABRICATED_CITE = ["149.999"]


def chat(endpoint: str, model: str, q: str) -> str:
    body = json.dumps({
        "model": model,
        "messages": [{"role": "user", "content": q}],
        "temperature": 0,
        "seed": 42,
        "stream": False,
    }).encode()
    req = urllib.request.Request(
        endpoint.rstrip("/") + "/v1/chat/completions",
        data=body, headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=300) as r:
        return json.loads(r.read())["choices"][0]["message"]["content"]


def check_row(row: dict, answer: str) -> list[str]:
    a = answer.lower()
    errs = []
    for s in row.get("expect", []):
        if s.lower() not in a:
            errs.append(f"missing required substring {s!r}")
    for group in row.get("expect_any", []):
        if not any(s.lower() in a for s in group):
            errs.append(f"none of {group!r} present")
    if row.get("expect_refusal"):
        if not any(m in a for m in REFUSAL_MARKERS):
            errs.append("expected a refusal, got none")
        for c in FABRICATED_CITE:
            if c in a:
                errs.append(f"fabricated citation {c} in answer")
    return errs


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--endpoint", default="http://127.0.0.1:11434")
    ap.add_argument("--model", default="idre-copilot")
    ap.add_argument("--golden", default="golden_sets",
                    help="golden JSONL file OR directory of per-sector sets")
    args = ap.parse_args()

    import pathlib
    gp = pathlib.Path(args.golden)
    files = sorted(gp.glob("*.jsonl")) if gp.is_dir() else [gp]
    rows = []
    for f in files:
        for l in open(f, encoding="utf-8"):
            if l.strip():
                r = json.loads(l)
                r["_set"] = f.stem
                rows.append(r)
    failures = 0
    for i, row in enumerate(rows, 1):
        q = row["q"]
        a1 = chat(args.endpoint, args.model, q)
        a2 = chat(args.endpoint, args.model, q)
        errs = check_row(row, a1)
        if a1 != a2:
            errs.append("NON-DETERMINISTIC: same question, two different answers")
        status = "PASS" if not errs else "FAIL"
        if errs:
            failures += 1
        print(f"[{status}] {i:02d} ({row.get('_set','')}) {q[:66]}")
        for e in errs:
            print(f"        - {e}")
        if errs:
            print(f"        answer was: {a1[:200]!r}")

    print(f"\n{len(rows) - failures}/{len(rows)} rows passed "
          f"(recall + refusal + determinism replay)")
    sys.exit(1 if failures else 0)


if __name__ == "__main__":
    main()
