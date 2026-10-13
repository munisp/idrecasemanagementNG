"""doc-intel service: consumes doc.uploaded events from Kafka, fetches the
ciphertext from MinIO, decrypts via the vault, runs the docked PaddleOCR+VLM
pipeline, and persists results (Postgres + OpenSearch + Kafka + Temporal signal).

Run: python main.py   (GPU optional — PaddleOCR auto-detects; VLM is remote)
"""

from __future__ import annotations

import json
import os
import re
import signal
import sys
import time
from collections import deque
from concurrent.futures import ThreadPoolExecutor

import httpx
import psycopg
from confluent_kafka import Consumer
from minio import Minio
from opensearchpy import OpenSearch

from pipeline import bytes_to_pages, classify_text, ooxml_safe, run_pipeline, sniff_doc_kind
from fields import extract_fields
import tpa_docs
from rules_engine import expand_template, fire_rules, load_rules
from check_processor import extract_check
import learning


def is_blocked(evt: dict, kind: str, subj_id: str) -> bool:
    """case-api quarantines rule-blocked uploads (analysis_status='BLOCKED')
    AFTER publishing doc.uploaded — doc-intel must honor the quarantine and
    never run OCR/VLM over blocked bytes."""
    table = (f"tenant_{evt['tenant']}.documents" if kind == "case"
             else "public.application_documents")
    try:
        with psycopg.connect(DSN) as c:
            row = c.execute(
                f"SELECT analysis_status FROM {table} WHERE id=%s",
                (evt["doc_id"],),
            ).fetchone()
        return bool(row and row[0] == "BLOCKED")
    except psycopg.Error:
        return False  # fail open: quarantine is enforced again at read time


def fire_doc_rules(evt: dict, kind: str, subj_id: str, ctx: dict) -> None:
    """doc.analyzed program rules — same engine semantics as the Go side
    (rules_engine mirrors rules.go). Facts describe the analysis outcome;
    actions drive notifications, activity, review flags, and case detail
    fields. Fresh rule read per document: admin edits apply immediately."""
    low_conf = [f for f, cf in ctx.get("field_confidence", {}).items() if cf == "low"]
    facts = {
        "tenant": evt["tenant"], "doc_id": evt["doc_id"],
        "case_id": subj_id if kind == "case" else "",
        "application_id": subj_id if kind == "application" else "",
        "doc_type": ctx.get("doc_type") or "",
        "analysis_status": ctx.get("status", ""),
        "ungrounded_count": len(low_conf),
        "scan_quality_poor": bool(ctx.get("scan_quality_poor")),
        "ocr_confidence": ctx.get("ocr_confidence"),
        "ocr_low_confidence": bool(ctx.get("ocr_low_confidence")),
        "ambiguous": bool(ctx.get("classify_ambiguous")),
        "truncated": bool(ctx.get("result_truncated")),
        "findings_count": len(ctx.get("findings", [])),
    }
    with psycopg.connect(DSN, autocommit=True) as c:
        rules = load_rules(c, evt["tenant"], "doc.analyzed")
        if not rules:
            return
        actions, fired, suspect = fire_rules(rules, facts)
        for name in suspect:
            print(f"doc-intel: suspect rule {name!r} skipped on doc.analyzed",
                  file=sys.stderr, flush=True)
        for a in actions:
            atype, params = a.get("type", ""), a.get("params", {}) or {}
            if atype == "notify":
                c.execute(
                    """INSERT INTO public.notifications (tenant, user_sub, type, body)
                       VALUES (%s,'*',%s,%s)""",
                    (evt["tenant"], params.get("kind") or "MILESTONE",
                     expand_template(str(params.get("body", "")), facts)[:2000]),
                )
            elif atype == "log_activity" and kind == "case":
                c.execute(
                    """INSERT INTO public.case_activities (tenant, case_id, type, body)
                       VALUES (%s,%s,'RULE_DOC_ANALYZED',%s)""",
                    (evt["tenant"], subj_id,
                     expand_template(str(params.get("body", "")), facts)[:2000]),
                )
            elif atype == "flag_review" and kind == "case":
                c.execute(
                    f"UPDATE tenant_{evt['tenant']}.documents SET analysis_status='NEEDS_REVIEW' WHERE id=%s",
                    (evt["doc_id"],),
                )
            elif atype == "set_detail" and kind == "case":
                key = str(params.get("key", ""))
                if re.fullmatch(r"[a-z][a-z0-9_]{0,40}", key):
                    c.execute(
                        f"""UPDATE tenant_{evt['tenant']}.cases
                            SET details = jsonb_set(details, %s, to_jsonb(%s::text), true), updated_at=now()
                            WHERE id=%s""",
                        ("{" + key + "}", expand_template(str(params.get("value", "")), facts), subj_id),
                    )
        if fired:
            print(f"doc-intel: rules fired on doc.analyzed: {', '.join(fired)}",
                  file=sys.stderr, flush=True)

