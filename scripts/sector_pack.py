#!/usr/bin/env python3
"""sector_pack.py — build, validate, and install sector packs (NG phase 5).

A sector pack is a directory (or zip) containing everything needed to onboard
a sector × jurisdiction program with no code changes:

    pack/
      manifest.json       # Program Manifest (terminology, lifecycle, clocks,
                          #   rules, documents.schemas, letters, roster)
      letters/*.docx      # statutory notice templates (merge fields per manifest)
      README.md           # provenance: statutes, versions, contacts

Usage:
    python3 sector_pack.py validate <pack_dir>
    python3 sector_pack.py build    <pack_dir> <out.zip>
    python3 sector_pack.py install  <pack_dir> --tenant <slug> \
                                    [--dsn postgres://...] [--minio host] [--dry-run]

Install writes the manifest into public.program_rules.config.manifest
(audited by the API path in production; this tool is for ops/bootstrap) and
uploads letter templates to MinIO under the manifest's letters.pack prefix.
"""

from __future__ import annotations

import argparse
import io
import json
import re
import sys
import zipfile
from pathlib import Path

KNOWN_ENGINES = {"qpa", "fee_schedule", "comparable", "final_offer"}
FIELD_TYPES = {"text", "email", "number", "date", "select"}
NAME_RE = re.compile(r"^[a-z][a-z0-9_]{0,40}$")


def validate_manifest(m: dict) -> list[str]:
    """Mirror of validateManifest (Go) — packs must pass BOTH; keeping the
    checks identical is the contract (see manifest_test.go)."""
    errors = []
    if not m.get("program"):
        errors.append("program is required")
    if not m.get("version"):
        errors.append("version is required")
    t = m.get("terminology") or {}
    for k in ("case_noun", "case_plural", "party_a", "party_b", "neutral", "intake_noun"):
        if not t.get(k):
            errors.append(f"terminology.{k} is required")
    for kind in ("intake_statuses", "case_statuses"):
        stages = (m.get("lifecycle") or {}).get(kind) or []
        if not stages:
            errors.append(f"lifecycle.{kind} must declare at least one status")
        seen = set()
        for st in stages:
            name = st.get("name", "")
            if not name:
                errors.append(f"lifecycle.{kind}: every status needs a name")
            elif name in seen:
                errors.append(f"lifecycle.{kind}: duplicate status {name!r}")
            seen.add(name)
            if not st.get("label"):
                errors.append(f"lifecycle.{kind}: status {name!r} needs a label")
            if st.get("terminal") and st.get("anchors_clock"):
                errors.append(f"lifecycle.{kind}: terminal status {name!r} cannot anchor a clock")
    seen = set()
    for c in m.get("clocks") or []:
        name = c.get("name", "")
        if not name or name in seen:
            errors.append(f"clocks: missing or duplicate name {name!r}")
        seen.add(name)
        if not isinstance(c.get("days"), int) or c["days"] <= 0:
            errors.append(f"clocks.{name}: days must be a positive integer")
        if c.get("day_type") not in ("calendar", "business"):
            errors.append(f"clocks.{name}: day_type must be calendar or business")
        if not c.get("basis"):
            errors.append(f"clocks.{name}: basis is required")
    seen = set()
    for f in m.get("intake_fields") or []:
        n = f.get("name", "")
        if not NAME_RE.match(n):
            errors.append(f"intake_fields: invalid field name {n!r}")
        elif n in seen:
            errors.append(f"intake_fields: duplicate field {n!r}")
        seen.add(n)
        if not f.get("label"):
            errors.append(f"intake_fields.{n}: label required")
        if f.get("type") not in FIELD_TYPES:
            errors.append(f"intake_fields.{n}: unknown type {f.get('type')!r}")
        elif f["type"] == "select" and not f.get("options"):
            errors.append(f"intake_fields.{n}: select field needs options")
    eng = (m.get("determination") or {}).get("engine")
    if eng and eng not in KNOWN_ENGINES:
        errors.append(f"determination.engine {eng!r} unknown (registered: {', '.join(sorted(KNOWN_ENGINES))})")
    return errors


