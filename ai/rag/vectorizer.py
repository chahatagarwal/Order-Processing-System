"""
TF-IDF vectoriser and cosine similarity, written from scratch.

================================================================================
WHY WE IMPLEMENT THIS INSTEAD OF `pip install sentence-transformers`
================================================================================
Three reasons, in order of importance:

1. YOU CAN EXPLAIN IT. "We embed with a neural model" is not an explanation.
   "We weight each word by how often it appears in this document, divided by
   how common it is across all documents, then compare documents by the angle
   between their weight vectors" IS an explanation — and it is the whole of
   what follows. An interviewer can ask you anything about this file.

2. IT RUNS ANYWHERE, INSTANTLY. No model download, no 2GB of PyTorch, no GPU,
   no pip install at all. The tests run in milliseconds and work offline.

3. IT IS GENUINELY GOOD ENOUGH HERE. Our corpus is a few policy documents with
   a narrow, consistent vocabulary. TF-IDF excels at exactly that: short
   corpora where the useful signal IS the keyword overlap.

================================================================================
WHERE THIS APPROACH FAILS — the honest limitation, which matters more than the
implementation
================================================================================
TF-IDF matches WORDS, not MEANING. So:

    query:    "how do I get my money back?"
    document: "Refunds are issued within 5 working days."

share NO words, and TF-IDF scores them near zero. A real embedding model maps
both into nearby vectors because it has learned that "money back" and "refund"
mean the same thing. That is called SEMANTIC search, and it is why production
RAG uses embeddings.

WE MITIGATE IT WITH A SYNONYM MAP (see normalise below), which is the classic
pre-neural technique. It works, it is explainable, and it does not scale to
open-domain questions — which is precisely the trade-off to describe out loud
and the reason you would swap in a real embedding model as the corpus grows.

THE MIGRATION PATH IS ONE FUNCTION: everything downstream depends only on
`vectorize(text) -> dict[term, weight]` and `cosine(a, b) -> float`. Replace
those two with model calls and the retriever, the generator and the API are
untouched. That is the Dependency Inversion Principle applied to an ML
component.
================================================================================
"""

from __future__ import annotations

import math
import re
from collections import Counter
from typing import Dict, Iterable, List

# ------------------------------------------------------------------------------
# Tokenisation
# ------------------------------------------------------------------------------

# Words of 2+ characters. We keep digits because order numbers and SKUs
# ("ORD-7K2M9QX4", "SKU-LAPTOP") are exactly the terms a user searches for.
_TOKEN_RE = re.compile(r"[a-z0-9][a-z0-9\-]*")

# STOP WORDS: terms so common they carry no discriminating signal.
#
# WHY REMOVE THEM: "the" appears in every document, so its presence tells you
# nothing about which document is relevant. Worse, in a short query like
# "where is the order", the stop words would be half the vector and would dilute
# the two words that matter.
#
# (IDF already down-weights common terms automatically, so this is belt and
# braces — but on a small corpus IDF is noisy, and an explicit list is more
# predictable.)
#
# NOTE WHAT IS DELIBERATELY *ABSENT* FROM THIS LIST: the interrogatives
# "how", "what", "when", "where", "which" and "who".
#
# The obvious instinct is to strip them — they are grammatical filler. Doing so
# measurably BROKE retrieval, and the reason is worth understanding:
#
#   query:   "when am I charged?"        -> tokens: {charged}
#   heading: "When You Are Charged"      -> tokens: {charged}
#
# One shared term, and "charged" appears across several documents so its IDF is
# low. The query scored 0.09 and fell below the refusal threshold — the
# assistant said "I don't know" to a question answered by a heading that
# almost literally restates it.
#
# Keeping the interrogatives means the query and that heading share TWO terms,
# including the distinctive "when". Policy documents are full of headings like
# "When You Can Cancel" and "How Long Refunds Take", so these words carry real
# structural signal here. And we do not need to strip them for weighting
# reasons: IDF ALREADY down-weights anything that appears everywhere, which is
# exactly the job a stop-word list is a crude approximation of.
#
# This is a good illustration of why you measure a retrieval change rather than
# reasoning about it from first principles.
#
_STOP_WORDS = frozenset("""
a am an and are as at be been being but by can could did do does for from had
has have i if in into is it its may me might must my of on or our shall should
so than that the their them then there these they this to was we were will with
would you your
""".split())

