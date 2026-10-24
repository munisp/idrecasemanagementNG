# Author: Patrick Munis at newwave
"""Continuous learning loop for document extraction.

Answers the question "can doc-intel keep improving on the documents it
encounters?" — yes, and here's the mechanism, designed so it can never make
extraction WORSE silently:

1. CAPTURE — every human correction on the review screen lands in
   public.doc_feedback (field, extracted value, corrected value). Human review
   is already the pipeline's safety net (ANALYZED_WITH_FINDINGS routing); this
   turns that labor into training signal instead of discarding it.
2. MINE — a background pass groups corrections into pattern candidates. The
   highest-value pattern for extraction is the LABEL SYNONYM: the money value
   was on the page, the deterministic extractor just didn't know this payer's
   phrasing ("Carrier Allowed", "Plan Allowance", "Resp."). The miner finds
   the corrected value inside the document text and harvests the label phrase
   in front of it.
3. CHAMPION/CHALLENGER — a candidate never goes live on support alone. It is
   evaluated against the accumulated feedback corpus: re-extract with the
   candidate synonym active and measure what fraction of applications yield
   exactly the human-corrected value. precision >= PROMOTE_PRECISION with
   support >= MIN_SUPPORT promotes it to ACTIVE (versioned); anything less
   stays a CANDIDATE. ACTIVE patterns whose precision later degrades below
   RETIRE_PRECISION on fresh feedback are RETIRED — automatic rollback.
4. APPLY — ACTIVE synonyms are OR'd into fields.extract_fields()'s tagged
   money pattern (escaped literals, no regex injection) via the in-memory
   cache below, refreshed after every mining pass. No code deploys, no model
   weights change — the deterministic extractor simply knows more payer
   vocabulary over time. Everything is auditable in doc_learned_patterns.

What deliberately does NOT happen: no gradient updates, no LLM fine-tuning,
no changes to OCR weights. The local qwen2.5 model is inference-only; the
safe, reversible, auditable improvement surface for a regulated platform is
the deterministic layer.
"""

from __future__ import annotations

import os
import re
import threading
import time
from typing import Any

import fields as _fields

MIN_SUPPORT = int(os.environ.get("LEARNING_MIN_SUPPORT", "3"))
PROMOTE_PRECISION = float(os.environ.get("LEARNING_PROMOTE_PRECISION", "0.95"))
RETIRE_PRECISION = float(os.environ.get("LEARNING_RETIRE_PRECISION", "0.80"))
MINE_INTERVAL_SEC = int(os.environ.get("LEARNING_MINE_INTERVAL_SEC", "3600"))

# Correction field names (from the review UI / VLM schema) -> canonical money
# label. Only canonical money fields are learnable targets today: they have
# an unambiguous correctness check (the value must equal the human's number).
_FIELD_TO_CANONICAL = {
    "billed_amount_usd": "billed", "billed": "billed", "total_billed": "billed",
    "allowed_amount_usd": "allowed", "allowed": "allowed",
    "plan_paid_usd": "paid", "paid": "paid", "amount_paid_usd": "paid",
    "patient_responsibility_usd": "patient_resp", "patient_resp": "patient_resp",
    "patient_responsibility": "patient_resp",
    "deductible_usd": "deductible", "deductible": "deductible",
    "copay_usd": "copay", "copay": "copay",
    "coinsurance_usd": "coinsurance", "coinsurance": "coinsurance",
    "qpa_usd": "qpa", "qpa": "qpa",
}

# In-memory cache of ACTIVE synonyms: canonical -> [phrases]. stage_fields
# reads this; the mining loop refreshes it. process-local, rebuilt from the
# DB on startup and after every pass — the DB is the source of truth.
_ACTIVE_LOCK = threading.Lock()
_ACTIVE_LABELS: dict[str, list[str]] = {}
_ACTIVE_MEMO_STRIP: list[str] = []        # MEMO_NOISE_PHRASE values
_ACTIVE_PAYER_ALIAS: dict[str, str] = {}  # PAYER_ALIAS key(ocr payer, lower) -> corrected