DSN = os.environ.get("DATABASE_URL", "postgres://idre:idre@localhost:5432/idre")
VAULT = os.environ.get("VAULT_URL", "http://localhost:8081")
# Vault enforces X-Internal-Auth on every route when VAULT_INTERNAL_TOKEN is
# set on its side — both sides must agree (compose wires one shared value).
VAULT_TOKEN = os.environ.get("VAULT_INTERNAL_TOKEN", "")
VAULT_HEADERS = {"X-Internal-Auth": VAULT_TOKEN} if VAULT_TOKEN else {}
TEMPORAL_SIGNAL_URL = os.environ.get("CASE_API_URL", "http://localhost:8080")
# The signal call below sent no Authorization header at all, wrapped in a
# silent except -- it has always 401'd (case-api's /signal routes require
# auth like every other /v1/tenants/... route), invisibly. WORKER_TOKEN is
# the same service credential case-api validates for idre-workflows' own
# service-to-service calls (authn.middleware, SERVICE_WORKER role).
WORKER_TOKEN = os.environ.get("WORKER_TOKEN", "")
AUTH_HEADERS = {"Authorization": f"Bearer {WORKER_TOKEN}"} if WORKER_TOKEN else {}

minio = Minio(
    os.environ.get("MINIO_ENDPOINT", "localhost:9000"),
    access_key=os.environ.get("MINIO_USER", "idre"),
    secret_key=os.environ.get("MINIO_PASSWORD", "idre-secret"),
    secure=False,
)
# The shared OpenSearch cluster's security plugin is on -- constructed with
# no credentials, every index() call below 401'd (opensearchpy raises
# AuthenticationException), uncaught, which escaped persist() into main()'s
# poll-loop catch-all and overwrote the already-successful analysis with a
# generic ERROR status. Confirmed live.
OPENSEARCH_PASSWORD = os.environ.get("OPENSEARCH_PASSWORD", "")
search = OpenSearch(
    hosts=[os.environ.get("OPENSEARCH_URL", "http://localhost:9200")],
    http_auth=("admin", OPENSEARCH_PASSWORD) if OPENSEARCH_PASSWORD else None,
    use_ssl=False,
)


def fetch_and_decrypt(tenant: str, object_key: str) -> bytes:
    import base64
    ct = minio.get_object("idre-docs", object_key).read()
    resp = httpx.post(f"{VAULT}/docs/open", timeout=60, headers=VAULT_HEADERS, json={
        "tenant": tenant, "key": object_key, "data_b64": base64.b64encode(ct).decode(),
    })
    resp.raise_for_status()
    return base64.b64decode(resp.json()["data_b64"])


def load_case(tenant: str, case_id: str) -> dict | None:
    with psycopg.connect(DSN) as c:
        row = c.execute(
            f"SELECT case_number, qpa_cents FROM tenant_{tenant}.cases WHERE id=%s",
            (case_id,),
        ).fetchone()
    return {"case_number": row[0], "qpa_cents": row[1]} if row else None


