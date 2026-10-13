"""TPA onboarding package validation (feature flag: tpa_onboarding).

Consumes `tpa_doc.uploaded` events (published by case-api tpa_onboarding.go):

  DISCRETE  — one event per slot upload; validate the document against the
              slot's expectation and write the verdict onto
              public.tpa_package_docs.
  MERGED    — one event for the giant PDF; split it by page-level
              classification into the six required slots, seal + store each
              segment through the vault, then validate each segment.

Every verdict is machine-readable (reasons jsonb), fail-open on analyzer
outages (NEEDS_REVIEW, never silent acceptance), and the package flips to
READY only when all six slots are ACCEPTED. The filing gate in case-api
re-checks READY + verified payment, so a worker bug can delay a filing but
never let an unvalidated package through.
"""

from __future__ import annotations

import base64
import io
import json
import uuid

import httpx
import psycopg

SLOTS = [
    "EOB", "ITEMIZED_BILL", "PROOF_OF_COVERAGE",
    "OPEN_NEGOTIATION_NOTICE", "OFFER_SUBSTANTIATION", "IDRE_ATTESTATION",
]

# Slot -> classifier type that must win (None = keyword floor instead).
SLOT_TYPE = {
    "EOB": "eob",
    "ITEMIZED_BILL": "idr_claim",
    "OFFER_SUBSTANTIATION": "idr_claim",
    "PROOF_OF_COVERAGE": None,
    "OPEN_NEGOTIATION_NOTICE": None,
    "IDRE_ATTESTATION": None,
}

# Keyword floor for slots without a dedicated classifier type — the document
# must say what it is, in its own words.
SLOT_KEYWORDS = {
    "PROOF_OF_COVERAGE": ["coverage", "eligible", "eligibility", "member", "subscriber"],
    "OPEN_NEGOTIATION_NOTICE": ["open negotiation", "30 business days", "notice of initiation"],
    "IDRE_ATTESTATION": ["attest", "attestation", "certify", "under penalty"],
}

MIN_TEXT_CHARS = 40  # below this the document is illegible to the pipeline


def _pdf_pages(raw: bytes) -> tuple[list, object]:
    import pypdfium2 as pdfium
    pdf = pdfium.PdfDocument(raw)
    texts = []
    for page in pdf:
        tp = page.get_textpage()
        texts.append(tp.get_text_range() or "")
        tp.close()
        page.close()
    return texts, pdf


def _seal(vault: str, headers: dict, tenant: str, key: str, raw: bytes) -> bytes:
    resp = httpx.post(f"{vault}/docs/seal", timeout=60, headers=headers, json={
        "tenant": tenant, "key": key, "data_b64": base64.b64encode(raw).decode(),
    })
    resp.raise_for_status()
    return base64.b64decode(resp.json()["sealed_b64"])


def _verdict(slot: str, filename: str, text: str, classify_text, extract_fields) -> tuple[str, list[dict]]:
    """(status, reasons). Conservative: only a confident wrong-type is a
    REJECTED; ambiguity and illegibility go to NEEDS_REVIEW for a human."""
    reasons: list[dict] = []
    body = (text or "").strip()
    if len(body) < MIN_TEXT_CHARS:
        reasons.append({"code": "LEGIBILITY_LOW",
                        "detail": f"only {len(body)} characters of extractable text (scanned or corrupt?)"})
        return "NEEDS_REVIEW", reasons
    dtype, score, margin = classify_text(filename, body)
    expected = SLOT_TYPE[slot]
    if expected:
        if dtype == "unrelated":
            reasons.append({"code": "TYPE_UNCLEAR",
                            "detail": "no recognizable document-type signal"})
            return "NEEDS_REVIEW", reasons
        if dtype != expected:
            reasons.append({"code": "TYPE_MISMATCH",
                            "detail": f"classified as {dtype} (score {score}), expected {expected}"})
            return "REJECTED", reasons
        if margin < 2:
            reasons.append({"code": "TYPE_AMBIGUOUS",
                            "detail": f"{dtype} won by a thin margin ({margin})"})
            return "NEEDS_REVIEW", reasons
        # Field floor: billing documents must carry something to fight about.
        fields = extract_fields(body) or {}
        idents = fields.get("identifiers") or {}
        if expected == "eob" and not (idents.get("claim_number") or fields.get("cpt_codes")):
            reasons.append({"code": "FIELDS_MISSING",
                            "detail": "no claim number or CPT/HCPCS codes extracted"})
            return "NEEDS_REVIEW", reasons
        if expected == "idr_claim" and not fields.get("cpt_codes"):
            reasons.append({"code": "FIELDS_MISSING",
                            "detail": "no CPT/HCPCS codes extracted"})
            return "NEEDS_REVIEW", reasons
        return "ACCEPTED", reasons
    # Keyword-floor slots.
    low = body.lower()
    if any(k in low for k in SLOT_KEYWORDS[slot]):
        return "ACCEPTED", reasons
    reasons.append({"code": "KEYWORDS_MISSING",
                    "detail": f"none of {SLOT_KEYWORDS[slot]} found in document text"})
    return "NEEDS_REVIEW", reasons


