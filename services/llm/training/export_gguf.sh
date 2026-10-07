#!/usr/bin/env bash
# Export the fine-tuned model to GGUF and register it with Ollama.
#
# Prereqs:
#   git clone https://github.com/ggml-org/llama.cpp && pip install -r llama.cpp/requirements.txt
#   ollama running (ollama serve)
#
# Usage: ./export_gguf.sh [merged-model-dir] [quant]
#   quant default q8_0 — pin your choice; changing quantization changes outputs.
set -euo pipefail

MERGED="${1:-idre-lora-merged}"
QUANT="${2:-q8_0}"
GGUF_IN="qwen25-7b-idre-f16.gguf"
GGUF_OUT="qwen25-7b-idre-${QUANT}.gguf"

echo "== 1/3 convert HF -> GGUF (f16 master) =="
python3 llama.cpp/convert_hf_to_gguf.py "$MERGED" --outfile "$GGUF_IN" --outtype f16

echo "== 2/3 quantize -> $QUANT =="
./llama.cpp/build/bin/llama-quantize "$GGUF_IN" "$GGUF_OUT" "$QUANT"

echo "== 3/3 register with Ollama =="
sed "s|^FROM .*|FROM ./${GGUF_OUT}|" ../../deploy/ollama/Modelfile.idre > Modelfile.finetuned
ollama create idre-copilot -f Modelfile.finetuned

cat <<EOF

Done. Now:
  1. Evaluate BEFORE trusting it:
       python3 eval_golden.py --model idre-copilot
  2. If the golden set passes (recall + refusal + determinism replay), point
     case-api at it:  COPILOT_MODEL=idre-copilot
  3. Keep the f16 master ($GGUF_IN) — you can re-quantize from it, never
     from an already-quantized GGUF.
EOF