def load_manifest_schemas(tenant: str) -> dict:
    """Extraction schemas declared by the tenant's Program Manifest
    (config.manifest.documents.schemas) — the NG manifest-terminology layer.
    Fresh read per document: a schema edit in the admin UI applies to the
    next analysis with no redeploy. Empty/missing = built-in schemas
    (unchanged behavior). Failures degrade to built-ins, never to a crash."""
    try:
        with psycopg.connect(DSN) as c:
            row = c.execute(
                "SELECT config->'manifest'->'documents'->'schemas' "
                "FROM public.program_rules WHERE tenant=%s",
                (tenant,),
            ).fetchone()
        if row and isinstance(row[0], dict):
            return row[0]
    except psycopg.Error as exc:
        print(f"doc-intel: manifest schema load failed for {tenant}: {exc}",
              file=sys.stderr, flush=True)
    return {}


def unwrap_cloudevent(raw: dict) -> dict:
    """This consumer reads Kafka directly (confluent-kafka), not through a
    Dapr app-level subscription, but case-api publishes through its Dapr
    sidecar -- which wraps every message in a CloudEvents envelope
    (real payload nested under "data", top-level "type" forced to
    "com.dapr.event.sent") regardless of the pubsub component's config.
    Without this, every `evt.get("type") == "doc.uploaded"` check below
    silently never matched: the message was still consumed and committed
    (just skipped, no exception raised), so document analysis -- and the
    DOCS_VERIFIED/DOC_ANALYZED signals that unblock onboarding and offer
    workflows -- was dead with zero trace anywhere."""
    if raw.get("specversion") and isinstance(raw.get("data"), dict):
        return raw["data"]
    return raw


def subject(evt: dict) -> tuple[str, str]:
    """("case", id) or ("application", id) -- whichever key the event carries.
    Checked first, before anything touches evt["case_id"] directly: an
    application event has no case_id key at all (not an empty one), so a
    blind evt["case_id"] raises KeyError -- which used to propagate out of
    process(), into the bare `except Exception` in main()'s poll loop, which
    itself called mark() with the same blind evt["case_id"] access and raised
    AGAIN, uncaught, killing the whole consumer (every tenant's doc processing
    stops, not just the one event) until the pod restarted."""
    if evt.get("case_id"):
        return "case", evt["case_id"]
    return "application", evt["application_id"]


def features_for(tenant: str) -> dict:
    """Per-tenant feature toggles, read fresh per document. Orig platform
    stores them at program_rules.config->'features'; NG inside the manifest
    (config->'manifest'->'features'). Absent flag = ON, matching the case-api
    gate semantics — a deploy with no flags configured changes nothing."""
    try:
        with psycopg.connect(DSN) as c:
            row = c.execute(
                "SELECT config FROM public.program_rules WHERE tenant=%s", (tenant,),
            ).fetchone()
    except Exception:
        return {}
    if not row:
        return {}
    cfg = row[0] or {}
    flags = dict((cfg.get("manifest") or {}).get("features") or {})
    flags.update(cfg.get("features") or {})
    return flags


def merge_case_codes(tenant: str, case_id: str, ex: dict) -> None:
    """Lift extracted clinical codes onto the case row so queue filters have
    structured data to work with. Until now ICD-10/CPT/HCPCS extraction was a
    dead end -- it sat in doc_analysis.result and nothing downstream could
    query it. Codes merge (union, deduped, capped) into cases.details, which
    is already GIN-indexed; a bundling vocabulary hit sets
    details.bundling_suspected=true (sticky -- later docs never clear a flag
    an earlier doc earned)."""
    if not ex:
        return
    dx = set(ex.get("diagnosis_codes") or [])
    cpt = set(ex.get("cpt_codes") or [])
    hcpcs = set(ex.get("hcpcs_codes") or [])
    bundling = bool(ex.get("bundling_signals"))
    if not (dx or cpt or hcpcs or bundling):
        return
    with psycopg.connect(DSN, autocommit=True) as c:
        row = c.execute(
            f"SELECT details FROM tenant_{tenant}.cases WHERE id=%s", (case_id,),
        ).fetchone()
        if not row:
            return
        details = dict(row[0] or {})
        for key, vals, cap in (("diagnosis_codes", dx, 50),
                               ("cpt_codes", cpt, 80),
                               ("hcpcs_codes", hcpcs, 50)):
            if vals:
                merged = sorted(set(details.get(key) or []) | vals)[:cap]
                details[key] = merged
        # claim_number is the join key for duplicate-claim detection — it has
        # to live on the case row, not buried in doc_analysis.result.
        claim_no = (ex.get("identifiers") or {}).get("claim_number")
        if claim_no and not details.get("claim_number"):
            details["claim_number"] = claim_no
        if bundling:
            details["bundling_suspected"] = True
        c.execute(
            f"UPDATE tenant_{tenant}.cases SET details=%s, updated_at=now() WHERE id=%s",
            (json.dumps(details), case_id),
        )


