"""Pipeline stage tests — heavy models (docling/paddleocr) and the OpenAI
client are stubbed; the endpoint under test is the LOCAL Ollama, never a
network service."""

import sys
import time
import types

import pytest

import pipeline as pl


# --- classify --------------------------------------------------------------

def test_classify_strong_signal_wins():
    dtype, score, margin = pl.classify_text(
        "eob.pdf", "Explanation of Benefits. Allowed amount: $120.00. Patient responsibility applies.")
    assert dtype == "eob"
    assert score >= pl.CLASSIFY_MIN_SCORE


def test_classify_no_signal_is_unrelated():
    dtype, score, margin = pl.classify_text("menu.pdf", "today's specials: soup, salad, sandwich")
    assert dtype == "unrelated"
    assert score == 0


def test_stage_classify_flags_thin_margin():
    ctx = pl.stage_classify({"filename": "x.pdf",
                             "text": "determination and claim number and cpt"}, {})
    assert "classify_ambiguous" in ctx


# --- model singletons --------------------------------------------------------

def test_model_singleton_builds_once_even_under_threads():
    pl._MODEL_CACHE.clear()
    builds = []

    def factory():
        builds.append(1)
        time.sleep(0.02)  # widen the race window
        return object()

    from concurrent.futures import ThreadPoolExecutor
    with ThreadPoolExecutor(max_workers=8) as pool:
        objs = list(pool.map(lambda _: pl._model(("test", 1), factory), range(16)))
    assert len(builds) == 1
    assert all(o is objs[0] for o in objs)


def test_model_cache_keys_by_config():
    pl._MODEL_CACHE.clear()
    a = pl._model(("k", True), lambda: "A")
    b = pl._model(("k", False), lambda: "B")
    assert a == "A" and b == "B"


# --- pipeline spec cache -----------------------------------------------------

def test_load_pipeline_caches_and_invalidates_on_mtime(tmp_path):
    p = tmp_path / "spec.yaml"
    p.write_text("stages: []\n")
    s1 = pl.load_pipeline(str(p))
    s2 = pl.load_pipeline(str(p))
    assert s1 is s2  # cached object, not a re-parse
    time.sleep(0.02)
    p.write_text("stages: [{name: classify}]\n")
    import os
    os.utime(str(p), (time.time() + 5, time.time() + 5))
    s3 = pl.load_pipeline(str(p))
    assert s3["stages"] == [{"name": "classify"}]


# --- verify / grounding ------------------------------------------------------

def test_grounded_tolerates_punctuation_variance():
    hay = "claim number: abc-12345, billed $1,234.56"
    squashed = pl._grounded.__defaults__  # not used; explicit below
    import re
    hs = re.sub(r"[^a-z0-9]", "", hay)
    assert pl._grounded("ABC 12345", hay, hs)
    assert pl._grounded("$1234.56", hay, hs)
    assert not pl._grounded("ZZZ-999", hay, hs)


def test_stage_verify_drops_ungrounded_from_trusted_normalized():
    ctx = {
        "markdown": "billed amount $500.00 for CPT 99213",
        "extracted": {"billed_amount_usd": 500.0, "qpa_usd": 9999.0},
        "normalized": {"billed_amount_usd": 500.0, "qpa_usd": 9999.0},
    }
    ctx = pl.stage_verify(ctx, {})
    assert ctx["field_confidence"]["billed_amount_usd"] == "high"
    assert ctx["field_confidence"]["qpa_usd"] == "low"
    assert "qpa_usd" in ctx["ungrounded_fields"]
    assert "qpa_usd" not in ctx["normalized"]
    assert ctx["normalized"]["billed_amount_usd"] == 500.0


# --- normalization -----------------------------------------------------------

def test_norm_amount_and_date():
    assert pl._norm_amount("$1,234.56") == 1234.56
    assert pl._norm_amount("garbage") is None
    assert pl._norm_date("January 5, 2026") == "2026-01-05"
    assert pl._norm_date("01/05/26") == "2026-01-05"
    assert pl._norm_date("2026-13-45") is None


def test_merge_extractions_first_non_null_and_list_union():
    merged = pl._merge_extractions(
        [{"a": 1, "b": ["x", "y"], "c": None},
         {"a": 2, "b": ["y", "z"], "c": "seen"}],
        ["a", "b", "c"])
    assert merged == {"a": 1, "b": ["x", "y", "z"], "c": "seen"}


# --- VLM extraction: local Ollama stubbed ------------------------------------

def _stub_openai(monkeypatch):
    """stage_vlm_extract builds its client via `from openai import OpenAI`;
    inject a fake module so no real client (or network) exists."""
    fake = types.ModuleType("openai")

    class OpenAI:  # noqa: D401 - test stub
        def __init__(self, **kwargs):
            self.kwargs = kwargs

    fake.OpenAI = OpenAI
    monkeypatch.setitem(sys.modules, "openai", fake)


