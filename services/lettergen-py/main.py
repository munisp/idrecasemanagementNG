# Author: Patrick Munis at newwave
"""lettergen service: generates party-facing letters from DOCX templates.

Consumes letter.requested events (Kafka topic idre.<tenant>.letters, produced
via the transactional outbox), then:
  1. fetch the tenant's DOCX template from MinIO (idre-templates/<tenant>/<key>.docx)
  2. merge case fields ({case_number}, {provider_name}, {payer_name}, {qpa},
     {date}, {details.*} — any key in cases.details)
  3. convert to PDF with LibreOffice headless
  4. seal through the vault, store in idre-docs, insert the documents row
     (folder from the template config, scan_status CLEAN — generated content)
  5. open a PENDING qa_reviews row — every outbound letter passes QA (G4)
     before it is attached to correspondence and mailed.

Templates are per-tenant and replaceable at runtime: upload a new DOCX to
idre-templates/<tenant>/<key>.docx and the next letter uses it. No deploys.

Run: python main.py
"""

from __future__ import annotations

import base64
import json
import os
import re
import signal
import subprocess
import sys
import tempfile
from datetime import date

import httpx
import psycopg
from confluent_kafka import Consumer
from docx import Document
from minio import Minio

DSN = os.environ.get("DATABASE_URL", "postgres://idre:idre@localhost:5432/idre")
VAULT = os.environ.get("VAULT_URL", "http://localhost:8081")
# Vault enforces X-Internal-Auth on every route when VAULT_INTERNAL_TOKEN is
# set on its side — both sides must agree (compose wires one shared value).
VAULT_TOKEN = os.environ.get("VAULT_INTERNAL_TOKEN", "")
VAULT_HEADERS = {"X-Internal-Auth": VAULT_TOKEN} if VAULT_TOKEN else {}

minio = Minio(
    os.environ.get("MINIO_ENDPOINT", "localhost:9000"),
    access_key=os.environ.get("MINIO_USER", "idre"),
    secret_key=os.environ.get("MINIO_PASSWORD", "idre-secret"),
    secure=False,
)

TEMPLATE_BUCKET = "idre-templates"
DOC_BUCKET = "idre-docs"


def features_for(tenant: str) -> dict:
    """Per-tenant feature toggles (orig: config->'features'; NG: manifest).
    Absent = ON, matching case-api gate semantics."""
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


def load_citations(tenant: str, case_id: str) -> dict:
    """Determination-letter auto-citation: the extracted codes, denial date,
    determination amount and rationale the letter relies on, flattened to
    {citation_*} placeholders. Templates that lack them render unchanged —
    templates that include them make the certified letter self-documenting."""
    out: dict = {}
    with psycopg.connect(DSN) as c:
        det = c.execute(
            """SELECT d.amount_cents, d.rationale, d.issued_at, o.party_id
               FROM public.determinations d JOIN public.offers o ON o.id = d.selected_offer_id
               WHERE d.tenant=%s AND d.case_id=%s""",
            (tenant, case_id),
        ).fetchone()
        row = c.execute(
            f"SELECT details FROM tenant_{tenant}.cases WHERE id=%s", (case_id,),
        ).fetchone()
    details = (row[0] if row else {}) or {}
    codes = sorted(set((details.get("cpt_codes") or []) + (details.get("hcpcs_codes") or [])))
    if codes:
        out["citation_codes"] = ", ".join(codes[:20])
    if details.get("diagnosis_codes"):
        out["citation_diagnoses"] = ", ".join(details["diagnosis_codes"][:20])
    if details.get("eob_denial_date"):
        out["citation_denial_date"] = str(details["eob_denial_date"])
    if det:
        out["citation_determination_amount"] = f"${(det[0] or 0) / 100:,.2f}"
        out["citation_rationale"] = det[1] or ""
        out["citation_prevailing_party"] = det[3] or ""
        out["citation_issued_at"] = det[2].date().isoformat() if det[2] else ""
    return out


def load_case(tenant: str, case_id: str) -> dict:
    with psycopg.connect(DSN) as c:
        row = c.execute(
            f"""SELECT case_number, provider_id, payer_id, qpa_cents, details
                FROM tenant_{tenant}.cases WHERE id=%s""",
            (case_id,),
        ).fetchone()
    if not row:
        raise KeyError(f"case {case_id} not found")
    details = row[4] or {}
    fields = {
        "case_number": row[0],
        "provider_name": row[1] or "",
        "payer_name": row[2] or "",
        "qpa": f"${(row[3] or 0) / 100:,.2f}",
        "date": date.today().isoformat(),
    }
    for k, v in details.items():
        fields[f"details.{k}"] = str(v)
    return fields


def merge_docx(template: bytes, fields: dict) -> bytes:
    """Replace {placeholder} tokens across paragraphs and tables. Runs are
    joined per paragraph before substitution so tokens split across runs by
    Word's spell-checker still merge."""
    with tempfile.NamedTemporaryFile(suffix=".docx", delete=False) as tf:
        tf.write(template)
        path = tf.name
    doc = Document(path)

    def sub(text: str) -> str:
        return re.sub(r"\{([a-z0-9_.]+)\}", lambda m: fields.get(m.group(1), m.group(0)), text, flags=re.I)

    for para in doc.paragraphs:
        merged = sub("".join(r.text for r in para.runs))
        for r in para.runs:
            r.text = ""
        if para.runs:
            para.runs[0].text = merged
        elif merged:
            para.add_run(merged)
    for table in doc.tables:
        for row in table.rows:
            for cell in row.cells:
                for para in cell.paragraphs:
                    merged = sub("".join(r.text for r in para.runs))
                    for r in para.runs:
                        r.text = ""
                    if para.runs:
                        para.runs[0].text = merged
    out = path + ".merged.docx"
    doc.save(out)
    with open(out, "rb") as f:
        return f.read()


