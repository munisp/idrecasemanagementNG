#!/usr/bin/env python3
# Author: Patrick Munis at newwave
# Deploy freeze gate — reads deploy/freeze-windows.json and decides whether a
# change may ship RIGHT NOW. CI deploy jobs run this before promoting; the
# policy it enforces is documented in docs/CHANGE-FREEZE.md.
#
# Usage:
#   scripts/check-freeze-window.py --change deploy|migration|infra
#                                   [--exception] [--cab-approval]
#                                   [--date YYYY-MM-DD]   # testing only
# Exit 0 = allowed, 1 = blocked (reason on stdout).
#
# Window dates are recurring MM-DD and may wrap the year (12-01 -> 01-31).
import argparse
import json
import os
import sys
from datetime import date

RULES_BLOCK = {
    # change type -> blocked under hard / soft freeze
    # hard: everything blocked (unless --exception for the allowed categories)
    # soft: deploys allowed on Tuesdays only, migrations blocked, infra needs CAB
    "deploy":    {"hard": True,  "soft": "tuesday-only"},
    "migration": {"hard": True,  "soft": True},
    "infra":     {"hard": True,  "soft": "cab-approval"},
}


def parse_mmdd(s):
    # "MM-DD:11-01" -> (11, 1)
    _, mmdd = s.split(":", 1)
    mm, dd = mmdd.split("-")
    return int(mm), int(dd)


def in_window(today, start, end):
    sm, sd = parse_mmdd(start)
    em, ed = parse_mmdd(end)
    cur = (today.month, today.day)
    if (sm, sd) <= (em, ed):
        return (sm, sd) <= cur <= (em, ed)
    # Wraps the year boundary, e.g. 12-01 -> 01-31.
    return cur >= (sm, sd) or cur <= (em, ed)


def main():
    ap = argparse.ArgumentParser(description="IDRE deploy freeze gate")
    ap.add_argument("--change", choices=["deploy", "migration", "infra"], required=True)
    ap.add_argument("--exception", action="store_true",
                    help="change ticket carries the freeze-exception label")
    ap.add_argument("--cab-approval", action="store_true",
                    help="infra change has CAB approval (soft freeze only)")
    ap.add_argument("--date", help="YYYY-MM-DD override (testing)")
    ap.add_argument("--config", default=os.path.join(os.path.dirname(__file__), "..", "deploy", "freeze-windows.json"))
    args = ap.parse_args()

    today = date.fromisoformat(args.date) if args.date else date.today()
    with open(args.config) as f:
        cfg = json.load(f)

    active = [w for w in cfg["windows"] if in_window(today, w["start"], w["end"])]
    hard = [w for w in active if w["type"] == "hard"]
    soft = [w for w in active if w["type"] == "soft"]

    if hard:
        w = hard[0]
        if args.exception:
            print(f"ALLOW: hard freeze '{w['name']}' active but freeze-exception label present "
                  f"(permitted categories: {', '.join(cfg['rules']['exception_categories'])}); "
                  f"canary deploy + 24h watch + retro within 5 business days are MANDATORY")
            return 0
        print(f"BLOCK: hard freeze '{w['name']}' ({w['start']}..{w['end']}): {w['reason']}. "
              f"No {args.change} changes until the window ends. Emergency security patches and "
              f"data-loss/compliance fixes may pass with the freeze-exception label.")
        return 1

    if soft:
        w = soft[0]
        rule = RULES_BLOCK[args.change]["soft"]
        if rule is True:
            print(f"BLOCK: soft freeze '{w['name']}': {args.change} changes are blocked during "
              f"soft freezes ({w['reason']})")
            return 1
        if rule == "tuesday-only" and today.weekday() != 1:
            print(f"BLOCK: soft freeze '{w['name']}': deploys ship Tuesdays only during soft "
              f"freezes ({w['reason']})")
            return 1
        if rule == "cab-approval" and not args.cab_approval:
            print(f"BLOCK: soft freeze '{w['name']}': infra changes need CAB approval during "
              f"soft freezes ({w['reason']})")
            return 1
        print(f"ALLOW: soft freeze '{w['name']}' active; {args.change} permitted under soft-freeze rules")
        return 0

    print(f"ALLOW: no freeze window active on {today.isoformat()}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