def _long_ctx(monkeypatch, n_chunks=4):
    monkeypatch.setattr(pl, "_chunk_markdown",
                        lambda md, n: [f"chunk-{i} billed amount ${100+i}.00" for i in range(n_chunks)])
    return {"markdown": "x" * 50, "doc_type": "idr_claim", "pages": []}


_SCHEMAS = {"idr_claim": {"fields": ["billed_amount_usd", "claim_number"]}}


def test_vlm_chunks_run_concurrently_and_merge_in_order(monkeypatch):
    _stub_openai(monkeypatch)
    pl._MODEL_CACHE.clear()
    calls = []

    def fake_call(client, model, content, max_tokens):
        import re as _re
        calls.append(time.monotonic())
        time.sleep(0.15)  # simulate Ollama round-trip
        m = _re.search(r"section (\d+) of", content[0]["text"])
        i = int(m.group(1)) - 1
        return {"_in_domain": True, "billed_amount_usd": 100 + i,
                "claim_number": f"C{i}"}

    monkeypatch.setattr(pl, "_vlm_call", fake_call)
    ctx = _long_ctx(monkeypatch)
    t0 = time.monotonic()
    ctx = pl.stage_vlm_extract(ctx, {"endpoint": "http://ollama:11434/v1",
                                     "model": "qwen2.5:7b-instruct",
                                     "parallel_chunks": 4,
                                     "chunk_chars": 10}, _SCHEMAS)
    elapsed = time.monotonic() - t0
    assert elapsed < 0.45  # 4 x 0.15s serial would be 0.60s+
    # merge is order-deterministic: first non-null wins -> chunk 0's amount
    assert ctx["extracted"]["billed_amount_usd"] == 100
    assert ctx["in_domain"] is True


def test_vlm_parallel_default_is_conservative_for_local_ollama(monkeypatch):
    """Without explicit config the concurrency default must be gentle —
    the endpoint is the shared local Ollama, not OpenAI."""
    _stub_openai(monkeypatch)
    pl._MODEL_CACHE.clear()
    inflight = {"cur": 0, "max": 0}

    def fake_call(client, model, content, max_tokens):
        inflight["cur"] += 1
        inflight["max"] = max(inflight["max"], inflight["cur"])
        time.sleep(0.1)
        inflight["cur"] -= 1
        return {"_in_domain": True, "billed_amount_usd": 1}

    monkeypatch.setattr(pl, "_vlm_call", fake_call)
    monkeypatch.delenv("VLM_PARALLEL_CHUNKS", raising=False)
    ctx = _long_ctx(monkeypatch, n_chunks=6)
    pl.stage_vlm_extract(ctx, {"endpoint": "http://ollama:11434/v1",
                               "model": "m", "chunk_chars": 10}, _SCHEMAS)
    assert inflight["max"] <= 2


def test_vlm_out_of_domain_short_circuits(monkeypatch):
    _stub_openai(monkeypatch)
    pl._MODEL_CACHE.clear()
    monkeypatch.setattr(pl, "_vlm_call",
                        lambda *a, **k: {"_in_domain": False, "billed_amount_usd": None})
    ctx = _long_ctx(monkeypatch, n_chunks=1)
    ctx = pl.stage_vlm_extract(ctx, {"endpoint": "http://x/v1", "model": "m",
                                     "chunk_chars": 10}, _SCHEMAS)
    assert ctx["in_domain"] is False


def test_vlm_client_cached_per_endpoint(monkeypatch):
    _stub_openai(monkeypatch)
    pl._MODEL_CACHE.clear()
    monkeypatch.setattr(pl, "_vlm_call",
                        lambda *a, **k: {"_in_domain": True})
    cfg = {"endpoint": "http://ollama:11434/v1", "model": "m", "chunk_chars": 10}
    pl.stage_vlm_extract(_long_ctx(monkeypatch, 1), cfg, _SCHEMAS)
    pl.stage_vlm_extract(_long_ctx(monkeypatch, 1), cfg, _SCHEMAS)
    clients = [k for k in pl._MODEL_CACHE if k[0] == "vlm_client"]
    assert len(clients) == 1


# --- scan quality (cv2 available, no paddle needed) --------------------------

def test_assess_page_quality_flags_blur_not_crisp():
    from PIL import Image, ImageDraw, ImageFilter, ImageFont
    # NB: int fill on an RGB image is a packed color (255 -> red), not white
    crisp = Image.new("RGB", (1000, 1400), (255, 255, 255))
    d = ImageDraw.Draw(crisp)
    try:
        f = ImageFont.truetype("/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf", 28)
    except OSError:
        f = ImageFont.load_default()
    for y in range(60, 1300, 44):
        d.text((50, y), "Billed amount $1,234.56  CPT 99213  NPI 1234567890",
               font=f, fill=(0, 0, 0))
    blurry = crisp.filter(ImageFilter.GaussianBlur(6))
    q_crisp = pl.assess_page_quality(crisp)
    q_blur = pl.assess_page_quality(blurry)
    assert q_crisp["blur"] > q_blur["blur"]
    assert q_blur["poor"] is True
    assert q_crisp["poor"] is False


