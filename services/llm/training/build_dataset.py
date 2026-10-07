#!/usr/bin/env python3
"""Build SECTOR-CONDITIONED instruction datasets (JSONL) for QLoRA tuning.

NG is multi-program: one Ollama host serves NSA/IDR, FL AHCA CDR, and any
future sector (appraisal, tax, insurance…). One blended model is operationally
right here (OLLAMA_MAX_LOADED_MODELS=1), so every row is CONDITIONED on its
sector: the instruction names the program, teaching the model to answer
inside the program's frame instead of blending authorities.

Layout:
  corpus/nsa/*.txt        — NSA/IDR authorities (45 CFR 149 auto-fetched,
                            CMS guidance, preamble extracts)
  corpus/fl-ahca/*.txt    — FL AHCA Claims Dispute Resolution materials
  corpus/<sector>/*.txt   — any future program's statutes/rules/SOPs

Usage:
  python3 build_dataset.py --sector nsa                 # one sector
  python3 build_dataset.py --all                        # merged multi-sector set
  python3 build_dataset.py --all -o dataset_multi.jsonl # -> train_lora.py

Output rows (JSONL): {"instruction": ..., "input": ..., "output": ...}
Row types per sector:
  RECALL   — passage-grounded Q&A in the program's voice
  CITE     — citation-fidelity pairs (answer must name the authority)
  REFUSAL  — bait questions answered with the platform's refusal phrase;
             fine-tuning WITHOUT these produces a fluent hallucinator.
"""
import argparse
import json
import re
import sys
import urllib.request
import xml.etree.ElementTree as ET
from pathlib import Path

ECFR_URL = ("https://www.ecfr.gov/api/versioner/v1/full/{date}/title-45.xml"
            "?part=149&subtitle=B&chapter=A&subchapter=B")

# Sector registry: program label used to CONDITION instructions. Add sectors
# here as programs onboard — the corpus dir and golden set follow the key.
SECTORS = {
    "nsa":     {"label": "the federal No Surprises Act IDR program", "ecfr": True},
    "fl-ahca": {"label": "the Florida AHCA Claims Dispute Resolution program", "ecfr": False},
}

REFUSAL = ("That is not in the record or authority available to me for this "
           "program. I don't speculate — check the cited regulation or ask "
           "the case coordinator.")

REFUSAL_TEMPLATES = [
    "What will the determination be for my case next week?",
    "Is the other side acting in bad faith?",
    "Who will win this dispute?",
    "What did the parties say in their confidential settlement talks?",
    "Give me the arbitrator's phone number.",
    "How should I structure offers to guarantee I win?",
]


def fetch_part149(date: str) -> str:
    req = urllib.request.Request(ECFR_URL.format(date=date),
                                 headers={"User-Agent": "idre-copilot-ng-training/1.0"})
    with urllib.request.urlopen(req, timeout=120) as r:
        return r.read().decode("utf-8")


def sections_from_ecfr(xml_text: str) -> list[tuple[str, str]]:
    out = []
    for div in ET.fromstring(xml_text).iter("DIV8"):
        sec_no = div.attrib.get("N", "")
        text = re.sub(r"\s+", " ", " ".join(div.itertext())).strip()
        if sec_no and len(text) > 120:
            out.append((sec_no, text))
    return out


def condense(text: str, max_sent: int = 3) -> str:
    """Extractive condense — never paraphrase. Training on paraphrase teaches
    paraphrase; training on authority text teaches quoting."""
    return " ".join(re.split(r"(?<=[.!?])\s+", text)[:max_sent]).strip()


def rows_from_passage(sector_label: str, source: str, text: str) -> list[dict]:
    return [
        {"instruction": f"Under {sector_label}, what does this passage from {source} establish?",
         "input": text[:3000],
         "output": condense(text)},
        {"instruction": f"Which authority in {sector_label} covers this, and what is its operative rule? ({source})",
         "input": "",
         "output": f"{source}. {condense(text, 2)}"},
    ]


def rows_for_sector(key: str, date: str, corpus_root: Path) -> list[dict]:
    spec = SECTORS[key]
    label = spec["label"]
    rows: list[dict] = []
    if spec.get("ecfr"):
        try:
            for sec, text in sections_from_ecfr(fetch_part149(date)):
                rows.extend(rows_from_passage(label, f"45 CFR {sec}", text))
            print(f"{key}: ecfr ok ({len(rows)} rows)", file=sys.stderr)
        except Exception as e:
            print(f"{key}: ecfr fetch failed ({e}); corpus only", file=sys.stderr)
    d = corpus_root / key
    if d.is_dir():
        for f in sorted(d.glob("*.txt")):
            n0 = len(rows)
            chunks = [c.strip() for c in re.split(r"\n\s*\n", f.read_text(encoding="utf-8", errors="replace"))
                      if len(c.strip()) > 300]
            for ch in chunks[:200]:
                rows.extend(rows_from_passage(label, f"{f.stem}", ch))
            print(f"{key}: {f.name} +{len(rows)-n0}", file=sys.stderr)
    for q in REFUSAL_TEMPLATES:
        rows.append({"instruction": f"Under {label}: {q}", "input": "", "output": REFUSAL})
    return rows


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--sector", choices=sorted(SECTORS))
    ap.add_argument("--all", action="store_true", help="merge every sector into one conditioned set")
    ap.add_argument("--date", default="2026-01-01")
    ap.add_argument("--corpus", default="corpus")
    ap.add_argument("-o", "--out", default="")
    a = ap.parse_args()
    if not a.sector and not a.all:
        ap.error("pick --sector or --all")

    keys = sorted(SECTORS) if a.all else [a.sector]
    out = a.out or ("dataset_multi.jsonl" if a.all else f"dataset_{a.sector}.jsonl")
    n = 0
    with open(out, "w", encoding="utf-8") as fh:
        for k in keys:
            for r in rows_for_sector(k, a.date, Path(a.corpus)):
                fh.write(json.dumps(r, ensure_ascii=False) + "\n")
                n += 1
    print(f"wrote {n} rows -> {out} (sectors: {', '.join(keys)})", file=sys.stderr)


if __name__ == "__main__":
    main()
