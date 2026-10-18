"""
Document loading, CHUNKING, and the in-memory vector index.

================================================================================
CHUNKING IS THE MOST UNDER-APPRECIATED DECISION IN RAG
================================================================================
You cannot retrieve a whole 5,000-word document and hand it to a model: it
blows the context window, it costs money, and — most importantly — it BURIES
the one relevant sentence in 4,990 irrelevant words, which measurably degrades
the answer. So you split documents into chunks and retrieve chunks.

HOW BIG SHOULD A CHUNK BE? It is a genuine trade-off with no universal answer:

  TOO SMALL (one sentence)
    + Very precise retrieval; the matched text is all signal.
    - Loses context. "It must be returned within 30 days" — what must? The
      chunk no longer says, so the generated answer cannot either.

  TOO LARGE (a whole document)
    + Context is fully preserved.
    - Retrieval gets noisy: the chunk matches because of a word in an unrelated
      paragraph, and the real answer is diluted.

OUR CHOICE: split on MARKDOWN HEADINGS, i.e. on the document's OWN semantic
boundaries, then split any over-long section by paragraph with an OVERLAP.

WHY HEADING-BASED BEATS FIXED-SIZE: a fixed 500-character window cuts
mid-sentence and mid-table, severing a rule from its exception. An author
already grouped related sentences under a heading — that grouping is free,
human-curated semantic structure, and ignoring it in favour of a character
count throws away the best signal in the file.

WHY THE OVERLAP: if a rule spans a paragraph break, a hard split puts the
condition in chunk 1 and the consequence in chunk 2, and NEITHER chunk answers
the question. Repeating a little text at the boundary means a straddling fact
survives in at least one chunk. The cost is some duplication; the benefit is
not silently losing answers.
================================================================================
"""

from __future__ import annotations

import os
import re
from dataclasses import dataclass, field
from typing import Dict, List, Optional

from .vectorizer import TfidfVectorizer, cosine


@dataclass
class Chunk:
    """One retrievable unit of text."""

    chunk_id: str
    # source and heading together form the CITATION. Every generated answer
    # must be traceable to the text it came from — an answer you cannot verify
    # is indistinguishable from one the model invented.
    source: str
    heading: str
    text: str
    # vector is populated at index time, not query time: embedding the corpus
    # once and reusing it is the whole reason an index exists.
    vector: Dict[str, float] = field(default_factory=dict)

    @property
    def citation(self) -> str:
        if self.heading:
            return f"{self.source} § {self.heading}"
        return self.source


@dataclass
class SearchResult:
    chunk: Chunk
    score: float


# A markdown heading: one to six '#' followed by text.
_HEADING_RE = re.compile(r"^(#{1,6})\s+(.*)$")

# Chunk size bounds, in CHARACTERS.
#
# These are deliberately generous for policy prose: a typical policy section is
# 200-800 characters, so most sections become exactly one chunk and keep their
# author-intended boundaries.
MAX_CHUNK_CHARS = 1200
MIN_CHUNK_CHARS = 40  # below this, a chunk is a fragment with no useful context
# WHY 40 AND NOT 80: at 80 a terse but complete section ("Gift cards cannot
# be returned.") was discarded entirely, so the corpus silently lost
# answerable content. A floor exists to drop markdown artefacts and stray
# fragments, not real sentences — so it belongs just above fragment length.
OVERLAP_CHARS = 150


def chunk_markdown(text: str, source: str) -> List[Chunk]:
    """Split a markdown document into chunks on its heading structure."""
    lines = text.splitlines()

    sections: List[tuple[str, List[str]]] = []
    current_heading = ""
    current_lines: List[str] = []

    for line in lines:
        match = _HEADING_RE.match(line)
        if match:
            # A heading starts a new section, so flush the previous one.
            if current_lines:
                sections.append((current_heading, current_lines))
            current_heading = match.group(2).strip()
            current_lines = []
        else:
            current_lines.append(line)
    if current_lines:
        sections.append((current_heading, current_lines))

    chunks: List[Chunk] = []
    for heading, body_lines in sections:
        body = "\n".join(body_lines).strip()
        if not body:
            continue

        # Include the heading IN the chunk text.
        #
        # WHY: the heading is often the most informative phrase in the section
        # ("Cancellation Policy"), and a user's query usually echoes it. Leaving
        # it out of the indexed text discards the single best matching signal
        # in the whole document.
        full = f"{heading}\n{body}" if heading else body

        if len(full) <= MAX_CHUNK_CHARS:
            chunks.append(_make_chunk(source, heading, full, len(chunks)))
            continue

        # The section is too long: split it by paragraph, with overlap.
        for piece in _split_with_overlap(full, heading):
            chunks.append(_make_chunk(source, heading, piece, len(chunks)))

    # Drop fragments. A 20-character chunk cannot answer anything, and it adds
    # noise to every similarity comparison.
    return [c for c in chunks if len(c.text.strip()) >= MIN_CHUNK_CHARS or len(chunks) == 1]


def _split_with_overlap(text: str, heading: str) -> List[str]:
    """Split long text on paragraph boundaries, repeating a little at each seam."""
    paragraphs = [p.strip() for p in re.split(r"\n\s*\n", text) if p.strip()]
    pieces: List[str] = []
    buffer = ""

    for para in paragraphs:
        candidate = f"{buffer}\n\n{para}" if buffer else para
        if len(candidate) <= MAX_CHUNK_CHARS:
            buffer = candidate
            continue

        if buffer:
            pieces.append(buffer)
            # THE OVERLAP: carry the tail of the previous chunk into the next,
            # so a fact spanning the seam appears in both. We also repeat the
            # heading, or the continuation chunk loses the context that says
            # what it is about.
            tail = buffer[-OVERLAP_CHARS:]
            buffer = f"{heading}\n...{tail}\n\n{para}" if heading else f"...{tail}\n\n{para}"
        else:
            # A single paragraph longer than the limit. Hard-split it; there is
            # no semantic boundary left to respect.
            for i in range(0, len(para), MAX_CHUNK_CHARS - OVERLAP_CHARS):
                pieces.append(para[i : i + MAX_CHUNK_CHARS])
            buffer = ""

    if buffer:
        pieces.append(buffer)
    return pieces


