FROM python:3.12-slim
WORKDIR /app
# Paddle system deps
RUN apt-get update && apt-get install -y --no-install-recommends \
    libgl1 libglib2.0-0 tesseract-ocr && rm -rf /var/lib/apt/lists/*
COPY requirements.txt .
# Split into several layers + a CPU-only torch wheel: a single layer bundling
# docling+paddleocr+opencv+torch reliably stalls mid-push on this registry
# (confirmed repeatedly: the same blob digest 520s every retry). torch's
# default PyPI wheel also drags in ~3.5GB of nvidia-* CUDA/cuDNN/triton
# packages irrelevant to this CPU-only cluster.
RUN pip install --no-cache-dir torch --index-url https://download.pytorch.org/whl/cpu
# transformers: TrOCR handwriting ICR (microsoft/trocr-base-handwritten) for
# the check legal-amount line; model weights pull from HF Hub on first dock
# (bake into the image with a build-time warmup if the cluster is air-gapped).
RUN pip install --no-cache-dir transformers
RUN pip install --no-cache-dir paddlepaddle paddleocr pypdfium2
RUN pip install --no-cache-dir docling
RUN pip install --no-cache-dir openai confluent-kafka minio httpx "psycopg[binary]" opensearch-py pyyaml pillow
COPY . .
USER 10001
# GPU variant: use paddlepaddle-gpu and an nvidia runtime; pipeline is identical.
CMD ["python", "main.py"]
