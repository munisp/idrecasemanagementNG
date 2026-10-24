FROM python:3.12-slim
LABEL org.opencontainers.image.authors="Patrick Munis at newwave"
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY . .
# OpenMed models are staged at runtime into /models (air-gap friendly); the
# service runs regex+lexicon without them and reports mode in /healthz.
ENV OPENMED_MODEL_DIR=/models
EXPOSE 8083
CMD ["uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8083"]
