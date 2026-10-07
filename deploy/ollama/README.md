# IDRE Copilot Model Operations

The platform's only LLM is local Ollama (Qwen2.5-7B-Instruct). This directory
is the operating manual for making it deterministic, performant, and
domain-expert — and for keeping the honest line on hallucination.

## The three layers (in order of importance)

**1. Grounding architecture — this is what controls hallucination.**
No decode setting or fine-tune produces "zero hallucination"; that property
is architectural, and the platform already enforces it: every model call is
fed platform-verified facts (`gatherCopilotFacts`, briefing digests, party-safe
projections), instructed to say "not in record" when a fact is absent, kept
to one bounded temperature-0 call per turn, and never allowed to touch case
state. The model proposes; Postgres is the source of truth. Keep it that way.

**2. Deterministic decode — same input, same output.**
- `Modelfile.idre` pins temperature 0, seed 42, top_k 1, repeat_penalty 1.0.
- The Go caller sends temperature 0 + seed 42 on every call.
- `ollama.service.env` sets `OLLAMA_NUM_PARALLEL=1` — **required** for
  byte-identical replay: batched kernels reorder float reductions, so
  concurrent serving is non-deterministic on every inference stack, not just
  Ollama. The platform makes one bounded call per human action; single-stream
  serving is not a real throughput constraint.
- Pin the quantization and `OLLAMA_*` numerics flags; changing them changes
  outputs. `eval_golden.py` asserts determinism by replay — run it after any
  host/config change.

**3. Domain expertise — QLoRA fine-tune on the statute.**
`services/llm/training/` builds the dataset (45 CFR 149 pulled from the
official eCFR API + any authority texts you drop in `corpus/`), fine-tunes a
LoRA adapter on the frozen base, merges, converts to GGUF, and registers
`idre-copilot` with Ollama. The fine-tune teaches vocabulary, statutory-clock
fluency, and citation discipline. **It does not make outputs true** — a
model trained on the CFR learns the *shape* of authority and will invent
plausible citations if asked beyond it, which is exactly why the dataset
includes refusal rows and why layer 1 is non-negotiable.

## Runbook

```bash
# 0. Runtime
sudo systemctl edit ollama          # EnvironmentFile=.../ollama.service.env
sudo systemctl restart ollama

# 1. Base model, deterministic config — deployable today
ollama create idre-copilot -f deploy/ollama/Modelfile.idre
python3 services/llm/training/eval_golden.py --model idre-copilot
#   -> COPILOT_MODEL=idre-copilot on case-api when green

# 2. Domain fine-tune (GPU host, 12GB+ VRAM)
cd services/llm/training
python3 build_dataset.py                     # eCFR live + corpus/*.txt
pip install unsloth trl datasets accelerate peft bitsandbytes
python3 train_lora.py --dataset dataset.jsonl
./export_gguf.sh                             # merge -> GGUF q8_0 -> ollama create

# 3. Gate — do not promote without all three green:
#    recall (statute Q&A), refusal (bait questions refused), determinism replay
python3 eval_golden.py --model idre-copilot

# 4. Swap COPILOT_MODEL and restart case-api. Keep the f16 master GGUF.
```

## Corpus for the fine-tune (drop .txt files into `services/llm/training/corpus/`)

- 45 CFR Part 149 — fetched automatically by `build_dataset.py`
- 2026 final rule preamble extracts (Federal Register)
- CMS federal IDR process guidance and FAQs
- Your IDRE operating procedures and determination-letter exemplars
- FL AHCA Claims Dispute Resolution materials (for the FL tenant's program)

More good rows beat more rows: 500 statute-grounded, citation-exact examples
with refusals outperform 50,000 noisy web scrapes.

## NG note — one model, MANY programs (multi-sector tuning)

This platform is manifest-driven and multi-program: one Ollama host serves
NSA/IDR, FL AHCA CDR, and every future sector, and `OLLAMA_MAX_LOADED_MODELS=1`
means one blended model is the operational reality. So the NG pipeline is
SECTOR-CONDITIONED rather than NSA-specialized:

- `build_dataset.py --all` merges per-sector corpora (`corpus/<sector>/*.txt`)
  into one dataset where every instruction names its program — the model
  learns to answer inside the program's frame, not to blend authorities.
- New sectors onboard by adding a `SECTORS` entry + corpus dir — no pipeline
  changes.
- Golden sets are per-sector (`golden_sets/<sector>.jsonl`) and include
  FRAME-CONFUSION BAITS — cross-program questions the model must refuse
  (e.g. asking for 45 CFR authority inside an AHCA question). Blending is
  the signature multi-sector failure mode; the eval gate tests for it
  directly.
- Model name `idre-copilot` and decode/determinism posture are unchanged;
  per-call system prompts from case-api carry the tenant's manifest terms.

```bash
python3 build_dataset.py --all -o dataset_multi.jsonl
python3 train_lora.py --dataset dataset_multi.jsonl
./export_gguf.sh
python3 eval_golden.py            # runs every golden_sets/*.jsonl
```

## Eval discipline

Extend `golden_set.jsonl` whenever the model is caught being wrong in
production — every miss becomes a permanent regression test. The gate checks
recall + refusal + byte-identical replay; exit code is CI-friendly.
