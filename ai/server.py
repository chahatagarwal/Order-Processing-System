#!/usr/bin/env python3
"""
The ASSISTANT SERVICE — a RAG chatbot sidecar for the order system.

================================================================================
WHY A SEPARATE SERVICE IN A DIFFERENT LANGUAGE
================================================================================
This is the polyglot-microservice decision, and it is a real trade-off:

  WHY SPLIT IT OUT:
    * DIFFERENT RESOURCE PROFILE. An LLM/RAG workload is latency-tolerant and
      occasionally memory-hungry. The order service must stay fast and
      predictable. Separate processes mean a slow retrieval cannot consume the
      goroutines that are taking orders.
    * DIFFERENT DEPLOY CADENCE. You tune prompts and corpora weekly; you deploy
      the order service when business rules change. Coupling them means every
      prompt tweak redeploys your revenue path.
    * THE RIGHT LANGUAGE FOR THE JOB. The entire ML and NLP ecosystem is
      Python. Even though we use no libraries here, the moment you want a real
      embedding model you are in Python.
    * FAILURE ISOLATION. If the assistant crashes, orders keep flowing. It is
      explicitly a NON-CRITICAL dependency, which is why nothing in the order
      service's /readyz depends on it.

  WHAT IT COSTS, stated honestly:
    * Two toolchains, two Dockerfiles, two sets of CI.
    * A network hop, and a new failure mode between the services.
    * Duplicated cross-cutting concerns (logging format, health checks,
      request ids) that have to be kept consistent by discipline rather than by
      a shared library.
    For a system this size that is a real cost; the deciding factor is that the
    AI workload genuinely belongs in Python.

================================================================================
WHY http.server AND NOT FastAPI
================================================================================
ZERO PIP INSTALL. The whole sidecar runs on a bare Python 3 interpreter, which
means it starts in any environment, in any container, offline, with no
dependency resolution and no lockfile.

The cost is honest: we hand-write the routing, the JSON handling and the
OpenAPI description that FastAPI would generate. That is about 120 lines. In a
production team FastAPI is the right answer — but "readable with no setup" was
the explicit requirement here, and this is what satisfies it.
================================================================================
"""

from __future__ import annotations

import json
import logging
import os
import sys
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Optional
from urllib.parse import parse_qs, urlparse

# Make `rag` importable when the script is run directly from any directory.
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from rag.generator import LLMGenerator, MIN_CONFIDENCE, TemplateGenerator  # noqa: E402
from rag.index import load_corpus  # noqa: E402
from rag.intent import Intent, classify_intent  # noqa: E402
from rag.orders import ORDER_LOOKUP_TOOL_SCHEMA, OrderClient  # noqa: E402

# ------------------------------------------------------------------------------
# Configuration — same philosophy as the Go services: read the environment
# ONCE, into typed values, at startup.
# ------------------------------------------------------------------------------

SERVICE_NAME = "assistant-service"
VERSION = os.environ.get("ASSISTANT_VERSION", "dev")
HTTP_ADDR = os.environ.get("ASSISTANT_ADDR", "0.0.0.0:8090")
ORDER_SERVICE_URL = os.environ.get("ORDER_SERVICE_URL", "http://localhost:8080")
CORPUS_DIR = os.environ.get("ASSISTANT_CORPUS_DIR", os.path.join(os.path.dirname(os.path.abspath(__file__)), "corpus"))
TOP_K = int(os.environ.get("ASSISTANT_TOP_K", "3"))
LOG_LEVEL = os.environ.get("LOG_LEVEL", "info").upper()

# The LLM path is OPT-IN and OFF by default, which is what makes this service
# deterministic and free to run. See generator.py for what is given up.
LLM_ENABLED = os.environ.get("ASSISTANT_LLM", "off").lower() in ("on", "true", "1")
ANTHROPIC_API_KEY = os.environ.get("ANTHROPIC_API_KEY", "")

