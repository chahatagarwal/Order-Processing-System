#!/usr/bin/env python3
"""
Tests for the RAG pipeline.

Run with:  python3 -m unittest discover -s ai/tests -v
      or:  make test-ai

================================================================================
WHAT WE TEST, AND WHY THESE THINGS AND NOT OTHERS
================================================================================
The temptation with an AI component is to assert on the exact wording of the
generated answer. That produces a test suite that breaks every time you touch a
template and tells you nothing about correctness.

Instead we test the PROPERTIES that must hold:

  1. The maths is right (TF-IDF, cosine, normalisation).
  2. Chunking preserves meaning and never loses text.
  3. Relevant queries rank the RIGHT DOCUMENT first.
  4. Irrelevant queries are REFUSED rather than answered.      <-- the big one
  5. Intent routing sends questions down the right path.
  6. Every grounded answer carries a CITATION.
  7. A failing order service produces an honest answer, not a lie.

Property 4 is the one that matters most: a RAG system without a tested refusal
path is a hallucination machine that happens to work on your demo questions.
================================================================================
"""

from __future__ import annotations

import os
import sys
import unittest

# Make the `rag` package importable regardless of where the tests are run from.
_HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(_HERE))

from rag.generator import HIGH_CONFIDENCE, MIN_CONFIDENCE, TemplateGenerator  # noqa: E402
from rag.index import VectorIndex, chunk_markdown, load_corpus  # noqa: E402
from rag.intent import Intent, classify_intent, extract_order_refs  # noqa: E402
from rag.orders import OrderFacts  # noqa: E402
from rag.vectorizer import TfidfVectorizer, cosine, tokenize  # noqa: E402

CORPUS_DIR = os.path.join(os.path.dirname(_HERE), "corpus")


# =============================================================================
# 1. The vectoriser maths
# =============================================================================


class TestTokenizer(unittest.TestCase):
    def test_lowercases_and_strips_stop_words(self):
        tokens = tokenize("The refund policy IS the best")
        self.assertNotIn("the", tokens)
        self.assertNotIn("is", tokens)
        self.assertIn("refund", tokens)

    def test_keeps_order_numbers_and_skus(self):
        """Identifiers must survive tokenisation — they are what users search for."""
        tokens = tokenize("where is ORD-7K2M9QX4 and SKU-LAPTOP")
        self.assertIn("ord-7k2m9qx4", tokens)
        self.assertIn("sku-laptop", tokens)

    def test_synonyms_collapse_to_a_canonical_term(self):
        """'money back' and 'refund' must share a token, or cosine scores zero."""
        self.assertIn("refund", tokenize("I want my money back"))
        self.assertIn("refund", tokenize("please reimburse me"))
        self.assertIn("cancel", tokenize("I need to abort this"))

    def test_stemming_matches_singular_and_plural(self):
        self.assertEqual(tokenize("delays")[0], tokenize("delay")[0])

    def test_short_words_are_not_over_stemmed(self):
        """Stemming 'was' or 'ties' would corrupt more than it fixes."""
        self.assertIn("ties", tokenize("ties"))


class TestTfidf(unittest.TestCase):
    def setUp(self):
        self.docs = [
            "refunds are issued within five working days",
            "cancellation is free while the order is pending",
            "delivery takes three to five working days",
        ]
        self.vec = TfidfVectorizer().fit(self.docs)

    def test_vectors_are_unit_length(self):
        """L2 normalisation is what stops long documents winning every query."""
        v = self.vec.transform(self.docs[0])
        magnitude = sum(w * w for w in v.values()) ** 0.5
        self.assertAlmostEqual(magnitude, 1.0, places=6)

    def test_rare_terms_outweigh_common_ones(self):
        """This is the whole point of IDF.

        'working' appears in 2 of 3 documents; 'refunds' in 1. So 'refund' must
        carry more weight than 'work' in the document containing both.
        """
        v = self.vec.transform("refunds working")
        self.assertGreater(v["refund"], v["work"])

    def test_identical_text_scores_one(self):
        v = self.vec.transform(self.docs[0])
        self.assertAlmostEqual(cosine(v, v), 1.0, places=6)

    def test_unrelated_text_scores_zero(self):
        a = self.vec.transform("refunds are issued")
        b = self.vec.transform("quantum chromodynamics lattice")
        self.assertAlmostEqual(cosine(a, b), 0.0, places=6)

    def test_transform_before_fit_is_an_error(self):
        """Failing loudly beats silently returning meaningless weights."""
        with self.assertRaises(RuntimeError):
            TfidfVectorizer().transform("anything")

    def test_empty_input_is_handled(self):
        """A query of pure stop words must return {} rather than crashing."""
        self.assertEqual(self.vec.transform("the and is"), {})
        self.assertEqual(cosine({}, {"a": 1.0}), 0.0)

    def test_cosine_is_symmetric(self):
        a = self.vec.transform(self.docs[0])
        b = self.vec.transform(self.docs[2])
        self.assertAlmostEqual(cosine(a, b), cosine(b, a), places=9)

    def test_document_length_does_not_dominate(self):
        """A short on-topic text must beat a long off-topic one.

        This is the regression test for forgetting to normalise — the single
        most consequential bug you can introduce in this file.
        """
        vec = TfidfVectorizer().fit([
            "refund",
            "delivery " * 200,
        ])
        query = vec.transform("refund")
        short_on_topic = vec.transform("refund")
        long_off_topic = vec.transform("delivery " * 200)
        self.assertGreater(cosine(query, short_on_topic), cosine(query, long_off_topic))


