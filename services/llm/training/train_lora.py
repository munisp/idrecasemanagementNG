#!/usr/bin/env python3
"""QLoRA fine-tune of Qwen2.5-7B-Instruct on the NSA/IDRE dataset.

Why QLoRA and not full fine-tune:
  - Full fine-tuning a 7B model needs ~60GB VRAM and, worse, OVERWRITES the
    instruction tuning you are trying to keep. LoRA adds a small adapter
    (~40MB) on top of the frozen base: the base's safety/refusal behavior
    survives, the adapter teaches domain voice and statute recall.
  - r=16, alpha=32 is the sweet spot for terminology/procedure absorption
    without memorization brittleness.

What this fine-tune does and does NOT do (be honest with stakeholders):
  DOES:  teach NSA/IDR vocabulary, statutory-clock fluency, the platform's
         grounded-or-silent voice, citation discipline ("45 CFR 149.510").
  DOES NOT: make outputs true. Truth comes from the grounding architecture
         (fact sheets, RAG passages, JSON-only extraction) — the model is
         never the source of record. Fine-tuning alone would INCREASE
         confident hallucination: it learns the SHAPE of authority.

Requires: pip install unsloth trl datasets accelerate peft bitsandbytes
GPU: 12GB+ VRAM (RTX 3060 12GB works with 4-bit loading).

Usage: python3 train_lora.py [--dataset dataset.jsonl] [--out idre-lora]
"""
import argparse

from datasets import load_dataset
from trl import SFTTrainer
from transformers import TrainingArguments
from unsloth import FastLanguageModel

PROMPT = (
    "### Instruction:\n{instruction}\n\n"
    "### Input:\n{input}\n\n"
    "### Response:\n{output}"
)


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--dataset", default="dataset.jsonl")
    ap.add_argument("--base", default="unsloth/Qwen2.5-7B-Instruct-bnb-4bit")
    ap.add_argument("--out", default="idre-lora")
    ap.add_argument("--epochs", type=float, default=2.0)
    args = ap.parse_args()

    model, tokenizer = FastLanguageModel.from_pretrained(
        model_name=args.base,
        max_seq_length=4096,
        load_in_4bit=True,
    )
    model = FastLanguageModel.get_peft_model(
        model,
        r=16,
        lora_alpha=32,
        lora_dropout=0.0,          # dropout is stochastic — keep training deterministic too
        target_modules=["q_proj", "k_proj", "v_proj", "o_proj",
                        "gate_proj", "up_proj", "down_proj"],
        use_gradient_checkpointing="unsloth",
        random_state=42,
    )

    def fmt(ex):
        text = PROMPT.format(**ex) + tokenizer.eos_token
        return {"text": text}

    ds = load_dataset("json", data_files=args.dataset, split="train").map(fmt)

    trainer = SFTTrainer(
        model=model,
        tokenizer=tokenizer,
        train_dataset=ds,
        dataset_text_field="text",
        max_seq_length=4096,
        args=TrainingArguments(
            output_dir=args.out,
            per_device_train_batch_size=2,
            gradient_accumulation_steps=8,
            num_train_epochs=args.epochs,
            learning_rate=2e-4,
            lr_scheduler_type="linear",
            warmup_steps=20,
            logging_steps=10,
            save_strategy="epoch",
            bf16=True,
            seed=42,
            data_seed=42,
            report_to=[],
        ),
    )
    trainer.train()

    # Merge adapter into base weights — the GGUF exporter takes a merged
    # model, and merging at fp16 preserves the most fidelity for later
    # quantization.
    model.save_pretrained_merged(f"{args.out}-merged", tokenizer,
                                 save_method="merged_16bit")
    print(f"merged model -> {args.out}-merged")


if __name__ == "__main__":
    main()