def cmd_validate(pack: Path) -> int:
    errors = []
    mp = pack / "manifest.json"
    if not mp.exists():
        print("✗ manifest.json missing")
        return 1
    try:
        m = json.loads(mp.read_text())
    except json.JSONDecodeError as e:
        print(f"✗ manifest.json invalid JSON: {e}")
        return 1
    errors += validate_manifest(m)
    # Letters referenced by the manifest must exist in the pack.
    pack_prefix = (m.get("letters") or {}).get("pack", "")
    for key in (m.get("letters") or {}).get("templates") or []:
        if not (pack / "letters" / f"{key}.docx").exists():
            errors.append(f"letters: template {key!r} declared but letters/{key}.docx missing")
    if not pack_prefix and (m.get("letters") or {}).get("templates"):
        errors.append("letters.pack prefix required when templates are declared")
    if errors:
        for e in errors:
            print(f"✗ {e}")
        return 1
    print(f"✓ {m['program']}@{m['version']} — valid "
          f"({len((m.get('lifecycle') or {}).get('case_statuses') or [])} case statuses, "
          f"{len(m.get('clocks') or [])} clocks, "
          f"{len((m.get('letters') or {}).get('templates') or [])} letter templates)")
    return 0


def cmd_build(pack: Path, out: Path) -> int:
    if cmd_validate(pack) != 0:
        return 1
    with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as z:
        for f in sorted(pack.rglob("*")):
            if f.is_file():
                z.write(f, f.relative_to(pack))
    print(f"✓ built {out} ({out.stat().st_size} bytes)")
    return 0


def cmd_install(pack: Path, tenant: str, dsn: str, minio_ep: str, dry: bool) -> int:
    if cmd_validate(pack) != 0:
        return 1
    m = json.loads((pack / "manifest.json").read_text())
    manifest_json = json.dumps(m)
    print(f"→ tenant {tenant}: manifest {m['program']}@{m['version']}")
    if dry:
        print("dry run — nothing written")
        return 0
    import psycopg  # deferred: validate/build don't need the DB driver

    with psycopg.connect(dsn, autocommit=True) as c:
        c.execute(
            """INSERT INTO public.program_rules (tenant, program, config)
               VALUES (%s, %s, jsonb_build_object('manifest', %s::jsonb))
               ON CONFLICT (tenant) DO UPDATE
               SET config = jsonb_set(program_rules.config, '{manifest}', %s::jsonb, true),
                   program = EXCLUDED.program, updated_at = now()""",
            (tenant, m["program"], manifest_json, manifest_json),
        )
        c.execute(
            """INSERT INTO public.rule_changes (tenant, changed_by, note, before, after)
               VALUES (%s, 'sector-pack-tool', %s, '{}'::jsonb,
                       jsonb_build_object('manifest', %s::jsonb))""",
            (tenant, f"sector pack install: {m['program']}@{m['version']}", manifest_json),
        )
    print(f"✓ manifest installed for tenant {tenant} (audit row written)")

    letters = list((pack / "letters").glob("*.docx"))
    if letters:
        from minio import Minio

        mc = Minio(minio_ep, access_key="idre", secret_key="idre-secret", secure=False)
        prefix = m["letters"]["pack"]
        for f in letters:
            mc.fput_object("idre-templates", f"{prefix}/{f.name}", str(f))
            print(f"✓ uploaded letters/{f.name} → idre-templates/{prefix}/{f.name}")
    return 0


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)
    for name in ("validate", "build", "install"):
        p = sub.add_parser(name)
        p.add_argument("pack", type=Path)
        if name == "build":
            p.add_argument("out", type=Path)
        if name == "install":
            p.add_argument("--tenant", required=True)
            p.add_argument("--dsn", default="postgres://idre:idre@localhost:5432/idre")
            p.add_argument("--minio", default="localhost:9000")
            p.add_argument("--dry-run", action="store_true")
    args = ap.parse_args()
    if args.cmd == "validate":
        return cmd_validate(args.pack)
    if args.cmd == "build":
        return cmd_build(args.pack, args.out)
    return cmd_install(args.pack, args.tenant, args.dsn, args.minio, args.dry_run)


if __name__ == "__main__":
    sys.exit(main())