logging.basicConfig(
    level=getattr(logging, LOG_LEVEL, logging.INFO),
    # A format that mirrors the Go services' structured logs, so that one
    # grep across all five services still works. Consistency across a
    # polyglot system has to be maintained deliberately — nothing enforces it.
    format="time=%(asctime)s level=%(levelname)s service=" + SERVICE_NAME + " %(message)s",
    datefmt="%Y-%m-%dT%H:%M:%S",
)
log = logging.getLogger(SERVICE_NAME)


# ------------------------------------------------------------------------------
# Application state, built once at startup
# ------------------------------------------------------------------------------


class Assistant:
    """Wires the index, the intent classifier, the order tool and the generator.

    This is the composition root of the Python service, exactly as
    cmd/orderd/main.go is for the Go side.
    """

    def __init__(self) -> None:
        started = time.monotonic()

        # Build the index ONCE, at startup, not per request.
        #
        # WHY: fitting IDF and embedding every chunk takes milliseconds here,
        # but doing it per request would multiply that by your traffic for no
        # benefit — the corpus does not change between requests. "Expensive
        # setup once, cheap work per request" is the shape of every serving
        # system.
        self.index = load_corpus(CORPUS_DIR)
        self.orders = OrderClient(ORDER_SERVICE_URL)

        # STRATEGY SELECTION. Both generators satisfy the same interface, so
        # this choice is invisible to every caller below.
        if LLM_ENABLED and ANTHROPIC_API_KEY:
            self.generator = LLMGenerator(ANTHROPIC_API_KEY)
            log.info("generator=llm model=%s", self.generator.model)
        else:
            self.generator = TemplateGenerator()
            if LLM_ENABLED and not ANTHROPIC_API_KEY:
                # A specific warning, because "I set the flag and nothing
                # happened" is a frustrating way to lose twenty minutes.
                log.warning("ASSISTANT_LLM=on but ANTHROPIC_API_KEY is empty; falling back to the offline generator")
            log.info("generator=template mode=offline-deterministic")

        self.ready = False
        # The counters make the service observable without a metrics
        # dependency, and they are what /metrics exposes.
        self.lock = threading.Lock()
        self.stats = {
            "questions_total": 0,
            "answers_grounded": 0,
            "answers_refused": 0,
            "tool_calls": 0,
            "by_intent": {},
        }

        stats = self.index.stats
        log.info(
            "corpus loaded chunks=%d sources=%d vocabulary=%d duration_ms=%.1f",
            stats["chunks"],
            len(stats["sources"]),
            stats["vocabulary_size"],
            (time.monotonic() - started) * 1000,
        )

    def ask(self, question: str, request_id: str) -> dict:
        """The full RAG pipeline for one question.

        READ THIS METHOD TO UNDERSTAND THE WHOLE AI LAYER — the five steps
        below are the entire architecture.
        """
        start = time.monotonic()

        # STEP 1: CLASSIFY. Decide whether this needs live data or a document.
        # (See intent.py for why this comes first and why it is not an LLM.)
        classification = classify_intent(question)

        # STEP 2: TOOL CALL, if the question is about a specific order.
        order_facts = None
        if classification.intent == Intent.ORDER_STATUS and classification.order_refs:
            order_facts = self.orders.lookup(classification.order_refs[0])
            with self.lock:
                self.stats["tool_calls"] += 1

        # STEP 3: RETRIEVE. We do this even for order questions, so the answer
        # can combine live status with the policy that explains what it means —
        # which is what makes the answer actionable rather than merely factual.
        results = []
        if classification.intent != Intent.GREETING:
            results = self.index.search(question, top_k=TOP_K, min_score=0.0)

        # STEP 4: GENERATE, grounded in whatever we gathered.
        answer = self.generator.generate(question, classification, results, order_facts)

        # STEP 5: RECORD what happened, for observability.
        elapsed_ms = (time.monotonic() - start) * 1000
        with self.lock:
            self.stats["questions_total"] += 1
            if answer.grounded:
                self.stats["answers_grounded"] += 1
            else:
                self.stats["answers_refused"] += 1
            intent_key = classification.intent.value
            self.stats["by_intent"][intent_key] = self.stats["by_intent"].get(intent_key, 0) + 1

        log.info(
            'request_id=%s intent=%s confidence=%.3f grounded=%s tool=%s duration_ms=%.1f question="%s"',
            request_id,
            classification.intent.value,
            answer.confidence,
            answer.grounded,
            answer.used_tool,
            elapsed_ms,
            # Truncate the logged question: a chat log may contain personal
            # information, and unbounded user input in log lines is both a
            # privacy and a log-volume problem.
            question[:120].replace("\n", " "),
        )

        return {
            "answer": answer.text,
            "citations": answer.citations,
            "confidence": round(answer.confidence, 4),
            "intent": answer.intent,
            # grounded is returned so a CALLER can decide how much to trust
            # this. An ungrounded answer might be shown with a "contact
            # support" prompt attached.
            "grounded": answer.grounded,
            "used_tool": answer.used_tool,
            "generator": answer.generator,
            "matched_on": classification.matched_on,
            "order_refs": classification.order_refs,
            "duration_ms": round(elapsed_ms, 2),
            "debug": answer.debug,
        }

    def search(self, query: str, top_k: int) -> dict:
        """Raw retrieval, with no generation.

        WHY EXPOSE THIS: when an answer is wrong, the first question is always
        "was retrieval wrong, or was generation wrong?". This endpoint answers
        that in one call. A RAG system without a retrieval-inspection endpoint
        is extremely tedious to debug.
        """
        results = self.index.search(query, top_k=top_k, min_score=0.0)
        return {
            "query": query,
            "threshold": MIN_CONFIDENCE,
            "results": [
                {
                    "chunk_id": r.chunk.chunk_id,
                    "citation": r.chunk.citation,
                    "score": round(r.score, 4),
                    "above_threshold": r.score >= MIN_CONFIDENCE,
                    # Truncated: this endpoint is for diagnosis, and full chunk
                    # text would make the response unreadable.
                    "text": r.chunk.text[:300] + ("..." if len(r.chunk.text) > 300 else ""),
                }
                for r in results
            ],
        }