# --- tenant schema overrides --------------------------------------------------

def test_run_pipeline_merges_schema_overrides(monkeypatch):
    """NG Program Manifest schemas must SHADOW same-named built-ins and extend
    with new ones, without touching the pipeline spec on disk."""
    captured = {}
    fake_spec = {"stages": [{"name": "vlm_extract", "enabled": True}],
                 "schemas": {"idr_claim": {"fields": {"builtin": {}}}}}
    monkeypatch.setattr(pl, "load_pipeline", lambda: fake_spec)
    def fake_extract(ctx, cfg, schemas):
        captured["schemas"] = schemas
        return ctx
    monkeypatch.setattr(pl, "stage_vlm_extract", fake_extract)
    pl.run_pipeline({}, schema_overrides={"idr_claim": {"fields": {"tenant": {}}},
                                                "custom_doc": {"fields": {"x": {}}}})
    assert captured["schemas"]["idr_claim"]["fields"] == {"tenant": {}}  # shadowed
    assert "custom_doc" in captured["schemas"]                            # extended
    # no overrides -> built-ins pass through untouched
    captured.clear()
    pl.run_pipeline({})
    assert captured["schemas"] == fake_spec["schemas"]


# --- lazy page rasterization -------------------------------------------------

_TINY_PDF = b"""%PDF-1.4
1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj
2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj
3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 612 792]/Contents 4 0 R/Resources<</Font<</F1 5 0 R>>>>>>endobj
4 0 obj<</Length 58>>stream
BT /F1 24 Tf 100 700 Td (Billed $500.00 CPT 99213) Tj ET
endstream
endobj
5 0 obj<</Type/Font/Subtype/Type1/BaseFont/Helvetica>>endobj
trailer<</Root 1 0 R>>
%%EOF"""


def test_ensure_pages_renders_pdf_lazily_and_caches():
    pypdfium2 = pytest.importorskip("pypdfium2")
    ctx = {"raw_bytes": _TINY_PDF, "content_type": "application/pdf", "pages": []}
    pages = pl.ensure_pages(ctx)
    assert len(pages) == 1
    assert ctx["pages"] is pages
    again = pl.ensure_pages(ctx)
    assert again is pages  # no second render


def test_ensure_pages_noop_for_non_pdf():
    ctx = {"raw_bytes": b"not a pdf", "content_type": "text/plain", "pages": []}
    assert pl.ensure_pages(ctx) == []


# --- OCR stage batching (paddleocr stubbed) ---------------------------------

def _stub_paddle(monkeypatch, predict_fn):
    fake = types.ModuleType("paddleocr")

    class PaddleOCR:
        def __init__(self, **kwargs):
            pass
        def predict(self, inp):
            return predict_fn(inp)

    fake.PaddleOCR = PaddleOCR
    monkeypatch.setitem(sys.modules, "paddleocr", fake)
    pl._MODEL_CACHE.clear()


def _ocr_ctx(n_pages=6):
    from PIL import Image
    page = Image.new("RGB", (400, 300), (255, 255, 255))
    return {"pages": [page.copy() for _ in range(n_pages)],
            "raw_bytes": b"\x89PNG\r\n\x1a\n...", "content_type": "image/png"}


def test_ocr_batches_pages(monkeypatch):
    calls = []

    def predict(inp):
        calls.append(inp)
        n = len(inp) if isinstance(inp, list) else 1
        return [{"rec_texts": [f"line-from-{n}-page-batch"]} for _ in range(n)]

    _stub_paddle(monkeypatch, predict)
    ctx = pl.stage_ocr(_ocr_ctx(6), {"batch_pages": 4})
    # 6 pages at batch 4 -> 2 predict calls, both list input
    assert len(calls) == 2
    assert all(isinstance(c, list) for c in calls)
    assert [len(c) for c in calls] == [4, 2]
    assert "line-from-4-page-batch" in ctx["text"]
    assert "line-from-2-page-batch" in ctx["text"]


def test_ocr_falls_back_to_serial_when_batch_rejected(monkeypatch):
    def predict(inp):
        if isinstance(inp, list) and len(inp) > 1:
            raise RuntimeError("this paddle build rejects list input")
        return [{"rec_texts": ["serial-line"]}]

    _stub_paddle(monkeypatch, predict)
    ctx = pl.stage_ocr(_ocr_ctx(3), {"batch_pages": 4})
    assert ctx["text"].count("serial-line") == 3