# =============================================================================
# 2. Chunking
# =============================================================================


class TestChunking(unittest.TestCase):
    def test_splits_on_headings(self):
        doc = "# Title\n\nIntro text that is long enough to survive the minimum.\n\n## Section A\n\nBody of section A, also comfortably long enough to keep.\n\n## Section B\n\nBody of section B, likewise long enough to be retained here.\n"
        chunks = chunk_markdown(doc, "test.md")
        headings = {c.heading for c in chunks}
        self.assertIn("Section A", headings)
        self.assertIn("Section B", headings)

    def test_heading_text_is_inside_the_chunk(self):
        """The heading is usually the best matching phrase in the section."""
        doc = "## Refund Policy\n\nRefunds take five working days to be issued here.\n"
        chunks = chunk_markdown(doc, "test.md")
        self.assertIn("Refund Policy", chunks[0].text)

    def test_citation_identifies_source_and_section(self):
        doc = "## Returns\n\nYou may return items within thirty days of delivery.\n"
        chunks = chunk_markdown(doc, "returns.md")
        self.assertEqual(chunks[0].citation, "returns.md § Returns")

    def test_long_sections_are_split_with_overlap(self):
        """A rule spanning a seam must survive in at least one chunk."""
        para = "This is a sentence that carries real policy meaning. " * 12
        doc = f"## Long\n\n{para}\n\n{para}\n\n{para}\n"
        chunks = chunk_markdown(doc, "long.md")
        self.assertGreater(len(chunks), 1, "an oversized section should split")
        # Every continuation chunk must still say what section it belongs to,
        # or it loses the context that makes it interpretable.
        for chunk in chunks[1:]:
            self.assertIn("Long", chunk.text)

    def test_no_chunk_exceeds_the_limit_by_much(self):
        from rag.index import MAX_CHUNK_CHARS

        with open(os.path.join(CORPUS_DIR, "shipping.md"), encoding="utf-8") as fh:
            chunks = chunk_markdown(fh.read(), "shipping.md")
        for chunk in chunks:
            # Allowance for the heading plus the overlap prefix we prepend.
            self.assertLess(len(chunk.text), MAX_CHUNK_CHARS + OVERLAP_ALLOWANCE)

    def test_empty_document_produces_no_chunks(self):
        self.assertEqual(chunk_markdown("", "empty.md"), [])
        self.assertEqual(chunk_markdown("\n\n   \n", "blank.md"), [])


OVERLAP_ALLOWANCE = 400


# =============================================================================
# 3 & 4. Retrieval quality — ranking, and the refusal path
# =============================================================================