# SYNONYM NORMALISATION — our mitigation for TF-IDF's vocabulary blindness.
#
# Each key is mapped to a CANONICAL term, so a query and a document that use
# different words for the same concept end up sharing a token. This is a
# hand-built, auditable, domain-specific thesaurus: cheap to extend, and you
# can explain exactly why any given match happened.
_SYNONYMS = {
    # money back
    "money": "refund", "reimburse": "refund", "reimbursement": "refund",
    "repay": "refund", "refunded": "refund", "refunds": "refund",
    # cancellation
    "cancel": "cancel", "cancelled": "cancel", "canceling": "cancel",
    "cancelling": "cancel", "cancellation": "cancel", "abort": "cancel",
    "stop": "cancel", "scrap": "cancel",
    # delivery
    "deliver": "delivery", "delivered": "delivery", "delivering": "delivery",
    "arrive": "delivery", "arrival": "delivery", "arrives": "delivery",
    "shipping": "shipment", "ship": "shipment", "shipped": "shipment",
    "dispatch": "shipment", "dispatched": "shipment", "posted": "shipment",
    # tracking / status
    "track": "tracking", "tracked": "tracking", "trace": "tracking",
    "status": "status", "state": "status", "progress": "status",
    "where": "status",  # "where is my order" -> a status question
    # returns
    "return": "return", "returns": "return", "returning": "return",
    "send-back": "return", "exchange": "return",
    # payment
    "pay": "payment", "paid": "payment", "payments": "payment",
    "charge": "payment", "charged": "payment", "card": "payment",
    "billing": "payment", "billed": "payment",
    # order
    "orders": "order", "purchase": "order", "purchased": "order",
    "bought": "order", "buy": "order",
    # problems
    "broken": "damaged", "faulty": "damaged", "defective": "damaged",
    "late": "delayed", "slow": "delayed", "overdue": "delayed",
    "missing": "lost", "lost": "lost", "disappeared": "lost",
}


def tokenize(text: str) -> List[str]:
    """Lowercase, split into words, drop stop words, apply synonyms.

    This is one of only two functions the rest of the system depends on, so it
    is the right place to put all the text normalisation.
    """
    tokens: List[str] = []
    for raw in _TOKEN_RE.findall(text.lower()):
        if raw in _STOP_WORDS:
            continue
        # Map to the canonical form, then apply light stemming for plurals.
        term = _SYNONYMS.get(raw, raw)
        term = _crude_stem(term)
        if term and term not in _STOP_WORDS:
            tokens.append(term)
    return tokens


def _crude_stem(term: str) -> str:
    """Strip common English suffixes so 'delays' and 'delay' match.

    WHY SO CRUDE: a real stemmer (Porter, Snowball) is ~300 lines of rules, and
    a lemmatiser needs a dictionary. Three suffix rules capture most of the
    benefit for our vocabulary, and — importantly — you can predict exactly
    what this does to any input. We deliberately keep short words intact, since
    stemming "was" or "ties" does more harm than good.
    """
    if len(term) <= 4:
        return term
    for suffix in ("ing", "ies", "es", "s"):
        if term.endswith(suffix) and len(term) - len(suffix) >= 3:
            if suffix == "ies":
                return term[: -len(suffix)] + "y"
            return term[: -len(suffix)]
    return term


# ------------------------------------------------------------------------------
# The TF-IDF model
# ------------------------------------------------------------------------------