def _case_activity(tenant: str, case_id: str, typ: str, body: str) -> None:
    with psycopg.connect(DSN, autocommit=True) as c:
        c.execute(
            """INSERT INTO public.case_activities (tenant, case_id, type, body)
               VALUES (%s,%s,%s,%s)""",
            (tenant, case_id, typ, body[:2000]),
        )


def _set_details(tenant: str, case_id: str, patch: dict) -> None:
    with psycopg.connect(DSN, autocommit=True) as c:
        c.execute(
            f"UPDATE tenant_{tenant}.cases SET details = details || %s::jsonb, updated_at=now() WHERE id=%s",
            (json.dumps(patch), case_id),
        )


def necessity_screen(tenant: str, case_id: str, ex: dict) -> None:
    """dx->CPT plausibility tripwire. Flags go onto the case and the activity
    stream; the clinical queue's human reviewer adjudicates — this screen
    never decides anything by itself."""
    import necessity
    flags = necessity.screen(ex.get("cpt_codes") or [], ex.get("diagnosis_codes") or [])
    if not flags:
        return
    _set_details(tenant, case_id, {"necessity_flags": flags})
    _case_activity(tenant, case_id, "NECESSITY_FLAG",
                   "Automated dx/CPT plausibility screen flagged: " +
                   "; ".join(f"{f['code']} — {f['reason']}" for f in flags[:3]))


def deadline_verification(tenant: str, case_id: str, doc_type: str, ex: dict) -> None:
    """Extract the EOB's denial/processed date and measure it against the
    case's filing date. A large gap is a potential timely-filing defect — the
    attorney's threshold question. Flagged for review, never auto-decided;
    the program's own clock rules (NSA 4bd post-negotiation, AHCA petition
    windows) govern the actual determination."""
    if doc_type != "eob":
        return
    denial = None
    for d in ex.get("dates") or []:
        label = (d.get("label") or "").lower()
        if any(k in label for k in ("denial", "denied", "process", "determin", "remit", "paid", "adjudicat")):
            denial = d.get("iso")
            break
    if not denial:
        return
    with psycopg.connect(DSN) as c:
        row = c.execute(
            f"SELECT opened_at::date, details->>'eob_denial_date' FROM tenant_{tenant}.cases WHERE id=%s",
            (case_id,),
        ).fetchone()
    if not row:
        return
    from datetime import date
    try:
        denial_date = date.fromisoformat(denial[:10])
    except ValueError:
        return
    opened = row[0]
    gap = (opened - denial_date).days
    _set_details(tenant, case_id, {"eob_denial_date": denial_date.isoformat()})
    if gap > 30 and not row[1]:
        _set_details(tenant, case_id, {"filing_deadline_risk": True})
        _case_activity(tenant, case_id, "TIMELY_FILING_RISK",
                       f"EOB denial/processed date {denial_date.isoformat()} is {gap} days before this "
                       "case opened — possible timely-filing defect. Attorney review recommended; the "
                       "program's statutory clock rules govern the actual determination.")