def to_pdf(docx_bytes: bytes) -> bytes:
    with tempfile.TemporaryDirectory() as td:
        src = os.path.join(td, "letter.docx")
        with open(src, "wb") as f:
            f.write(docx_bytes)
        subprocess.run(
            ["soffice", "--headless", "--convert-to", "pdf", "--outdir", td, src],
            check=True, capture_output=True, timeout=120,
        )
        with open(os.path.join(td, "letter.pdf"), "rb") as f:
            return f.read()


def vault_seal(tenant: str, key: str, pt: bytes) -> bytes:
    resp = httpx.post(f"{VAULT}/docs/seal", timeout=60, headers=VAULT_HEADERS, json={
        "tenant": tenant, "key": key, "data_b64": base64.b64encode(pt).decode(),
    })
    resp.raise_for_status()
    return base64.b64decode(resp.json()["data_b64"])


def template_config(tenant: str, key: str) -> dict:
    with psycopg.connect(DSN) as c:
        row = c.execute(
            "SELECT config->'letter_templates' FROM public.program_rules WHERE tenant=%s",
            (tenant,),
        ).fetchone()
    for t in (row[0] if row and row[0] else []):
        if t.get("key") == key:
            return t
    raise KeyError(f"no letter template {key} for tenant {tenant}")


def handle(evt: dict) -> None:
    tenant, case_id, key = evt["tenant"], evt["case_id"], evt["template"]
    requested_by = evt.get("requested_by", "system")
    tpl = template_config(tenant, key)
    fields = load_case(tenant, case_id)
    if key == "determination_letter" and features_for(tenant).get("letter_citation", True):
        fields.update(load_citations(tenant, case_id))

    obj = minio.get_object(TEMPLATE_BUCKET, f"{tenant}/{key}.docx")
    merged = merge_docx(obj.read(), fields)
    pdf = to_pdf(merged)

    filename = tpl.get("filename", "{case_number}.pdf").format(**fields)
    object_key = f"{tenant}/{case_id}/{key}-{date.today().isoformat()}.pdf"
    ct = vault_seal(tenant, object_key, pdf)
    minio.put_object(DOC_BUCKET, object_key, data=__import__("io").BytesIO(ct), length=len(ct))

    with psycopg.connect(DSN, autocommit=True) as c:
        c.execute(
            f"""INSERT INTO tenant_{tenant}.documents
                  (case_id, object_key, size_bytes, content_type, sealed,
                   uploaded_by, version, scan_status, filename, folder)
                VALUES (%s,%s,%s,'application/pdf',false,%s,1,'CLEAN',%s,%s)""",
            (case_id, object_key, len(pdf), f"lettergen:{requested_by}", filename,
             tpl.get("folder", "CORRESPONDENCE")),
        )
        # Every letter goes through QA before it is mailed (G4).
        body_preview = re.sub(r"\s+", " ", " ".join(
            p.text for p in Document(__import__("io").BytesIO(merged)).paragraphs if p.text.strip()
        ))[:4000]
        c.execute(
            """INSERT INTO public.qa_reviews
                 (tenant, case_id, artifact, channel, subject, body, status, drafted_by)
               VALUES (%s,%s,%s,'letter',%s,%s,'PENDING',%s)""",
            (tenant, case_id, key, tpl.get("subject", key).format(**fields),
             body_preview, requested_by),
        )
        c.execute(
            """INSERT INTO public.notifications (tenant, user_sub, type, body, link)
               VALUES (%s,'*','QA_REVIEW',%s,%s)""",
            (tenant, f"Letter {key} generated for case {fields['case_number']} — awaiting QA",
             f"#/qa"),
        )
        c.execute(
            f"""INSERT INTO public.case_activities (tenant, case_id, type, body)
                VALUES (%s,%s,'LETTER_GENERATED',%s)""",
            (tenant, case_id, f"{filename} generated from template {key}, sealed, queued for QA"),
        )
    print(f"[lettergen] {tenant}/{case_id}: {key} -> {filename}", flush=True)


def main() -> None:
    consumer = Consumer({
        "bootstrap.servers": os.environ.get("KAFKA_BROKERS", "localhost:9092"),
        "group.id": "lettergen",
        "auto.offset.reset": "earliest",
        "enable.auto.commit": False,
    })
    consumer.subscribe([r"^idre\..*\.letters$"])

    running = True
    def stop(*_):
        nonlocal running
        running = False
    signal.signal(signal.SIGINT, stop)
    signal.signal(signal.SIGTERM, stop)

    print("[lettergen] listening for letter requests", flush=True)
    while running:
        msg = consumer.poll(1.0)
        if msg is None or msg.error():
            continue
        try:
            evt = json.loads(msg.value())
            handle(evt)
            consumer.commit(msg)  # commit only after the letter exists
        except Exception as e:  # noqa: BLE001 — poison event: log, skip once committed offsets move past
            print(f"[lettergen] ERROR {e}", file=sys.stderr, flush=True)
            consumer.commit(msg)
    consumer.close()


if __name__ == "__main__":
    main()