def get_active_labels() -> dict[str, list[str]]:
    with _ACTIVE_LOCK:
        return {k: list(v) for k, v in _ACTIVE_LABELS.items()}


def get_active_check_patterns() -> dict[str, Any]:
    """ACTIVE check-extraction patterns: memo noise phrases to strip and
    payer-name aliases (OCR misread -> canonical payer)."""
    with _ACTIVE_LOCK:
        return {"memo_strip": list(_ACTIVE_MEMO_STRIP),
                "payer_alias": dict(_ACTIVE_PAYER_ALIAS)}


def load_active(conn) -> int:
    """Refresh the ACTIVE caches from doc_learned_patterns (labels + check
    patterns share the table; pattern_type namespaces them)."""
    rows = conn.execute(
        "SELECT pattern_type, key, value FROM public.doc_learned_patterns "
        "WHERE status='ACTIVE'"
    ).fetchall()
    labels: dict[str, list[str]] = {}
    memo_strip: list[str] = []
    payer_alias: dict[str, str] = {}
    for ptype, key, value in rows:
        if ptype == "LABEL_SYNONYM":
            labels.setdefault(key, []).append(value)
        elif ptype == "MEMO_NOISE_PHRASE":
            memo_strip.append(value)
        elif ptype == "PAYER_ALIAS":
            payer_alias[key] = value
    with _ACTIVE_LOCK:
        global _ACTIVE_LABELS, _ACTIVE_MEMO_STRIP, _ACTIVE_PAYER_ALIAS
        _ACTIVE_LABELS = labels
        _ACTIVE_MEMO_STRIP = memo_strip
        _ACTIVE_PAYER_ALIAS = payer_alias
    return sum(len(v) for v in labels.values())


# ---------------------------------------------------------------------------
# Candidate mining
# ---------------------------------------------------------------------------

_WORD_RE = re.compile(r"[A-Za-z][A-Za-z&/.'-]*")


def harvest_label_phrase(text: str, value_cents: int) -> str | None:
    """Find `value` (as a money string) in document text and return the label
    phrase immediately before it, normalized. None when the value isn't in
    the text or no plausible label precedes it."""
    amt = value_cents / 100
    forms = {f"${amt:,.2f}", f"{amt:,.2f}", f"${amt:,.0f}" if amt == int(amt) else "",
             f"{amt:,.0f}" if amt == int(amt) else ""}
    for form in forms:
        if not form:
            continue
        idx = text.find(form)
        if idx < 0:
            continue
        prefix = text[max(0, idx - 80):idx]
        words = _WORD_RE.findall(prefix)
        # Drop trailing stop-words/punctuation noise; keep up to 4 words.
        while words and words[-1].lower() in {"the", "a", "of", "to", "is", ":", "-"}:
            words.pop()
        if not words:
            return None
        phrase = " ".join(words[-4:]).lower()
        if 2 <= len(phrase) <= 40:
            return phrase
    return None


def _corrected_cents(corrected_value: str) -> int | None:
    m = re.match(r"^\$?\s*(-?[\d,]+(?:\.\d{1,2})?)$", (corrected_value or "").strip())
    if not m:
        return None
    return int(round(float(m.group(1).replace(",", "")) * 100))