def duplicate_detection(tenant: str, case_id: str, ex: dict) -> None:
    """Same claim number on two open cases in the same tenant = probable
    duplicate filing (double-dip risk). Flags both cases for review."""
    claim_no = (ex.get("identifiers") or {}).get("claim_number")
    if not claim_no:
        return
    with psycopg.connect(DSN, autocommit=True) as c:
        dupes = c.execute(
            f"""SELECT id, case_number FROM tenant_{tenant}.cases
                WHERE id <> %s AND details->>'claim_number' = %s
                  AND status NOT LIKE 'CLOSED%%' LIMIT 10""",
            (case_id, claim_no),
        ).fetchall()
        if not dupes:
            return
        nums = [d[1] for d in dupes]
        _case_activity(tenant, case_id, "DUPLICATE_CLAIM_DETECTED",
                       f"Claim {claim_no} also appears on open case(s): {', '.join(nums)}")
        c.execute(
            f"UPDATE tenant_{tenant}.cases SET details = details || %s::jsonb, updated_at=now() WHERE id=%s",
            (json.dumps({"duplicate_claim_with": nums}), case_id),
        )
        # Mirror the flag onto each duplicate case so it surfaces from either side.
        me = c.execute(
            f"SELECT case_number FROM tenant_{tenant}.cases WHERE id=%s", (case_id,),
        ).fetchone()
        my_num = me[0] if me else str(case_id)
        for dup_id, _ in dupes:
            cur = c.execute(
                f"SELECT details->'duplicate_claim_with' FROM tenant_{tenant}.cases WHERE id=%s",
                (dup_id,),
            ).fetchone()
            lst = list(cur[0] or []) if cur else []
            if my_num not in lst:
                lst.append(my_num)
                c.execute(
                    f"UPDATE tenant_{tenant}.cases SET details = jsonb_set(details, '{{duplicate_claim_with}}', %s::jsonb), updated_at=now() WHERE id=%s",
                    (json.dumps(lst), dup_id),
                )


def persist(evt: dict, ctx: dict) -> None:
    kind, subj_id = subject(evt)
    case_id = subj_id if kind == "case" else None
    application_id = subj_id if kind == "application" else None
    with psycopg.connect(DSN, autocommit=True) as c:
        c.execute(
            """INSERT INTO public.doc_analysis (doc_id, tenant, case_id, application_id, status, doc_type, result)
               VALUES (%s,%s,%s,%s,%s,%s,%s)
               ON CONFLICT (doc_id) DO UPDATE SET status=EXCLUDED.status,
                 doc_type=EXCLUDED.doc_type, result=EXCLUDED.result, analyzed_at=now()""",
            (evt["doc_id"], evt["tenant"], case_id, application_id,
             ctx["status"], ctx.get("doc_type", ""),
             json.dumps({
                 "extracted": ctx.get("extracted", {}),
                 "normalized": ctx.get("normalized", {}),
                 "field_confidence": ctx.get("field_confidence", {}),
                 "findings": ctx.get("findings", []),
                 "seal_detected": ctx.get("seal_detected", False),
                 "table_count": len(ctx.get("tables", [])),
                 "schema_used": ctx.get("schema_used"),
                 "scan_quality_poor": ctx.get("scan_quality_poor", False),
                 "ocr_enhanced": ctx.get("ocr_enhanced", False),
                 "ocr_confidence": ctx.get("ocr_confidence"),
                 "ocr_low_confidence": ctx.get("ocr_low_confidence", False),
                 "ocr_pages": ctx.get("ocr_pages", []),
                 "extracted_fields": ctx.get("extracted_fields", {}),
                 "vlm_field_probability": ctx.get("vlm_field_probability", {}),
             })),
        )
    # Lift clinical codes onto the case itself (structured, queryable) — the
    # doc_analysis row above is the forensic record; this is what the coding
    # queue's bundling filter and downstream reviews actually read.
    if kind == "case":
        ex = ctx.get("extracted_fields") or {}
        merge_case_codes(evt["tenant"], case_id, ex)
        # Downstream screens (each independently feature-toggled per tenant).
        # Each is fail-open: a screen error must never block doc persistence.
        fl = features_for(evt["tenant"])
        for flag, fn in (
            ("necessity_screen", lambda: necessity_screen(evt["tenant"], case_id, ex)),
            ("deadline_verification", lambda: deadline_verification(evt["tenant"], case_id, ctx.get("doc_type", ""), ex)),
            ("duplicate_detection", lambda: duplicate_detection(evt["tenant"], case_id, ex)),
        ):
            if fl.get(flag, True):
                try:
                    fn()
                except Exception as exc:  # noqa: BLE001
                    print(f"doc-intel: {flag} failed (non-fatal): {exc}", file=sys.stderr, flush=True)
    # Best-effort, like the Temporal signal call below it: the Postgres
    # insert just above is the real system of record for analysis status.
    # A search-index outage should never cost the already-persisted result
    # (confirmed live: it did, before this was guarded).
    try:
        search.index(
            index=f"idre-docs-{evt['tenant']}",
            id=evt["doc_id"],
            # stage_index builds the searchable body; fall back to raw fields if
            # the pipeline stopped before indexing (e.g. early ERROR paths).
            body={**ctx.get("index_body", {}), "case_id": case_id, "application_id": application_id}
            if ctx.get("index_body") else {
                "case_id": case_id, "application_id": application_id, "doc_type": ctx.get("doc_type"),
                "text": ctx.get("text", "")[:100000],
                "extracted": ctx.get("extracted", {}),
            },
        )
    except Exception as exc:  # noqa: BLE001 — opensearchpy's exceptions don't share one base worth catching narrowly
        print(f"doc-intel: OpenSearch index failed (non-fatal): {exc}", file=sys.stderr, flush=True)
    if kind != "case":
        return  # case_activities is a case-timeline table; applications have no equivalent here
    # Unified timeline: analysis result appears on the case activity stream
    # (same table case-api writes DOCUMENT_UPLOADED / signals / voice / notes to).
    findings = ctx.get("findings", [])
    summary = f"Document analysis {ctx['status']}: type={ctx.get('doc_type', '?')}"
    if ctx.get("schema_used"):
        summary += f", schema={ctx['schema_used']}"
    if ctx.get("seal_detected"):
        summary += ", seal/stamp detected"
    low_conf = [f for f, c in ctx.get("field_confidence", {}).items() if c == "low"]
    if low_conf:
        summary += f", unverified field(s): {', '.join(low_conf[:5])}"
    if findings:
        summary += f", {len(findings)} finding(s): " + "; ".join(map(str, findings[:3]))
    with psycopg.connect(DSN, autocommit=True) as c:
        c.execute(
            """INSERT INTO public.case_activities (tenant, case_id, type, body)
               VALUES (%s,%s,'ANALYSIS_COMPLETE',%s)""",
            (evt["tenant"], case_id, summary[:2000]),
        )


