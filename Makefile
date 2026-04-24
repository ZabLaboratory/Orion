.PHONY: help install dev test lint typecheck audit format migrate up down logs

help:
	@echo "Orion — common targets"
	@echo "  install   — uv sync"
	@echo "  dev       — uvicorn with reload on :4007"
	@echo "  test      — pytest"
	@echo "  lint      — ruff check"
	@echo "  typecheck — mypy src"
	@echo "  audit     — pip-audit"
	@echo "  format    — ruff format"
	@echo "  migrate   — alembic upgrade head"
	@echo "  up/down   — docker compose up -d / down"
	@echo "  logs      — docker compose logs -f"

install:
	uv sync

dev:
	uv run uvicorn orion.main:app --reload --port 4007

test:
	uv run pytest

lint:
	uv run ruff check .

typecheck:
	uv run mypy src

audit:
	uv run pip-audit --skip-editable

format:
	uv run ruff format .

migrate:
	uv run alembic upgrade head

up:
	docker compose up -d

down:
	docker compose down

logs:
	docker compose logs -f