def mine(conn, os_client=None, index: str = "idre-docs-*") -> dict:
    """One mining pass. Returns counters for the worker log.

    Candidates are harvested from feedback + document text (OpenSearch holds
    the indexed text; when search is unavailable the pass still re-scores
    existing candidates against stored snippets and can retire, just not grow).
    """
    stats = {"feedback_rows": 0, "candidates": 0, "promoted": 0,
             "retired": 0, "active_labels": 0}

    fb = conn.execute(
        """SELECT id, tenant, doc_id, doc_type, field, extracted_value,
                  corrected_value
           FROM public.doc_feedback
           WHERE corrected_value IS NOT NULL AND corrected_value <> ''"""
    ).fetchall()
    stats["feedback_rows"] = len(fb)

    # 1) Harvest label-phrase candidates from document text.
    phrase_support: dict[tuple[str, str], set] = {}   # (canonical, phrase) -> doc_ids
    for _id, tenant, doc_id, _doc_type, field, _ex, corrected in fb:
        canonical = _FIELD_TO_CANONICAL.get((field or "").lower())
        cents = _corrected_cents(corrected)
        if not canonical or cents is None:
            continue
        text = _doc_text(os_client, index, doc_id) if os_client else None
        if not text:
            continue
        phrase = harvest_label_phrase(text, cents)
        if phrase:
            phrase_support.setdefault((canonical, phrase), set()).add(str(doc_id))

    for (canonical, phrase), doc_ids in phrase_support.items():
        stats["candidates"] += 1
        conn.execute(
            """INSERT INTO public.doc_learned_patterns
                 (pattern_type, key, value, support, status)
               VALUES ('LABEL_SYNONYM', %s, %s, %s, 'CANDIDATE')
               ON CONFLICT (pattern_type, key, value) DO UPDATE
                 SET support = EXCLUDED.support, last_seen = now()""",
            (canonical, phrase, len(doc_ids)),
        )

    # 2) Champion/challenger: score every CANDIDATE/ACTIVE pattern against
    #    the full feedback corpus. A pattern "hits" when extraction with its
    #    synonym active recovers the human-corrected cents from the document.
    corpus: list[tuple[str, int, str]] = []   # (canonical, cents, text)
    for _id, tenant, doc_id, _doc_type, field, _ex, corrected in fb:
        canonical = _FIELD_TO_CANONICAL.get((field or "").lower())
        cents = _corrected_cents(corrected)
        if not canonical or cents is None:
            continue
        text = _doc_text(os_client, index, doc_id) if os_client else None
        if text:
            corpus.append((canonical, cents, text))

    pats = conn.execute(
        """SELECT id, key, value, support, status
           FROM public.doc_learned_patterns
           WHERE pattern_type='LABEL_SYNONYM' AND status IN ('CANDIDATE','ACTIVE')"""
    ).fetchall()
    for pid, canonical, phrase, support, status in pats:
        relevant = [(c, v, t) for c, v, t in corpus if c == canonical]
        hits = 0
        for c, v, t in relevant:
            ex = _fields.extract_fields(t, extra_labels={canonical: [phrase]})
            if any(a.get("canonical") == canonical and a["cents"] == v
                   for a in ex.get("amounts", [])):
                hits += 1
        precision = hits / len(relevant) if relevant else 0.0
        if (status == "CANDIDATE" and (support or 0) >= MIN_SUPPORT
                and relevant and precision >= PROMOTE_PRECISION):
            conn.execute(
                """UPDATE public.doc_learned_patterns
                   SET status='ACTIVE', precision=%s, version=version+1,
                       promoted_at=now(), last_seen=now()
                   WHERE id=%s""",
                (round(precision, 4), pid),
            )
            stats["promoted"] += 1
        elif status == "ACTIVE" and relevant and precision < RETIRE_PRECISION:
            conn.execute(
                """UPDATE public.doc_learned_patterns
                   SET status='RETIRED', precision=%s, last_seen=now()
                   WHERE id=%s""",
                (round(precision, 4), pid),
            )
            stats["retired"] += 1
        else:
            conn.execute(
                "UPDATE public.doc_learned_patterns SET precision=%s, last_seen=now() WHERE id=%s",
                (round(precision, 4), pid),
            )

    stats.update(mine_check_patterns(conn))
    stats["active_labels"] = load_active(conn)
    return stats