def process(evt: dict) -> None:
    kind, subj_id = subject(evt)
    if evt.get("sealed"):
        # Sealed offer documents: skip analysis until lawful reveal.
        mark(evt, "SEALED_PENDING_REVEAL")
        return
    if is_blocked(evt, kind, subj_id):
        # Quarantined by a doc.upload program rule — never analyze blocked bytes.
        mark(evt, "BLOCKED")
        return
    raw = fetch_and_decrypt(evt["tenant"], evt["object_key"])
    # Magic-byte gate before any parsing: content the API allowlist can't
    # process (video, archives, unknown binaries — e.g. uploaded before the
    # edge policy existed) is refused cleanly instead of burning OCR/VLM work
    # and landing as an exception-driven ERROR.
    kind_of = sniff_doc_kind(raw, evt.get("content_type", ""))
    if kind_of == "unknown":
        mark(evt, "UNSUPPORTED_TYPE")
        return
    if kind_of == "office":
        # Zip-bomb pre-flight: a hostile .docx that expands past the cap must
        # never reach Docling's parser (worker memory = every tenant's queue).
        safe, reason = ooxml_safe(raw)
        if not safe:
            mark(evt, "UNSUPPORTED_TYPE")
            print(f"doc-intel: refused office container {evt.get('doc_id')}: {reason}",
                  file=sys.stderr, flush=True)
            return
    # Raster pages only for image uploads. PDFs render LAZILY inside the
    # pipeline (ensure_pages) — a born-digital PDF never pays for bitmaps.
    pages, truncated = (bytes_to_pages(raw, evt.get("content_type", ""), kind=kind_of)
                        if kind_of == "image" else ([], False))
    ctx = {
        "filename": evt.get("object_key", ""),
        "raw_bytes": raw,                                   # Docling parses bytes directly
        "content_type": evt.get("content_type", ""),
        "pages": pages,                                     # for OCR fallback + VLM image
    }
    ctx = run_pipeline(ctx, case=load_case(evt["tenant"], subj_id) if kind == "case" else None,
                       schema_overrides=load_manifest_schemas(evt["tenant"]))
    truncated = truncated or ctx.get("pages_truncated", False)
    if truncated:
        # Page-capped render: analysis covers the first DOC_INTEL_MAX_PAGES
        # pages; a human must look at the rest.
        ctx.setdefault("findings", []).append(
            {"field": None, "issue": f"Document exceeds page cap — analysis covers first {len(ctx.get('pages', []))} pages only; remainder needs manual review"})
        ctx["status"] = "ANALYZED_WITH_FINDINGS"
        ctx["result_truncated"] = True
    persist(evt, ctx)
    # doc.analyzed program rules (notify/flag/detail side effects, audited).
    try:
        fire_doc_rules(evt, kind, subj_id, ctx)
    except Exception as exc:  # rules must never crash analysis delivery
        print(f"doc-intel: doc.analyzed rule evaluation failed: {exc}",
              file=sys.stderr, flush=True)
    # Signal the workflow waiting on this analysis (case: DOC_ANALYZED: offer-
    # window-adjacent gates; application: DOCS_VERIFIED: the PENDING_DOCS gate).
    if kind == "case":
        url = f"{TEMPORAL_SIGNAL_URL}/v1/tenants/{evt['tenant']}/cases/{subj_id}/signal"
        payload = {"signal": "DOC_ANALYZED",
                   "data": {"doc_id": evt["doc_id"], "doc_type": ctx.get("doc_type"), "status": ctx["status"]}}
    else:
        url = f"{TEMPORAL_SIGNAL_URL}/v1/tenants/{evt['tenant']}/onboarding/applications/{subj_id}/signal"
        clean = ctx["status"] not in ("ERROR",) and not ctx.get("findings")
        payload = {"signal": "DOCS_VERIFIED",
                   "data": {"clean": clean, "findings": ctx.get("findings", [])}}
    try:
        httpx.post(url, json=payload, headers=AUTH_HEADERS, timeout=10)
    except httpx.HTTPError:
        pass  # signal is best-effort; analysis is already persisted


