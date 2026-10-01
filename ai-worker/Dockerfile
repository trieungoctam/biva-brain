# Build context: gốc repo (cần cả ai-worker/ và contracts/).
FROM python:3.12-slim
COPY --from=ghcr.io/astral-sh/uv:0.8 /uv /usr/local/bin/uv
WORKDIR /app
COPY ai-worker/pyproject.toml ai-worker/uv.lock ./
RUN uv sync --frozen --no-dev --no-install-project
COPY ai-worker/ ./
RUN uv sync --frozen --no-dev
COPY contracts /contracts
COPY kb /kb
ENV BIVA_CONTRACTS_DIR=/contracts \
    BIVA_KB_DIR=/kb \
    PATH="/app/.venv/bin:$PATH"
RUN useradd -u 10001 app
USER app
CMD ["python", "-m", "biva_worker"]