class TestRetrieval(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        # Build the index ONCE for the whole class: it is the expensive step,
        # and every test here reads it without mutating it.
        cls.index = load_corpus(CORPUS_DIR)

    def test_corpus_loaded(self):
        stats = self.index.stats
        self.assertGreater(stats["chunks"], 10)
        self.assertIn("refunds.md", stats["sources"])
        self.assertTrue(stats["built"])

    def test_ranks_the_right_document_first(self):
        """The core relevance assertion, over realistic phrasings.

        We assert on the SOURCE FILE, not on an exact chunk id or score. The
        file is the stable, meaningful unit; chunk ids shift whenever the
        corpus is edited, and asserting on them makes the suite fragile for no
        extra confidence.
        """
        cases = [
            ("how long do refunds take?", "refunds.md"),
            ("when can I cancel an order?", "cancellation.md"),
            ("how many days do I have to return something?", "returns.md"),
            ("what does PROCESSING mean?", "order_statuses.md"),
            ("which cards do you accept?", "payments.md"),
            ("how much is express delivery?", "shipping.md"),
        ]
        for query, expected_source in cases:
            with self.subTest(query=query):
                results = self.index.search(query, top_k=3)
                self.assertTrue(results, f"no results for {query!r}")
                sources = [r.chunk.source for r in results]
                self.assertIn(
                    expected_source, sources,
                    f"{query!r} should retrieve {expected_source}, got {sources}",
                )

    def test_synonyms_bridge_the_vocabulary_gap(self):
        """'money back' never appears in the corpus, but must find refunds.md.

        This is the test that justifies the synonym map: without it, TF-IDF
        scores this query at zero and the assistant refuses a perfectly
        ordinary question.
        """
        results = self.index.search("how do I get my money back?", top_k=3)
        self.assertTrue(results)
        self.assertEqual(results[0].chunk.source, "refunds.md")

    def test_scores_are_ordered_descending(self):
        results = self.index.search("refund policy", top_k=5)
        scores = [r.score for r in results]
        self.assertEqual(scores, sorted(scores, reverse=True))

    def test_off_topic_queries_score_below_the_threshold(self):
        """THE MOST IMPORTANT TEST IN THIS FILE.

        If an unrelated question scored above MIN_CONFIDENCE, the generator
        would answer it from whatever happened to come back — which is exactly
        the hallucination the threshold exists to prevent.
        """
        for query in [
            "what is the capital of France?",
            "write me a poem about the sea",
            "how do I configure nginx?",
            "what is the weather tomorrow?",
        ]:
            with self.subTest(query=query):
                results = self.index.search(query, top_k=3)
                best = results[0].score if results else 0.0
                self.assertLess(
                    best, MIN_CONFIDENCE,
                    f"{query!r} scored {best:.4f}, at or above the {MIN_CONFIDENCE} "
                    f"threshold — the refusal path would not fire",
                )

    def test_on_topic_queries_clear_the_threshold(self):
        """The other half of the same contract: the threshold must not be so
        strict that it refuses genuine questions. Both halves have to be
        asserted, or 'tuning' the threshold silently breaks one of them."""
        for query in [
            "how long do refunds take?",
            "can I cancel a pending order?",
            "what is the return window?",
            "when am I charged?",
        ]:
            with self.subTest(query=query):
                results = self.index.search(query, top_k=3)
                self.assertTrue(results)
                self.assertGreaterEqual(results[0].score, MIN_CONFIDENCE)

    def test_min_score_filter_is_applied(self):
        self.assertEqual(self.index.search("refund", top_k=3, min_score=0.99), [])

    def test_empty_query_returns_nothing(self):
        self.assertEqual(self.index.search("", top_k=3), [])
        self.assertEqual(self.index.search("the and of", top_k=3), [])

    def test_missing_corpus_directory_degrades_gracefully(self):
        """A missing documentation folder must not crash the service."""
        index = load_corpus("/nonexistent/path/xyz")
        self.assertEqual(index.stats["chunks"], 0)
        self.assertEqual(index.search("anything", top_k=3), [])


# =============================================================================
# 5. Intent classification
# =============================================================================


class TestOrderRefExtraction(unittest.TestCase):
    def test_extracts_new_format_order_number(self):
        self.assertEqual(extract_order_refs("where is ORD-7K2M9QX4?"), ["ORD-7K2M9QX4"])

    def test_is_case_insensitive_but_normalises(self):
        self.assertEqual(extract_order_refs("check ord-7k2m9qx4 please"), ["ORD-7K2M9QX4"])

    def test_extracts_legacy_reference(self):
        """Supporting both formats matters during a Strangler Fig migration."""
        self.assertEqual(extract_order_refs("about LEG-2019-0001"), ["LEG-2019-0001"])

    def test_extracts_uuid(self):
        uid = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
        self.assertEqual(extract_order_refs(f"order {uid}"), [uid])

    def test_preserves_mention_order(self):
        """The first order mentioned is usually the subject of the question."""
        refs = extract_order_refs("compare ORD-AAAA1111 with ORD-BBBB2222")
        self.assertEqual(refs, ["ORD-AAAA1111", "ORD-BBBB2222"])

    def test_deduplicates(self):
        refs = extract_order_refs("ORD-AAAA1111 and again ORD-AAAA1111")
        self.assertEqual(refs, ["ORD-AAAA1111"])

    def test_no_false_positives(self):
        self.assertEqual(extract_order_refs("I ordered something yesterday"), [])


class TestIntent(unittest.TestCase):
    def test_an_order_reference_routes_to_the_tool_path(self):
        c = classify_intent("where is ORD-7K2M9QX4?")
        self.assertEqual(c.intent, Intent.ORDER_STATUS)
        self.assertEqual(c.order_refs, ["ORD-7K2M9QX4"])
        self.assertGreater(c.confidence, 0.9)

    def test_status_question_without_a_reference_is_still_recognised(self):
        """We must recognise the intent so we can ASK for the order number,
        rather than retrieving the shipping policy and pretending."""
        c = classify_intent("where is my order?")
        self.assertEqual(c.intent, Intent.ORDER_STATUS)
        self.assertEqual(c.order_refs, [])

    def test_policy_questions_route_to_rag(self):
        for q in ["what is your refund policy?", "how long do returns take?", "do you offer express delivery?"]:
            with self.subTest(q=q):
                self.assertEqual(classify_intent(q).intent, Intent.POLICY_QUESTION)

    def test_imperative_is_an_action_request(self):
        c = classify_intent("cancel my order")
        self.assertEqual(c.intent, Intent.ACTION_REQUEST)

    def test_interrogative_about_an_action_is_a_policy_question(self):
        """The regression test for the misclassification found while building
        this: 'can I cancel my order?' asks about a RULE, not for an action."""
        for q in ["can I cancel my order?", "how do I cancel my order?", "am I able to cancel my order"]:
            with self.subTest(q=q):
                self.assertEqual(
                    classify_intent(q).intent, Intent.POLICY_QUESTION,
                    f"{q!r} is a question about the policy, not a command",
                )

    def test_greetings(self):
        for q in ["hello", "hi", "thanks"]:
            with self.subTest(q=q):
                self.assertEqual(classify_intent(q).intent, Intent.GREETING)

    def test_a_greeting_does_not_swallow_a_real_question(self):
        """'hi, where is my order?' is a status question that starts politely."""
        c = classify_intent("hi, where is my order ORD-7K2M9QX4?")
        self.assertEqual(c.intent, Intent.ORDER_STATUS)

    def test_empty_question_is_unknown(self):
        self.assertEqual(classify_intent("").intent, Intent.UNKNOWN)

    def test_classification_explains_itself(self):
        """matched_on makes a misroute diagnosable from the API response."""
        self.assertTrue(classify_intent("where is ORD-7K2M9QX4?").matched_on)


# =============================================================================
# 6 & 7. Answer generation
# =============================================================================


class TestGenerator(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.index = load_corpus(CORPUS_DIR)
        cls.gen = TemplateGenerator()

    def _ask(self, question, order_facts=None):
        classification = classify_intent(question)
        results = self.index.search(question, top_k=3)
        return self.gen.generate(question, classification, results, order_facts)

    def test_grounded_answers_always_carry_a_citation(self):
        """An answer you cannot verify is indistinguishable from an invented one."""
        for q in ["how long do refunds take?", "what is the return window?", "which cards do you accept?"]:
            with self.subTest(q=q):
                answer = self._ask(q)
                self.assertTrue(answer.grounded, f"{q!r} should be grounded")
                self.assertTrue(answer.citations, f"{q!r} produced no citation")

    def test_off_topic_question_is_refused_not_answered(self):
        answer = self._ask("what is the capital of France?")
        self.assertFalse(answer.grounded)
        self.assertIn("do not have information", answer.text.lower())
        self.assertEqual(answer.citations, [])
        # The debug block must explain WHY, or a wrong refusal is undiagnosable.
        self.assertEqual(answer.debug.get("reason"), "below the confidence threshold")

    def test_refusal_names_the_near_misses(self):
        answer = self._ask("how do I configure nginx?")
        self.assertIn("near_misses", answer.debug)

    def test_answer_contains_the_actual_policy_figure(self):
        """The substance test: does the answer carry the real number?"""
        answer = self._ask("how long do refunds take?")
        self.assertIn("5 working days", answer.text)

    def test_weak_match_is_hedged(self):
        """Communicating uncertainty is part of being correct."""
        answer = self._ask("how long do refunds take?")
        if answer.confidence < HIGH_CONFIDENCE:
            self.assertIn("may help", answer.text.lower())

    def test_greeting_needs_no_retrieval(self):
        answer = self._ask("hello")
        self.assertEqual(answer.intent, Intent.GREETING.value)
        self.assertFalse(answer.used_tool)

    def test_action_request_is_refused_with_an_explanation(self):
        """A chat message is not authenticated, so it must not mutate state."""
        answer = self._ask("cancel my order")
        self.assertIn("cannot make changes", answer.text.lower())
        self.assertFalse(answer.used_tool)

    def test_status_question_without_a_reference_asks_for_one(self):
        answer = self._ask("where is my order?")
        self.assertIn("order number", answer.text.lower())

    # ---------------------------------------------------------------
    # The order-tool path
    # ---------------------------------------------------------------

    def test_live_order_facts_drive_the_answer(self):
        facts = OrderFacts(
            found=True, order_ref="ORD-TEST1234", status="SHIPPED",
            status_description="Your order has left the warehouse and is on its way.",
            is_cancellable=False, is_final=False, total="99.98", currency="USD",
            item_count=2, item_names=["Wireless Headphones"],
            created_at="2026-10-01T10:00:00Z", status_changed_at="2026-10-03T08:00:00Z",
            source="new",
        )
        answer = self._ask("where is order ORD-TEST1234?", order_facts=facts)
        self.assertTrue(answer.used_tool)
        self.assertIn("SHIPPED", answer.text)
        self.assertIn("ORD-TEST1234", answer.text)
        # It must state what the customer can do next, not just a bare fact.
        self.assertIn("no longer be cancelled", answer.text.lower())

    def test_cancellable_order_says_so(self):
        facts = OrderFacts(
            found=True, order_ref="ORD-PEND0001", status="PENDING",
            status_description="Order received and awaiting processing.",
            is_cancellable=True, total="10.00", currency="USD", item_count=1,
        )
        answer = self._ask("where is order ORD-PEND0001?", order_facts=facts)
        self.assertIn("can still cancel", answer.text.lower())

    def test_missing_order_is_reported_honestly(self):
        facts = OrderFacts(found=False, order_ref="ORD-ZZZZ0000")
        answer = self._ask("where is order ORD-ZZZZ0000?", order_facts=facts)
        self.assertIn("could not find", answer.text.lower())

    def test_an_outage_is_not_reported_as_a_missing_order(self):
        """THE DISTINCTION THAT PROTECTS THE CUSTOMER.

        Telling someone their real order "does not exist" because our backend
        is down is a specifically damaging wrong answer. found=False WITH an
        error must read completely differently from found=False without one.
        """
        facts = OrderFacts(found=False, order_ref="ORD-RRRR0001", error="could not reach the order service")
        answer = self._ask("where is order ORD-RRRR0001?", order_facts=facts)
        self.assertNotIn("could not find", answer.text.lower())
        self.assertIn("not affected", answer.text.lower())

    def test_legacy_orders_are_flagged(self):
        facts = OrderFacts(
            found=True, order_ref="LEG-2019-0001", status="DELIVERED",
            status_description="Your order has been delivered.",
            is_final=True, total="1299.00", currency="USD", item_count=1,
            source="legacy",
        )
        answer = self._ask("where is order LEG-2019-0001?", order_facts=facts)
        self.assertIn("historical", answer.text.lower())

    def test_order_facts_context_never_leaks_personal_data(self):
        """Defence in depth: the projection in OrderFacts has no field for an
        email or an address, so neither can reach a prompt or a transcript."""
        facts = OrderFacts(found=True, order_ref="ORD-X", status="PENDING", total="1.00", currency="USD")
        context = facts.as_context()
        self.assertNotIn("@", context)
        for field_name in ("customer_email", "shipping_address", "version", "saga_id"):
            self.assertFalse(hasattr(facts, field_name), f"OrderFacts must not carry {field_name}")


# =============================================================================
# Index lifecycle
# =============================================================================


class TestIndexLifecycle(unittest.TestCase):
    def test_adding_a_document_invalidates_the_build(self):
        """IDF depends on the whole corpus, so a new document changes every
        weight. Serving stale weights would silently degrade every result."""
        index = VectorIndex()
        index.add_document("## A\n\nRefunds take five working days to process here.\n", "a.md")
        index.build()
        self.assertTrue(index.stats["built"])

        index.add_document("## B\n\nDeliveries take three working days to arrive.\n", "b.md")
        self.assertFalse(index.stats["built"], "adding a document must mark the index stale")

        # search() must rebuild automatically rather than return wrong scores.
        results = index.search("refunds", top_k=2)
        self.assertTrue(results)
        self.assertTrue(index.stats["built"])

    def test_chunk_ids_are_stable_across_loads(self):
        """Unstable ids make tests flaky and make a cached answer impossible to
        correlate with the text it came from."""
        first = [c.chunk_id for c in load_corpus(CORPUS_DIR).chunks]
        second = [c.chunk_id for c in load_corpus(CORPUS_DIR).chunks]
        self.assertEqual(first, second)


if __name__ == "__main__":
    unittest.main(verbosity=2)