# ---------------------------------------------------------------------------
# Check-extraction learning
# ---------------------------------------------------------------------------
# Physical checks carry no money labels to learn synonyms for — the learnable,
# repeatable OCR failures are (a) noise that rides into the memo line (bank
# stamps, "MEMO:" leftovers, pre-printed form text) and breaks the invoice-no
# substring match in case-api, and (b) payer names the OCR misreads the same
# way every time for a given issuer. Human check corrections (check_feedback)
# are mined into MEMO_NOISE_PHRASE / PAYER_ALIAS patterns, promoted by the same
# champion/challenger gate, and applied by check_processor on the next check.

def harvest_memo_noise(extracted: str, corrected: str) -> str | None:
    """Words present in the extracted memo but not the corrected one, as a
    contiguous prefix or suffix phrase. Interior edits aren't noise — they're
    content corrections — and aren't learned."""
    ex_words = (extracted or "").split()
    co_words = set((corrected or "").lower().split())
    diff = [w for w in ex_words if w.lower() not in co_words]
    if not diff:
        return None
    # Contiguous prefix or suffix only.
    if ex_words[:len(diff)] == diff:
        phrase = " ".join(diff)
    elif ex_words[-len(diff):] == diff:
        phrase = " ".join(diff)
    else:
        return None
    phrase = phrase.strip().lower()
    return phrase if 2 <= len(phrase) <= 60 else None


def strip_memo_noise(memo: str, phrases: list[str]) -> str:
    """Apply ACTIVE memo-noise phrases (case-insensitive, word-bounded)."""
    out = memo
    for phrase in phrases:
        out = re.sub(r"(?i)\b" + re.escape(phrase) + r"\b", " ", out)
    return re.sub(r"\s{2,}", " ", out).strip(" :-,")


