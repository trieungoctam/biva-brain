"""OpenTelemetry cho ai-worker.

Span của job nối tiếp trace bắt đầu ở brain-api (MCP call → enqueue) qua ``operations.trace_context``
(W3C traceparent). Exporter OTLP/HTTP chỉ bật khi có OTEL_EXPORTER_OTLP_ENDPOINT
(hoặc ..._TRACES_ENDPOINT); các biến OTEL_* chuẩn khác đều dùng được.
"""

from __future__ import annotations

import os

from opentelemetry import trace
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor


def setup(service: str, version: str) -> TracerProvider:
    provider = TracerProvider(resource=Resource.create({"service.name": service, "service.version": version}))
    if os.environ.get("OTEL_EXPORTER_OTLP_ENDPOINT") or os.environ.get("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"):
        from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter

        provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporter()))
    trace.set_tracer_provider(provider)
    return provider
