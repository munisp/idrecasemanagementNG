FROM python:3.12-slim
LABEL org.opencontainers.image.authors="Patrick Munis at newwave"
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY . .
USER 10001
CMD ["python", "worker.py"]