def mark(evt: dict, status: str) -> None:
    kind, subj_id = subject(evt)
    case_id = subj_id if kind == "case" else None
    application_id = subj_id if kind == "application" else None
    with psycopg.connect(DSN, autocommit=True) as c:
        c.execute(
            """INSERT INTO public.doc_analysis (doc_id, tenant, case_id, application_id, status, doc_type, result)
               VALUES (%s,%s,%s,%s,%s,'','{}')
               ON CONFLICT (doc_id) DO UPDATE SET status=EXCLUDED.status""",
            (evt["doc_id"], evt["tenant"], case_id, application_id, status),
        )


def process_check(evt: dict) -> None:
    """Physical check intake: vault-sealed image -> MICR/OCR/ICR extraction ->
    results posted back to case-api, which does invoice matching. The image
    never lingers unsealed; extraction detail persists via case-api (audit)."""
    check_id = evt["check_id"]
    try:
        raw = fetch_and_decrypt(evt["tenant"], evt["object_key"])
        result = extract_check(raw)
        resp = httpx.post(
            f"{TEMPORAL_SIGNAL_URL}/v1/tenants/{evt['tenant']}/internal/checks/{check_id}/result",
            json=result.as_dict(),
            headers={"Authorization": f"Bearer {os.environ.get('WORKER_TOKEN','')}"},
            timeout=30,
        )
        resp.raise_for_status()
        print(f"check {check_id}: extracted conf={result.confidence} "
              f"amount={result.amount_cents} match={resp.json().get('match')}", flush=True)
    except Exception as exc:  # noqa: BLE001 — tell case-api extraction failed
        print(f"check {check_id} extraction failed: {exc}", file=sys.stderr, flush=True)
        try:
            httpx.post(
                f"{TEMPORAL_SIGNAL_URL}/v1/tenants/{evt['tenant']}/internal/checks/{check_id}/result",
                json={"confidence": "low", "detail": {"error": str(exc)}},
                headers={"Authorization": f"Bearer {os.environ.get('WORKER_TOKEN','')}"},
                timeout=30,
            )
        except Exception:
            pass