# ------------------------------------------------------------------------------
# HTTP transport
# ------------------------------------------------------------------------------


class Handler(BaseHTTPRequestHandler):
    """Routes requests. One class, a small dispatch table, no framework."""

    # HTTP/1.1 so keep-alive works; without this, every request pays a new TCP
    # handshake, which dominates the latency of a fast endpoint.
    protocol_version = "HTTP/1.1"
    server_version = f"{SERVICE_NAME}/{VERSION}"

    assistant: Assistant  # injected on the server instance

    # --------------------------------------------------------------
    # Helpers
    # --------------------------------------------------------------

    def _send_json(self, status: int, payload: dict, request_id: str = "") -> None:
        body = json.dumps(payload, default=str).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        # Content-Length is REQUIRED for keep-alive to work: without it the
        # client cannot tell where one response ends and the next begins, and
        # the connection has to be closed after every request.
        self.send_header("Content-Length", str(len(body)))
        if request_id:
            # Echoing the request id lets you correlate this response with the
            # order service's logs for the same user action.
            self.send_header("X-Request-Id", request_id)
        self.end_headers()
        self.wfile.write(body)

    def _error(self, status: int, code: str, message: str, request_id: str = "") -> None:
        # The SAME error envelope as the Go services, so a client writes one
        # error handler for the whole system rather than one per service.
        self._send_json(status, {"error": {"code": code, "message": message, "request_id": request_id}}, request_id)

    def _read_json(self, max_bytes: int = 64 * 1024) -> Optional[dict]:
        try:
            length = int(self.headers.get("Content-Length") or 0)
        except ValueError:
            return None
        if length <= 0:
            return None
        if length > max_bytes:
            # Bound the read. Without a cap, a client claiming a 10GB body
            # makes us allocate until the process is killed.
            return None
        try:
            raw = self.rfile.read(length)
            parsed = json.loads(raw.decode("utf-8"))
            return parsed if isinstance(parsed, dict) else None
        except (json.JSONDecodeError, UnicodeDecodeError, OSError):
            return None

    def _request_id(self) -> str:
        # Honour the caller's id so a trace spans both services; otherwise mint
        # one. Same rule as the Go middleware.
        incoming = (self.headers.get("X-Request-Id") or "").strip()
        if incoming and len(incoming) <= 128:
            return incoming
        return "req-" + uuid.uuid4().hex[:16]

    def log_message(self, fmt: str, *args) -> None:
        # Suppress BaseHTTPRequestHandler's default stderr logging; we do our
        # own structured logging in ask(). Leaving both on double-logs every
        # request in two different formats.
        return

    # --------------------------------------------------------------
    # Routes
    # --------------------------------------------------------------

    def do_GET(self) -> None:  # noqa: N802  (the stdlib dictates this name)
        parsed = urlparse(self.path)
        path = parsed.path.rstrip("/") or "/"
        request_id = self._request_id()

        # LIVENESS: no dependency checks, by design. See health.go for why
        # checking dependencies in a liveness probe causes outages.
        if path == "/healthz":
            self._send_json(200, {"status": "alive", "service": SERVICE_NAME, "version": VERSION})
            return

        # READINESS: the corpus must be loaded. We report the order service's
        # reachability but DO NOT fail on it — the assistant can still answer
        # every policy question without it, so refusing traffic would remove
        # working functionality.
        if path == "/readyz":
            index_ok = self.assistant.index.stats["chunks"] > 0
            orders_ok = self.assistant.orders.health()
            status = "ok" if index_ok else "unavailable"
            if index_ok and not orders_ok:
                status = "degraded"
            self._send_json(
                200 if index_ok else 503,
                {
                    "status": status,
                    "service": SERVICE_NAME,
                    "version": VERSION,
                    "checks": {
                        "corpus": {"status": "ok" if index_ok else "fail", "critical": True,
                                   "chunks": self.assistant.index.stats["chunks"]},
                        "order_service": {"status": "ok" if orders_ok else "fail", "critical": False,
                                          "note": "policy questions still work without it"},
                    },
                },
            )
            return

        if path == "/metrics":
            # Prometheus text format, hand-written for the same reason as the
            # Go registry: it is four lines of formatting and no dependency.
            with self.assistant.lock:
                s = dict(self.assistant.stats)
                by_intent = dict(s.pop("by_intent"))
            lines = [
                "# TYPE assistant_questions_total counter",
                f"assistant_questions_total {s['questions_total']}",
                "# TYPE assistant_answers_grounded_total counter",
                f"assistant_answers_grounded_total {s['answers_grounded']}",
                "# TYPE assistant_answers_refused_total counter",
                f"assistant_answers_refused_total {s['answers_refused']}",
                "# TYPE assistant_tool_calls_total counter",
                f"assistant_tool_calls_total {s['tool_calls']}",
                "# TYPE assistant_questions_by_intent counter",
            ]
            for intent, count in sorted(by_intent.items()):
                lines.append(f'assistant_questions_by_intent{{intent="{intent}"}} {count}')
            body = ("\n".join(lines) + "\n").encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return

        # GET /api/v1/assistant/search?q=...  — the retrieval inspector.
        if path == "/api/v1/assistant/search":
            params = parse_qs(parsed.query)
            query = (params.get("q") or [""])[0]
            if not query.strip():
                self._error(400, "missing_query", "the q parameter is required", request_id)
                return
            try:
                top_k = min(int((params.get("top_k") or [str(TOP_K)])[0]), 10)
            except ValueError:
                top_k = TOP_K
            self._send_json(200, self.assistant.search(query, top_k), request_id)
            return

        # GET /api/v1/assistant/info — self-documenting, so you can see the
        # corpus, the threshold and the tool schema without reading the code.
        if path == "/api/v1/assistant/info":
            self._send_json(
                200,
                {
                    "service": SERVICE_NAME,
                    "version": VERSION,
                    "generator": self.assistant.generator.name,
                    "llm_enabled": LLM_ENABLED and bool(ANTHROPIC_API_KEY),
                    "confidence_threshold": MIN_CONFIDENCE,
                    "top_k": TOP_K,
                    "index": self.assistant.index.stats,
                    "tools": [ORDER_LOOKUP_TOOL_SCHEMA],
                    "how_it_works": [
                        "1. classify the question's intent (order status vs policy vs action)",
                        "2. if it names an order, call the order service (a tool call)",
                        "3. retrieve the most similar policy chunks by TF-IDF cosine similarity",
                        "4. generate an answer grounded ONLY in what was retrieved or fetched",
                        "5. refuse to answer if the best match is below the confidence threshold",
                    ],
                },
                request_id,
            )
            return

        self._error(404, "not_found", f"no route for GET {path}", request_id)

    def do_POST(self) -> None:  # noqa: N802
        parsed = urlparse(self.path)
        path = parsed.path.rstrip("/") or "/"
        request_id = self._request_id()

        if path != "/api/v1/assistant/chat":
            self._error(404, "not_found", f"no route for POST {path}", request_id)
            return

        body = self._read_json()
        if body is None:
            self._error(400, "invalid_json", "the request body must be a JSON object", request_id)
            return

        question = (body.get("question") or body.get("message") or "").strip()
        if not question:
            self._error(400, "missing_question", "a non-empty 'question' field is required", request_id)
            return
        if len(question) > 2000:
            # Bound the input. A very long question is either a mistake or an
            # attempt to flood the context window.
            self._error(400, "question_too_long", "the question must be at most 2000 characters", request_id)
            return

        try:
            result = self.assistant.ask(question, request_id)
        except Exception as exc:  # noqa: BLE001
            # A catch-all so that an unexpected bug returns a clean 500 rather
            # than killing the connection with a traceback. The detail goes to
            # the log, keyed by request id; the client gets a generic message.
            log.exception("request_id=%s unhandled error in ask()", request_id)
            self._error(500, "internal_error", "the assistant could not answer that request", request_id)
            return

        result["request_id"] = request_id
        self._send_json(200, result, request_id)