class TfidfVectorizer:
    """Turns text into a sparse weight vector.

    ============================================================================
    THE FORMULA, AND WHAT EACH HALF IS FOR
    ============================================================================

        weight(term, doc) = TF(term, doc) * IDF(term)

    TF = TERM FREQUENCY — "how much does this document talk about this term?"
         We use the SUBLINEAR form, 1 + log(count), rather than the raw count.
         WHY: a document mentioning "refund" 10 times is not 10x more about
         refunds than one mentioning it once. Raw counts let one repetitive
         document dominate every query. The log compresses that.

    IDF = INVERSE DOCUMENT FREQUENCY — "how rare, and therefore how
         informative, is this term across the whole corpus?"

             IDF(t) = log((N + 1) / (df(t) + 1)) + 1

         A term in EVERY document (df = N) gets an IDF near 1 — almost no
         discriminating power. A term in ONE document gets a high IDF, so
         matching it is strong evidence.

         THE "+1"s ARE SMOOTHING: they prevent a division by zero for an unseen
         term, and the trailing +1 keeps a universal term's weight above zero
         rather than annihilating it entirely.

    This product is the whole idea: a word is important to a document if it is
    frequent HERE and rare ELSEWHERE.
    ============================================================================
    """

    def __init__(self) -> None:
        self._idf: Dict[str, float] = {}
        self._doc_count = 0
        self._fitted = False

    def fit(self, documents: Iterable[str]) -> "TfidfVectorizer":
        """Learn IDF weights from the corpus."""
        doc_frequency: Counter[str] = Counter()
        n = 0
        for doc in documents:
            n += 1
            # set(), not the full list: DOCUMENT frequency counts how many
            # documents contain the term AT ALL, not how many times in total.
            # Using the raw count here is a classic implementation bug that
            # silently ruins the weighting.
            for term in set(tokenize(doc)):
                doc_frequency[term] += 1

        self._doc_count = n
        self._idf = {
            term: math.log((n + 1) / (df + 1)) + 1.0
            for term, df in doc_frequency.items()
        }
        self._fitted = True
        return self

    def transform(self, text: str) -> Dict[str, float]:
        """Vectorise one text into {term: weight}, L2-normalised."""
        if not self._fitted:
            raise RuntimeError("TfidfVectorizer.fit() must be called before transform()")

        counts = Counter(tokenize(text))
        if not counts:
            return {}

        vector: Dict[str, float] = {}
        for term, count in counts.items():
            # An UNSEEN term (not in the corpus) gets the maximum IDF we can
            # justify. It is maximally rare by definition. Dropping unseen terms
            # instead would silently ignore the most distinctive word in a query.
            idf = self._idf.get(term, math.log(self._doc_count + 1) + 1.0)
            tf = 1.0 + math.log(count)
            vector[term] = tf * idf

        return _l2_normalise(vector)

    @property
    def vocabulary_size(self) -> int:
        return len(self._idf)


def _l2_normalise(vector: Dict[str, float]) -> Dict[str, float]:
    """Scale the vector to unit length.

    ============================================================================
    WHY NORMALISING IS ESSENTIAL (not an optimisation)
    ============================================================================
    Without it, LONG DOCUMENTS WIN EVERY QUERY. A 2,000-word document has more
    terms with larger weights, so its dot product against anything is bigger —
    regardless of relevance.

    Dividing by the vector's length removes magnitude from the comparison and
    leaves only DIRECTION, i.e. the mix of topics. A short, perfectly on-topic
    paragraph then beats a long, vaguely-related one, which is what we want.

    It also makes cosine similarity cheap: once both vectors are unit length,
    cosine == dot product, and the division in the cosine formula disappears.
    ============================================================================
    """
    norm = math.sqrt(sum(w * w for w in vector.values()))
    if norm == 0:
        return vector
    return {term: w / norm for term, w in vector.items()}


def cosine(a: Dict[str, float], b: Dict[str, float]) -> float:
    """Cosine similarity of two sparse vectors, in [0, 1] for TF-IDF.

    ============================================================================
    WHAT COSINE SIMILARITY MEASURES: the angle between two vectors.

        1.0 = identical direction (the same topic mix)
        0.0 = orthogonal (no shared vocabulary at all)

    WHY ANGLE RATHER THAN DISTANCE: Euclidean distance between two documents is
    dominated by their LENGTHS. Two paragraphs about refunds, one twice as long
    as the other, are far apart by distance but identical by angle. For text,
    the angle is what carries the meaning.

    THE IMPLEMENTATION DETAIL THAT MATTERS: we iterate over the SMALLER vector
    and look terms up in the larger. Our vectors are sparse dicts, so this is
    O(min(|a|, |b|)) rather than O(vocabulary). On a query of 5 terms against a
    10,000-term vocabulary, that is 5 lookups instead of 10,000 — the reason
    sparse representations are used at all.
    ============================================================================
    """
    if not a or not b:
        return 0.0
    if len(b) < len(a):
        a, b = b, a
    return sum(weight * b.get(term, 0.0) for term, weight in a.items())