def main() -> None:
    consumer = Consumer({
        "bootstrap.servers": os.environ.get("KAFKA_BROKERS", "localhost:9092"),
        "group.id": "doc-intel",
        "auto.offset.reset": "earliest",
        "enable.auto.commit": False,
    })
    consumer.subscribe(["^idre\\..*\\.documents"], on_assign=lambda c, ps: None)

    running = True
    def stop(*_):
        nonlocal running
        running = False
    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)

    # Bounded worker pool: analysis is I/O-heavy (vault fetch, VLM round-
    # trips) plus CPU bursts (OCR). Serial poll->process->commit left the
    # pool idle during every network wait. Documents are processed
    # concurrently but commits stay IN ORDER (head-of-line drain), so an
    # offset is only committed once every preceding message has finished —
    # at-least-once semantics unchanged. DOC_INTEL_WORKERS default 3: the
    # heavy models are process singletons now, so each extra worker costs
    # threads, not model copies.
    workers = int(os.environ.get("DOC_INTEL_WORKERS", "3"))
    pool = ThreadPoolExecutor(max_workers=workers, thread_name_prefix="doc")
    pending = deque()  # (msg, future), FIFO for in-order commit

    # Continuous learning: load ACTIVE extraction synonyms mined from past
    # human corrections (stage_fields applies them immediately), then mine
    # new corrections on an interval. Fail-open: a learning outage must never
    # stop document processing — extraction just runs with the base label set.
    try:
        with psycopg.connect(DSN) as c:
            n = learning.load_active(c)
            if n:
                print(f"doc-intel learning: {n} active label synonyms loaded", flush=True)
        learning.start_learning_loop(lambda: psycopg.connect(DSN), search)
    except Exception as exc:  # noqa: BLE001
        print(f"doc-intel learning: startup skipped: {exc}", flush=True)

    def run(evt: dict) -> None:
        if evt.get("type") == "doc.uploaded":
            process(evt)
        elif evt.get("type") == "check.uploaded":
            process_check(evt)
        elif evt.get("type") == "tpa_doc.uploaded":
            tpa_docs.handle_tpa_doc(evt, {
                "dsn": DSN, "vault": VAULT, "vault_headers": VAULT_HEADERS,
                "minio": minio, "classify_text": classify_text,
                "extract_fields": extract_fields,
                "fetch_and_decrypt": fetch_and_decrypt,
                "features_for": features_for,
            })

    def drain(block: bool = False) -> None:
        while pending and (block or pending[0][1].done()):
            msg, fut = pending[0]
            try:
                fut.result()  # raises only on a bug that escaped process()
            except Exception as exc:  # noqa: BLE001
                # Poison message handling: park on doc_processing_errors,
                # keep consuming. mark() itself must never raise here.
                print(f"doc-intel error: {exc}", file=sys.stderr, flush=True)
                try:
                    mark(unwrap_cloudevent(json.loads(msg.value())), "ERROR")
                except Exception as mark_exc:  # noqa: BLE001
                    print(f"doc-intel error (while marking a previous error): {mark_exc}",
                          file=sys.stderr, flush=True)
            consumer.commit(msg)
            pending.popleft()
            block = False

    print(f"doc-intel consuming idre.*.documents (workers={workers}) ...", flush=True)
    while running:
        drain()
        if len(pending) >= workers * 2:
            time.sleep(0.05)  # backpressure: don't outrun the pool
            continue
        msg = consumer.poll(0.5)
        if msg is None or msg.error():
            continue
        try:
            evt = unwrap_cloudevent(json.loads(msg.value()))
            pending.append((msg, pool.submit(run, evt)))
        except Exception as exc:  # noqa: BLE001 — unparseable envelope
            print(f"doc-intel error: {exc}", file=sys.stderr, flush=True)
            consumer.commit(msg)
    # graceful shutdown: finish in-flight work before closing
    while pending:
        drain(block=True)
    pool.shutdown(wait=True)
    consumer.close()


if __name__ == "__main__":
    main()