def _refresh_package(conn, tenant: str, package_id: str) -> str:
    """Package status from its slot verdicts. READY iff all six ACCEPTED."""
    rows = conn.execute(
        "SELECT slot, status FROM public.tpa_package_docs WHERE tenant=%s AND package_id=%s",
        (tenant, package_id)).fetchall()
    by_slot = {r[0]: r[1] for r in rows}
    if all(by_slot.get(s) == "ACCEPTED" for s in SLOTS):
        status = "READY"
    elif any(v == "REJECTED" for v in by_slot.values()):
        status = "REJECTED"
    else:
        status = "VALIDATING"
    conn.execute(
        "UPDATE public.tpa_packages SET status=%s, updated_at=now() WHERE tenant=%s AND id=%s",
        (status, tenant, package_id))
    # A READY package can complete the client's docs gate.
    conn.execute(
        """UPDATE public.tpa_clients c SET onboarding_status='READY'
           WHERE c.tenant=%s AND c.id=(SELECT client_id FROM public.tpa_packages WHERE id=%s)
             AND %s='READY'
             AND c.npi IS NOT NULL AND c.tin IS NOT NULL AND c.address IS NOT NULL
             AND EXISTS (SELECT 1 FROM public.tpa_payment_methods m
                          WHERE m.tenant=c.tenant AND m.client_id=c.id AND m.verified AND m.status='ACTIVE')""",
        (tenant, package_id, status))
    return status


def handle_tpa_doc(evt: dict, ctx: dict) -> None:
    """Entry point called from main.run() on tpa_doc.uploaded events.
    ctx carries the shared helpers so this module stays testable:
    {dsn, vault, vault_headers, minio, classify_text, extract_fields,
     fetch_and_decrypt, features_for}."""
    tenant = evt.get("tenant")
    if not tenant:
        return
    if not ctx["features_for"](tenant).get("tpa_onboarding", True):
        return
    if evt.get("merged"):
        _handle_merged(evt, ctx)
    else:
        _handle_discrete(evt, ctx)


def _handle_discrete(evt: dict, ctx: dict) -> None:
    tenant, doc_id = evt["tenant"], evt["package_doc_id"]
    with psycopg.connect(ctx["dsn"]) as conn:
        conn.execute(
            "UPDATE public.tpa_package_docs SET status='ANALYZING' WHERE tenant=%s AND id=%s",
            (tenant, doc_id))
        conn.commit()
    try:
        raw = ctx["fetch_and_decrypt"](tenant, evt["object_key"])
        if raw[:4] == b"%PDF":
            texts, pdf = _pdf_pages(raw)
            text = "\n".join(texts)
            pdf.close()
        else:
            text = raw.decode("utf-8", "ignore")
        status, reasons = _verdict(evt["slot"], evt.get("filename") or "", text,
                                   ctx["classify_text"], ctx["extract_fields"])
    except Exception as exc:  # noqa: BLE001 — analyzer outage must fail open
        status, reasons = "NEEDS_REVIEW", [{"code": "ANALYZER_ERROR", "detail": str(exc)[:300]}]
    with psycopg.connect(ctx["dsn"]) as conn:
        conn.execute(
            """UPDATE public.tpa_package_docs
               SET status=%s, reasons=%s::jsonb, analyzed_at=now()
               WHERE tenant=%s AND id=%s""",
            (status, json.dumps(reasons), tenant, doc_id))
        pkg_status = _refresh_package(conn, tenant, evt["package_id"])
        conn.commit()
    print(f"tpa-doc {doc_id} slot={evt['slot']} -> {status} ({len(reasons)} reasons); "
          f"package {evt['package_id']} -> {pkg_status}", flush=True)


