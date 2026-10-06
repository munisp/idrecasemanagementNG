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

import httpx
import psycopg
from confluent_kafka import Consumer
from minio import Minio
from opensearchpy import OpenSearch

from pipeline import bytes_to_pages, ooxml_safe, run_pipeline, sniff_doc_kind
from rules_engine import expand_template, fire_rules, load_rules
from check_processor import extract_check


def load_manifest_schemas(tenant: str) -> dict:
    """Extraction schemas declared by the tenant's Program Manifest
    (config.manifest.documents.schemas). Fresh read per document — a schema
    edit in the admin UI applies to the next analysis with no redeploy.
    Empty/missing = built-in healthcare schemas (unchanged behavior)."""
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
search = OpenSearch(
    hosts=[os.environ.get("OPENSEARCH_URL", "http://localhost:9200")],
    use_ssl=False,
)


def fetch_and_decrypt(tenant: str, object_key: str) -> bytes:
    import base64
    ct = minio.get_object("idre-docs", object_key).read()
    resp = httpx.post(f"{VAULT}/docs/open", timeout=60, json={
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
             })),
        )
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
    pages, truncated = bytes_to_pages(raw, evt.get("content_type", ""), kind=kind_of)
    ctx = {
        "filename": evt.get("object_key", ""),
        "raw_bytes": raw,                                   # Docling parses bytes directly
        "content_type": evt.get("content_type", ""),
        "pages": pages,                                     # for OCR fallback + VLM image
    }
    ctx = run_pipeline(ctx, case=load_case(evt["tenant"], subj_id) if kind == "case" else None,
                       schema_overrides=load_manifest_schemas(evt["tenant"]))
    if truncated:
        # Page-capped render: analysis covers the first DOC_INTEL_MAX_PAGES
        # pages; a human must look at the rest.
        ctx.setdefault("findings", []).append(
            {"field": None, "issue": f"Document exceeds page cap — analysis covers first {len(pages)} pages only; remainder needs manual review"})
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
    results posted back to case-api, which does invoice matching."""
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
        print(f"check {check_id}: extracted conf={result.confidence}", flush=True)
    except Exception as exc:  # noqa: BLE001
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

    print("doc-intel consuming idre.*.documents ...", flush=True)
    while running:
        msg = consumer.poll(1.0)
        if msg is None or msg.error():
            continue
        try:
            evt = json.loads(msg.value())
            if evt.get("type") == "doc.uploaded":
                process(evt)
            elif evt.get("type") == "check.uploaded":
                process_check(evt)
            consumer.commit(msg)
        except Exception as exc:  # noqa: BLE001
            print(f"doc-intel error: {exc}", file=sys.stderr, flush=True)
            # Poison message handling: park on doc_processing_errors, keep
            # consuming. mark() itself must never be able to raise here -- it
            # used to (a blind evt["case_id"] on an application event), which
            # escaped uncaught and killed the whole consumer, not just this
            # one message, stopping document processing for every tenant.
            try:
                mark(json.loads(msg.value()), "ERROR")
            except Exception as mark_exc:  # noqa: BLE001
                print(f"doc-intel error (while marking a previous error): {mark_exc}", file=sys.stderr, flush=True)
            consumer.commit(msg)
    consumer.close()


if __name__ == "__main__":
    main()
