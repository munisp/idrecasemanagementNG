#!/usr/bin/env python3
"""Latency benchmark for the copilot's bounded Ollama call.

Budget: every conversational surface must answer in under 60 seconds
end to end. This script replays the three production prompt shapes
(briefing narration, party-safe chat, intake extraction) against the
local Ollama and reports per-shape latency and generation throughput.

Usage:
    python3 bench_ollama.py [--endpoint http://127.0.0.1:11434/v1]
                            [--model idre-copilot] [--budget 60]

Exit code is non-zero if any shape exceeds the budget, so the gate can
run in CI or as a pre-promotion check alongside eval_golden.py.

Interpretation guide (see deploy/ollama/README.md):
  - high prompt-eval time  -> prompt too large or CPU-bound prefill;
                              check `ollama ps` for 100% GPU offload
  - low tok/s generation   -> quantization too heavy for the host;
                              step down q8_0 -> q4_K_M, re-run eval gate
  - first call slow only   -> cold model load; KEEP_ALIVE=-1 must be set
"""
import argparse
import json
import sys
import time
import urllib.request

DIGEST = {
    "worker": "case.worker@example", "my_open_cases": 6, "at_risk_sla": 2,
    "new_docs_24h": 3, "checks_review": 1, "pending_qa": 1, "tasks_due": 2,
    "cases": [
        {"case_number": "FL-2026-04117", "status": "OPEN", "lane": "STANDARD", "sla_days": 2},
        {"case_number": "FL-2026-04102", "status": "REVIEW", "lane": "STANDARD", "sla_days": 4},
    ],
}

SHAPES = {
    "briefing": {
        "system": "You narrate a case worker's morning briefing. Use ONLY the "
                  "JSON digest below. Under 120 words. Lead with SLA risk.",
        "user": "DIGEST (JSON, platform-verified):\n" + json.dumps(DIGEST),
        "max_tokens": 400,
    },
    "party_chat": {
        "system": "You answer a disputing party using ONLY the JSON facts "
                  "below. Never discuss internal review, QA drafts, staffing, "
                  "other cases, or dollar amounts.",
        "user": 'FACTS: {"case_number":"FL-2026-04117","status":"OPEN",'
                '"outstanding_requests":["carrier response due"],"link_kind":"upload"}\n'
                "QUESTION: did my documents arrive?",
        "max_tokens": 300,
    },
    "intake": {
        "system": "Extract intake fields from the worker's message. Reply with "
                  'strict JSON {"fields":{...},"reply":"..."} only. Dollar '
                  "amounts to cents ($4,200 -> 420000).",
        "user": "Dana from Gulf Coast Surgical called about a $4,200 "
                "out-of-network dispute with Meridian, claim GC-88421.",
        "max_tokens": 300,
    },
}


def chat(endpoint, model, shape):
    body = json.dumps({
        "model": model,
        "messages": [
            {"role": "system", "content": shape["system"]},
            {"role": "user", "content": shape["user"]},
        ],
        "temperature": 0,
        "seed": 42,
        "max_tokens": shape["max_tokens"],
        "stream": False,
    }).encode()
    req = urllib.request.Request(endpoint.rstrip("/") + "/chat/completions",
                                 data=body, headers={"Content-Type": "application/json"})
    t0 = time.monotonic()
    with urllib.request.urlopen(req, timeout=120) as r:
        out = json.load(r)
    dt = time.monotonic() - t0
    usage = out.get("usage") or {}
    gen = usage.get("completion_tokens", 0)
    tps = gen / dt if gen and dt else 0
    return dt, gen, tps


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--endpoint", default="http://127.0.0.1:11434/v1")
    ap.add_argument("--model", default="idre-copilot")
    ap.add_argument("--budget", type=float, default=60.0)
    ap.add_argument("--repeat", type=int, default=2,
                    help="runs per shape; first run may include model load")
    a = ap.parse_args()

    print(f"endpoint={a.endpoint} model={a.model} budget={a.budget:.0f}s\n")
    failed = False
    for name, shape in SHAPES.items():
        runs = []
        for i in range(a.repeat):
            try:
                dt, gen, tps = chat(a.endpoint, a.model, shape)
            except Exception as e:  # noqa: BLE001 - report and fail the gate
                print(f"  {name:<10} run {i + 1}: ERROR {e}")
                failed = True
                continue
            runs.append(dt)
            warm = " (cold?)" if i == 0 and a.repeat > 1 else ""
            print(f"  {name:<10} run {i + 1}: {dt:6.1f}s  {gen:4d} tokens  "
                  f"{tps:5.1f} tok/s{warm}")
        if runs:
            best = min(runs)
            verdict = "OK " if best <= a.budget else "OVER BUDGET"
            print(f"  {name:<10} best: {best:6.1f}s  [{verdict}]\n")
            failed = failed or best > a.budget
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