def _handle_merged(evt: dict, ctx: dict) -> None:
    """Split the giant PDF by page classification into the six slots."""
    tenant, package_id = evt["tenant"], evt["package_id"]
    try:
        raw = ctx["fetch_and_decrypt"](tenant, evt["object_key"])
        texts, pdf = _pdf_pages(raw)
        # Classify every page; a page inherits its predecessor's type when it
        # has no signal of its own (continuation pages of a multi-page EOB).
        page_types: list[str] = []
        last = "unrelated"
        for t in texts:
            dtype, score, _ = ctx["classify_text"]("", t)
            if dtype == "unrelated" or score < 2:
                page_types.append(last)
            else:
                page_types.append(dtype)
                last = dtype
        # Group contiguous pages of one type.
        segments: list[tuple[str, int, int]] = []  # (type, start, end) inclusive
        for i, t in enumerate(page_types):
            if segments and segments[-1][0] == t:
                segments[-1] = (t, segments[-1][1], i)
            else:
                segments.append((t, i, i))
        # Map segments to slots (first segment of a type wins the slot).
        slot_for = {"eob": "EOB"}
        assigned: dict[str, tuple[int, int]] = {}
        for dtype, start, end in segments:
            if dtype == "unrelated":
                continue
            if dtype == "idr_claim":
                seg_text = "\n".join(texts[start:end + 1]).lower()
                slot = ("ITEMIZED_BILL" if any(k in seg_text for k in ("itemized", "ub-04", "ub04", "cms-1500"))
                        else "OFFER_SUBSTANTIATION")
            else:
                slot = slot_for.get(dtype)
                if slot is None:
                    # Keyword-floor slots detected inside a merged PDF.
                    low = "\n".join(texts[start:end + 1]).lower()
                    slot = next((s for s in ("PROOF_OF_COVERAGE", "OPEN_NEGOTIATION_NOTICE", "IDRE_ATTESTATION")
                                 if any(k in low for k in SLOT_KEYWORDS[s])), None)
            if slot and slot not in assigned:
                assigned[slot] = (start, end)
        with psycopg.connect(ctx["dsn"]) as conn:
            for slot in SLOTS:
                if slot in assigned:
                    start, end = assigned[slot]
                    seg = _export_segment(pdf, start, end)
                    key = f"{tenant}/tpa-packages/{package_id}/{slot.lower()}-{uuid.uuid4()}.enc"
                    ct = _seal(ctx["vault"], ctx["vault_headers"], tenant, key, seg)
                    ctx["minio"].put_object("idre-docs", key, io.BytesIO(ct), len(ct))
                    text = "\n".join(texts[start:end + 1])
                    status, reasons = _verdict(slot, f"{slot.lower()}.pdf", text,
                                               ctx["classify_text"], ctx["extract_fields"])
                    conn.execute(
                        """INSERT INTO public.tpa_package_docs
                             (tenant, package_id, slot, object_key, filename, status, reasons, page_start, page_end, analyzed_at)
                           VALUES (%s,%s,%s,%s,%s,%s,%s::jsonb,%s,%s,now())
                           ON CONFLICT (package_id, slot) DO UPDATE SET
                             object_key=excluded.object_key, filename=excluded.filename,
                             status=excluded.status, reasons=excluded.reasons,
                             page_start=excluded.page_start, page_end=excluded.page_end,
                             analyzed_at=now()""",
                        (tenant, package_id, slot, key, f"{slot.lower()} (pages {start + 1}-{end + 1})",
                         status, json.dumps(reasons + [{"code": "SPLIT_FROM_MERGED",
                                                        "detail": f"pages {start + 1}-{end + 1}"}]),
                         start + 1, end + 1))
                else:
                    # Slot never found in the merged PDF: NEEDS_REVIEW with the
                    # source as evidence — a human decides whether it's missing
                    # or the splitter missed it.
                    conn.execute(
                        """INSERT INTO public.tpa_package_docs
                             (tenant, package_id, slot, object_key, filename, status, reasons, analyzed_at)
                           VALUES (%s,%s,%s,%s,%s,'NEEDS_REVIEW',
                                   %s::jsonb, now())
                           ON CONFLICT (package_id, slot) DO UPDATE SET
                             status='NEEDS_REVIEW', reasons=excluded.reasons, analyzed_at=now()""",
                        (tenant, package_id, slot, evt["object_key"], evt.get("filename") or "merged.pdf",
                         json.dumps([{"code": "SLOT_NOT_FOUND_IN_MERGED",
                                      "detail": "page classification found no segment for this slot"}])))
            pkg_status = _refresh_package(conn, tenant, package_id)
            conn.commit()
        pdf.close()
        print(f"tpa-doc merged {package_id}: slots {sorted(assigned)} -> package {pkg_status}", flush=True)
    except Exception as exc:  # noqa: BLE001
        with psycopg.connect(ctx["dsn"]) as conn:
            conn.execute(
                "UPDATE public.tpa_packages SET status='REJECTED', updated_at=now() WHERE tenant=%s AND id=%s",
                (tenant, package_id))
            conn.commit()
        print(f"tpa-doc merged {package_id} FAILED: {exc}", flush=True)


def _export_segment(pdf, start: int, end: int) -> bytes:
    """Pages [start, end] of the merged PDF as a standalone PDF."""
    import pypdfium2 as pdfium
    out = pdfium.PdfDocument.new()
    out.import_pages(pdf, [i for i in range(start, end + 1)])
    buf = io.BytesIO()
    out.save(buf)
    out.close()
    return buf.getvalue()