def _make_chunk(source: str, heading: str, text: str, ordinal: int) -> Chunk:
    return Chunk(
        chunk_id=f"{source}#{ordinal}",
        source=source,
        heading=heading,
        text=text.strip(),
    )


class VectorIndex:
    """An in-memory vector index with brute-force cosine search.

    ============================================================================
    WHY BRUTE FORCE IS THE RIGHT CHOICE *HERE* — and when it stops being
    ============================================================================
    Search compares the query against EVERY chunk: O(n) in the number of
    chunks. With ~40 chunks that is roughly 40 sparse dot products, i.e. tens
    of microseconds. A vector database would be slower once you counted the
    network hop.

    WHEN YOU WOULD NEED A REAL VECTOR DATABASE (pgvector, FAISS, Qdrant):
    at roughly 10,000+ chunks, or when the index no longer fits in memory, or
    when you need it shared across replicas and persistent across restarts.
    Those systems use APPROXIMATE nearest-neighbour indexes (HNSW, IVF) that
    trade a small amount of recall for sub-linear search time.

    KNOWING WHICH SIDE OF THAT LINE YOU ARE ON IS THE ENGINEERING JUDGEMENT.
    Reaching for a vector database for 40 documents is the same mistake as
    brute-forcing 10 million. This is YAGNI applied to infrastructure:
    pgvector is already available because we run Postgres, so the upgrade is
    cheap WHEN it is needed — and not before.
    ============================================================================
    """

    def __init__(self) -> None:
        self.chunks: List[Chunk] = []
        self.vectorizer = TfidfVectorizer()
        self._built = False

    def add_document(self, text: str, source: str) -> int:
        """Chunk a document and add it to the index. Returns the chunk count."""
        new_chunks = chunk_markdown(text, source)
        self.chunks.extend(new_chunks)
        # Adding a document changes the corpus, which changes every IDF weight,
        # so the index must be rebuilt before the next search. Marking it stale
        # rather than silently serving old weights is what keeps results correct.
        self._built = False
        return len(new_chunks)

    def build(self) -> "VectorIndex":
        """Fit the vectoriser and embed every chunk.

        THE TWO-PHASE STRUCTURE IS REQUIRED, not stylistic: IDF depends on the
        WHOLE corpus, so you cannot compute any chunk's final weights until
        every document has been added. Embedding as you go would give the first
        document different weights from the last.
        """
        self.vectorizer.fit(c.text for c in self.chunks)
        for chunk in self.chunks:
            chunk.vector = self.vectorizer.transform(chunk.text)
        self._built = True
        return self

    def search(self, query: str, top_k: int = 3, min_score: float = 0.0) -> List[SearchResult]:
        """Return the top_k most similar chunks, best first."""
        if not self._built:
            self.build()
        if not self.chunks:
            return []

        query_vector = self.vectorizer.transform(query)
        if not query_vector:
            # Every query term was a stop word or unknown. Returning nothing is
            # correct and honest; returning arbitrary chunks would feed the
            # generator irrelevant context and invite a confident wrong answer.
            return []

        scored = [
            SearchResult(chunk=chunk, score=cosine(query_vector, chunk.vector))
            for chunk in self.chunks
        ]
        # A full sort is O(n log n); heapq.nlargest would be O(n log k) and is
        # what you would use at scale. At n=40 the difference is unmeasurable,
        # and sorted() is clearer to read.
        scored.sort(key=lambda r: r.score, reverse=True)

        # min_score is the HALLUCINATION GUARD, and it is the single most
        # important parameter in this file. Without it, an unrelated question
        # ("what is the capital of France?") still returns the three
        # least-irrelevant policy chunks with scores near 0.01 — and the
        # generator then dutifully answers from them. Filtering by score is how
        # the system earns the right to say "I don't know".
        return [r for r in scored[:top_k] if r.score > min_score]

    @property
    def stats(self) -> dict:
        return {
            "chunks": len(self.chunks),
            "sources": sorted({c.source for c in self.chunks}),
            "vocabulary_size": self.vectorizer.vocabulary_size if self._built else 0,
            "built": self._built,
        }


def load_corpus(directory: str) -> VectorIndex:
    """Load every .md file in a directory into a built index."""
    index = VectorIndex()
    if not os.path.isdir(directory):
        # An empty index, not a crash. The assistant then answers "I don't have
        # information about that", which is a correct degraded mode. Refusing
        # to start because a documentation folder is missing would take down a
        # service over a non-critical input.
        return index.build()

    # Sorted, so the chunk ids and therefore the citations are STABLE between
    # runs. Non-deterministic ids make tests flaky and make a cached answer
    # impossible to correlate with its source.
    for filename in sorted(os.listdir(directory)):
        if not filename.endswith(".md"):
            continue
        path = os.path.join(directory, filename)
        try:
            with open(path, "r", encoding="utf-8") as fh:
                index.add_document(fh.read(), source=filename)
        except OSError:
            # One unreadable file must not prevent the rest of the corpus from
            # loading. Partial knowledge beats no service.
            continue

    return index.build()
