FROM python:3.11-slim@sha256:9358444059ed78e2975ada2c189f1c1a3144a5dab6f35bff8c981afb38946634 AS build

WORKDIR /app

RUN apt-get update \
    && apt-get install -y --no-install-recommends git \
    && rm -rf /var/lib/apt/lists/*

RUN pip install --no-cache-dir uv

COPY pyproject.toml uv.lock* ./
RUN uv sync --frozen --no-dev --no-install-project || uv sync --no-dev --no-install-project

COPY src ./src
RUN uv sync --frozen --no-dev || uv sync --no-dev

FROM python:3.11-slim@sha256:9358444059ed78e2975ada2c189f1c1a3144a5dab6f35bff8c981afb38946634

RUN useradd --create-home --uid 1000 appuser

WORKDIR /app
RUN chown appuser:appuser /app

COPY --from=build --chown=appuser:appuser /app/.venv /app/.venv
COPY --from=build --chown=appuser:appuser /app/src /app/src
COPY --chown=appuser:appuser alembic.ini ./
COPY --chown=appuser:appuser alembic ./alembic

ENV PATH="/app/.venv/bin:$PATH"
ENV PYTHONPATH="/app/src"
ENV PYTHONUNBUFFERED=1

EXPOSE 4007

USER appuser

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD python -c "import urllib.request,sys; sys.exit(0 if urllib.request.urlopen('http://localhost:4007/health').status==200 else 1)"

CMD ["uvicorn", "orion.main:app", "--host", "0.0.0.0", "--port", "4007"]
