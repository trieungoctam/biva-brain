# Build context: gốc repo (cần cả python/ và contracts/).
FROM python:3.12-slim
COPY --from=ghcr.io/astral-sh/uv:0.8 /uv /usr/local/bin/uv
WORKDIR /app
COPY python/pyproject.toml python/uv.lock ./
RUN uv sync --frozen --no-dev --no-install-project
COPY python/ ./
RUN uv sync --frozen --no-dev
COPY contracts /contracts
ENV BIVA_CONTRACTS_DIR=/contracts \
    PATH="/app/.venv/bin:$PATH"
RUN useradd -u 10001 app
USER app
CMD ["python", "-m", "biva_worker"]