def main() -> int:
    host, _, port = HTTP_ADDR.rpartition(":")
    host = host or "0.0.0.0"

    assistant = Assistant()

    # ThreadingHTTPServer, not HTTPServer.
    #
    # WHY: the plain HTTPServer is SINGLE-THREADED, so one request waiting on
    # the order service's HTTP call blocks every other request — including
    # /healthz, which would make the whole service look dead. One thread per
    # request is the simplest fix and is entirely adequate at this scale.
    server = ThreadingHTTPServer((host, int(port)), Handler)
    # daemon_threads so that in-flight request threads cannot prevent the
    # process from exiting on shutdown.
    server.daemon_threads = True
    Handler.assistant = assistant

    assistant.ready = True
    log.info("assistant service listening addr=%s:%s order_service=%s", host, port, ORDER_SERVICE_URL)
    log.info("try: curl -s -X POST http://localhost:%s/api/v1/assistant/chat -d '{\"question\":\"how long do refunds take?\"}'", port)

    try:
        server.serve_forever()
    except KeyboardInterrupt:
        log.info("shutdown signal received")
    finally:
        # Graceful shutdown: stop accepting, then close the listening socket.
        server.shutdown()
        server.server_close()
        log.info("shutdown complete")
    return 0


if __name__ == "__main__":
    sys.exit(main())
