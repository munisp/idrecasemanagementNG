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

minio = Minio(
    os.environ.get("MINIO_ENDPOINT", "localhost:9000"),
    access_key=os.environ.get("MINIO_USER", "idre"),
    secret_key=os.environ.get("MINIO_PASSWORD", "idre-secret"),
    secure=False,
)

TEMPLATE_BUCKET = "idre-templates"
DOC_BUCKET = "idre-docs"


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
    resp = httpx.post(f"{VAULT}/docs/seal", timeout=60, json={
        "tenant": tenant, "key": key, "data_b64": base64.b64encode(pt).decode(),
    })
    resp.raise_for_status()
    return base64.b64decode(resp.json()["data_b64"])


def template_config(tenant: str, key: str) -> dict:
    """Resolve a letter template. Precedence:
    1. config.letter_templates[] (tenant-specific overrides, as before)
    2. the Program Manifest's letter pack (letters.templates + letters.pack) —
       sector packs ship their statutory notice set; tenants override per key.
    """
    with psycopg.connect(DSN) as c:
        row = c.execute(
            "SELECT config->'letter_templates', config->'manifest'->'letters' "
            "FROM public.program_rules WHERE tenant=%s",
            (tenant,),
        ).fetchone()
    for t in ((row[0] if row and row[0] else []) or []):
        if t.get("key") == key:
            return t
    letters = row[1] if row and row[1] else None
    if letters and key in (letters.get("templates") or []):
        # Pack entry: template object lives under the pack prefix.
        return {"key": key, "pack": letters.get("pack", ""),
                "filename": "{case_number}-" + key + ".pdf", "folder": "CORRESPONDENCE"}
    raise KeyError(f"no letter template {key} for tenant {tenant}")


def handle(evt: dict) -> None:
    tenant, case_id, key = evt["tenant"], evt["case_id"], evt["template"]
    requested_by = evt.get("requested_by", "system")
    tpl = template_config(tenant, key)
    fields = load_case(tenant, case_id)

    # Tenant override wins; otherwise the sector pack's copy (letters.pack prefix).
    if tpl.get("pack"):
        template_key = f"{tpl['pack']}/{key}.docx"
    else:
        template_key = f"{tenant}/{key}.docx"
    obj = minio.get_object(TEMPLATE_BUCKET, template_key)
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
