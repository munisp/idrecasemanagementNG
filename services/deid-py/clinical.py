"""clinical.py — clinical entity extraction for dispute documents.

Two layers, both reported in the `detector` field of every entity:

  lexicon  — always on: a curated medical lexicon (common drugs, conditions,
             anatomy, procedures) matched case-insensitively with word
             boundaries. This is a real deterministic NER pass (the same
             technique lexicon-based Spark NLP models use), sized for the
             dispute domain, and its hits are exact spans.
  openmed  — optional transformer recall (openmed_layer.clinical_scan) for
             entities the lexicon doesn't know.

The lexicon is deliberately small and dispute-relevant; extend it in the
*_TERMS lists. It is NOT a mock — it ships and scores real spans — but
staging the OpenMed clinical model raises recall materially.
"""

from __future__ import annotations

import re

from safeharbor import Finding
import openmed_layer

# --- curated lexicons (extend as the dispute corpus teaches) ---------------

_DRUGS = [
    "acetaminophen", "ibuprofen", "naproxen", "aspirin", "metformin",
    "lisinopril", "atorvastatin", "simvastatin", "amlodipine", "metoprolol",
    "omeprazole", "pantoprazole", "losartan", "gabapentin", "prednisone",
    "amoxicillin", "azithromycin", "ciprofloxacin", "levothyroxine",
    "albuterol", "fluticasone", "insulin", "warfarin", "apixaban", "eliquis",
    "oxycodone", "hydrocodone", "tramadol", "morphine", "fentanyl",
    "propofol", "ketamine", "lidocaine", "bupivacaine", "epinephrine",
    "naloxone", "ondansetron", "ceftriaxone", "vancomycin", "piperacillin",
]

_CONDITIONS = [
    "myocardial infarction", "heart failure", "atrial fibrillation",
    "stroke", "sepsis", "pneumonia", "copd", "asthma", "diabetes",
    "hypertension", "hyperlipidemia", "chronic kidney disease", "ckd",
    "appendicitis", "cholecystitis", "pancreatitis", "diverticulitis",
    "fracture", "dislocation", "sprain", "laceration", "concussion",
    "pulmonary embolism", "deep vein thrombosis", "dvt", "cellulitis",
    "urinary tract infection", "uti", "gastroenteritis", "anemia",
    "chest pain", "abdominal pain", "back pain", "migraine", "seizure",
    "overdose", "anaphylaxis", "respiratory failure", "cardiac arrest",
]

_ANATOMY = [
    "heart", "lung", "liver", "kidney", "spleen", "pancreas", "gallbladder",
    "appendix", "colon", "stomach", "esophagus", "trachea", "aorta",
    "femur", "tibia", "fibula", "humerus", "radius", "ulna", "clavicle",
    "spine", "cervical", "thoracic", "lumbar", "skull", "ribs",
    "knee", "hip", "shoulder", "elbow", "wrist", "ankle", "hand", "foot",
    "brain", "carotid", "coronary artery", "bowel", "bladder", "uterus",
]

_PROCEDURES = [
    "appendectomy", "cholecystectomy", "hysterectomy", "mastectomy",
    "coronary artery bypass", "cabg", "angioplasty", "stent",
    "cardiac catheterization", "endoscopy", "colonoscopy", "bronchoscopy",
    "intubation", "mechanical ventilation", "cpr", "defibrillation",
    "ct scan", "mri", "ultrasound", "x-ray", "echocardiogram", "ekg", "ecg",
    "blood transfusion", "dialysis", "lumbar puncture", "thoracentesis",
    "paracentesis", "central line", "chest tube", "wound debridement",
    "suturing", "splinting", "casting", "reduction", "arthroplasty",
    "knee replacement", "hip replacement", "laminectomy", "spinal fusion",
]

_LEXICON = (
    [("DRUG", t) for t in _DRUGS]
    + [("CONDITION", t) for t in _CONDITIONS]
    + [("ANATOMY", t) for t in _ANATOMY]
    + [("PROCEDURE", t) for t in _PROCEDURES]
)

# Longest-first so "myocardial infarction" beats "infarction" if both listed.
_LEXICON.sort(key=lambda kv: -len(kv[1]))
_PATTERNS = [(typ, re.compile(r"(?i)\b" + re.escape(term) + r"\b"), term)
             for typ, term in _LEXICON]


def lexicon_scan(text: str) -> list[Finding]:
    out: list[Finding] = []
    for typ, pat, _term in _PATTERNS:
        for m in pat.finditer(text):
            out.append(Finding(typ, m.start(), m.end(), 0.85, m.group(0), "clinical-lexicon"))
    out.sort(key=lambda f: (f.start, -(f.end - f.start)))
    deduped: list[Finding] = []
    last_end = -1
    for f in out:  # keep the longest span on overlap (sort order above)
        if f.start >= last_end:
            deduped.append(f)
            last_end = f.end
    return deduped


def summarize(entities: list[Finding]) -> dict:
    counts: dict[str, int] = {}
    for e in entities:
        counts[e.type] = counts.get(e.type, 0) + 1
    return counts


def analyze(text: str) -> tuple[list[Finding], list[str]]:
    """Lexicon pass + optional OpenMed pass. Returns (entities, detectors)."""
    ents = lexicon_scan(text)
    detectors = ["clinical-lexicon/v1"]
    ner = openmed_layer.clinical_scan(text)
    if ner:
        # NER spans win on overlap (higher recall, learned boundaries).
        ner_spans = [(f.start, f.end) for f in ner]
        ents = [f for f in ents
                if not any(s < f.end and f.start < e for (s, e) in ner_spans)]
        ents.extend(ner)
        detectors.append("openmed-clinical")
    ents.sort(key=lambda f: f.start)
    return ents, detectors