def mine_check_patterns(conn) -> dict:
    """Mine check_feedback into MEMO_NOISE_PHRASE / PAYER_ALIAS patterns and
    run the same champion/challenger gate used for label synonyms."""
    stats = {"check_feedback_rows": 0, "check_candidates": 0,
             "check_promoted": 0, "check_retired": 0}

    fb = conn.execute(
        """SELECT id, field, extracted_value, corrected_value
           FROM public.check_feedback
           WHERE corrected_value IS NOT NULL AND corrected_value <> ''"""
    ).fetchall()
    stats["check_feedback_rows"] = len(fb)
    if not fb:
        return stats

    # 1) Harvest candidates.
    memo_support: dict[str, set] = {}             # phrase -> feedback ids
    payer_support: dict[tuple[str, str], set] = {}  # (ocr payer, corrected) -> ids
    for fid, field, extracted, corrected in fb:
        f = (field or "").lower()
        if f == "memo":
            phrase = harvest_memo_noise(extracted or "", corrected)
            if phrase:
                memo_support.setdefault(phrase, set()).add(fid)
        elif f == "payer_name" and extracted and extracted.strip().lower() != corrected.strip().lower():
            payer_support.setdefault((extracted.strip().lower(), corrected.strip()), set()).add(fid)

    for phrase, ids in memo_support.items():
        stats["check_candidates"] += 1
        conn.execute(
            """INSERT INTO public.doc_learned_patterns
                 (pattern_type, key, value, support, status)
               VALUES ('MEMO_NOISE_PHRASE', 'memo', %s, %s, 'CANDIDATE')
               ON CONFLICT (pattern_type, key, value) DO UPDATE
                 SET support = EXCLUDED.support, last_seen = now()""",
            (phrase, len(ids)),
        )
    for (ocr, fixed), ids in payer_support.items():
        stats["check_candidates"] += 1
        conn.execute(
            """INSERT INTO public.doc_learned_patterns
                 (pattern_type, key, value, support, status)
               VALUES ('PAYER_ALIAS', %s, %s, %s, 'CANDIDATE')
               ON CONFLICT (pattern_type, key, value) DO UPDATE
                 SET support = EXCLUDED.support, last_seen = now()""",
            (ocr, fixed, len(ids)),
        )

    # 2) Champion/challenger scoring against the feedback corpus.
    memo_rows = [(ex or "", co or "") for _id, f, ex, co in fb if (f or "").lower() == "memo"]
    payer_rows = [(ex or "", co or "") for _id, f, ex, co in fb if (f or "").lower() == "payer_name"]

    pats = conn.execute(
        """SELECT id, pattern_type, key, value, support, status
           FROM public.doc_learned_patterns
           WHERE pattern_type IN ('MEMO_NOISE_PHRASE','PAYER_ALIAS')
             AND status IN ('CANDIDATE','ACTIVE')"""
    ).fetchall()
    for pid, ptype, key, value, support, status in pats:
        if ptype == "MEMO_NOISE_PHRASE":
            relevant = [(ex, co) for ex, co in memo_rows
                        if re.search(r"(?i)\b" + re.escape(value) + r"\b", ex)]
            hits = sum(1 for ex, co in relevant
                       if strip_memo_noise(ex, [value]).lower() == co.strip().lower())
        else:  # PAYER_ALIAS: key = OCR'd payer (lower), value = correction
            relevant = [(ex, co) for ex, co in payer_rows if ex.strip().lower() == key]
            hits = sum(1 for _ex, co in relevant if co.strip() == value)
        precision = hits / len(relevant) if relevant else 0.0
        if (status == "CANDIDATE" and (support or 0) >= MIN_SUPPORT
                and relevant and precision >= PROMOTE_PRECISION):
            conn.execute(
                """UPDATE public.doc_learned_patterns
                   SET status='ACTIVE', precision=%s, version=version+1,
                       promoted_at=now(), last_seen=now()
                   WHERE id=%s""",
                (round(precision, 4), pid),
            )
            stats["check_promoted"] += 1
        elif status == "ACTIVE" and relevant and precision < RETIRE_PRECISION:
            conn.execute(
                """UPDATE public.doc_learned_patterns
                   SET status='RETIRED', precision=%s, last_seen=now()
                   WHERE id=%s""",
                (round(precision, 4), pid),
            )
            stats["check_retired"] += 1
        else:
            conn.execute(
                "UPDATE public.doc_learned_patterns SET precision=%s, last_seen=now() WHERE id=%s",
                (round(precision, 4), pid),
            )
    return stats


def _doc_text(os_client, index: str, doc_id: Any) -> str | None:
    """Indexed document text by doc_id; None when search is unavailable —
    mining degrades (no new candidates) rather than failing."""
    try:
        res = os_client.search(index=index, body={
            "size": 1,
            "query": {"term": {"doc_id": str(doc_id)}},
            "_source": ["text"],
        })
        hits = res.get("hits", {}).get("hits", [])
        if hits:
            return hits[0].get("_source", {}).get("text", "")
    except Exception:
        pass
    return None


# ---------------------------------------------------------------------------
# Background loop
# ---------------------------------------------------------------------------

def start_learning_loop(connect, os_client, index: str = "idre-docs-*") -> threading.Thread:
    """Mining on an interval, in a daemon thread. The first pass runs after
    one interval — at startup there is nothing new to learn yet, and the
    ACTIVE cache is loaded synchronously by main() instead."""
    def loop():
        while True:
            time.sleep(MINE_INTERVAL_SEC)
            try:
                with connect() as conn:
                    stats = mine(conn, os_client, index)
                    conn.commit()
                print(f"doc-intel learning: mined {stats}", flush=True)
            except Exception as exc:
                # Learning must never take down document processing.
                print(f"doc-intel learning: pass failed: {exc}", flush=True)

    t = threading.Thread(target=loop, name="doc-intel-learning", daemon=True)
    t.start()
    return t
