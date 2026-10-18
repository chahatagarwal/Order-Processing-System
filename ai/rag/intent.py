"""
Intent classification and order-reference extraction.

================================================================================
WHY INTENT CLASSIFICATION EXISTS — the RAG vs TOOL-CALLING distinction
================================================================================
This is the single most important architectural idea in the AI layer, and the
one most often got wrong.

Consider two questions:

  (A) "What is your refund policy?"
      The answer is written down in a document. RETRIEVE the document.
      -> This is RAG.

  (B) "Where is order ORD-7K2M9QX4?"
      The answer is NOT in any document. It is a row in a database that changed
      four minutes ago. No amount of retrieval will find it.
      -> This needs a TOOL CALL: query the order API.

A system that treats everything as RAG will answer (B) by retrieving the
*shipping policy* and confidently explaining that orders usually ship within
two days — which is not what was asked, is not this customer's status, and is
indistinguishable from a correct answer to someone who does not already know.

So the FIRST decision is not "what do I retrieve?" but "WHAT KIND OF QUESTION
IS THIS?". That is what this module answers.

                        +------------------+
            question -> | classify_intent  |
                        +--------+---------+
                                 |
             +-------------------+-------------------+
             |                                       |
      ORDER_STATUS                            POLICY_QUESTION
      (a live fact)                           (a written rule)
             |                                       |
     call the order API                      retrieve from the corpus
             |                                       |
             +-------------------+-------------------+
                                 |
                      generate a grounded answer

WHY A RULE-BASED CLASSIFIER AND NOT AN LLM:
  * It is DETERMINISTIC. The same question always routes the same way, which
    means the behaviour is testable and an incident is reproducible.
  * It is INSTANT and free — no API call on the critical path.
  * It is EXPLAINABLE. When it misroutes, you can see exactly which keyword
    did it and fix that line.
The honest limitation: it is brittle to phrasings we did not anticipate. That
is why `confidence` is returned alongside the intent, and why a low-confidence
classification falls back to RAG rather than guessing at a tool call — a wrong
retrieval is recoverable, a wrong tool call acts on the wrong data.
================================================================================
"""

from __future__ import annotations

import re
from dataclasses import dataclass
from enum import Enum
from typing import List, Optional


class Intent(str, Enum):
    """What the user is actually asking for."""

    # Needs LIVE DATA from the order service.
    ORDER_STATUS = "order_status"
    # Needs a WRITTEN RULE from the corpus. This is the RAG path.
    POLICY_QUESTION = "policy_question"
    # Wants to perform an ACTION. We deliberately do NOT execute these.
    ACTION_REQUEST = "action_request"
    # Conversational filler.
    GREETING = "greeting"
    # We genuinely cannot tell.
    UNKNOWN = "unknown"


@dataclass
class Classification:
    intent: Intent
    confidence: float
    # order_refs are the identifiers we found, which the tool call needs.
    order_refs: List[str]
    # matched_on names the signal that decided it. This is for EXPLAINABILITY:
    # the API returns it, so a misrouted question can be diagnosed from the
    # response alone instead of by reading server logs.
    matched_on: str


# ------------------------------------------------------------------------------
# Order reference extraction
# ------------------------------------------------------------------------------

# Our own order numbers: ORD- followed by 8 Crockford base-32 characters.
# Anchored to a word boundary so it does not match inside a longer token.
_ORDER_NUMBER_RE = re.compile(r"\bORD-[0-9ABCDEFGHJKMNPQRSTVWXYZ]{4,12}\b", re.IGNORECASE)

# Legacy references, e.g. LEG-2019-0001. Supporting BOTH formats matters:
# during a Strangler Fig migration a customer may quote either, and an
# assistant that only understands the new format fails exactly the users with
# the oldest, most valuable order history.
_LEGACY_REF_RE = re.compile(r"\bLEG-\d{4}-\d{3,6}\b", re.IGNORECASE)

# A bare UUID, in case someone pastes an internal id.
_UUID_RE = re.compile(
    r"\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b", re.IGNORECASE
)


def extract_order_refs(text: str) -> List[str]:
    """Pull every order identifier out of free text, preserving order.

    Deduplicated but ORDER-PRESERVING: if someone asks about two orders, the
    first one mentioned is usually the subject of the question, and losing that
    ordering (by using a set) would make us answer about the wrong one.
    """
    found: List[str] = []
    for pattern in (_ORDER_NUMBER_RE, _LEGACY_REF_RE, _UUID_RE):
        for match in pattern.findall(text):
            # Uppercase our prefixed references so lookups are case-insensitive
            # for the user; leave UUIDs as they are, since they are opaque.
            value = match.upper() if "-" in match and not _UUID_RE.fullmatch(match) else match
            if value not in found:
                found.append(value)
    return found


# ------------------------------------------------------------------------------
# Intent signals
# ------------------------------------------------------------------------------

# Phrases that indicate a question about a SPECIFIC order's live state.
_STATUS_PHRASES = (
    "where is", "where's", "track", "tracking", "status of", "what is happening",
    "what's happening", "has it shipped", "has my order", "when will", "delivered yet",
    "arrived yet", "on its way", "my order", "this order", "order status",
)

# Phrases that indicate a question about POLICY — a written rule.
_POLICY_PHRASES = (
    "policy", "how long", "how do i", "how can i", "can i cancel", "am i able",
    "what happens if", "are you able", "do you accept", "do you offer",
    "how does", "what are the rules", "terms", "eligible", "entitled",
)

# Phrases where the user wants us to DO something.
_ACTION_PHRASES = (
    "cancel my order", "please cancel", "refund me", "issue a refund",
    "change my address", "update my address", "change the quantity",
    "delete my order", "reorder",
)

_GREETINGS = (
    "hello", "hi", "hey", "good morning", "good afternoon", "good evening",
    "thanks", "thank you", "cheers", "bye", "goodbye",
)

# INTERROGATIVE OPENERS — the fix for a real misclassification.
#
# THE BUG THIS SOLVES: "can I cancel my order?" contains the action phrase
# "cancel my order", so a naive keyword match reads it as a COMMAND and replies
# "I cannot make changes for you" — when the user only asked whether it is
# possible. That is an unhelpful non-answer to a perfectly ordinary question.
#
# THE DISTINCTION: these two sentences share almost every word.
#
#   "cancel my order"        -> an IMPERATIVE. The user wants us to act.
#   "can I cancel my order?" -> an INTERROGATIVE. The user wants to know a rule.
#
# English marks the difference at the START of the sentence, so checking the
# opening words is a cheap and surprisingly reliable discriminator. This is the
# kind of correction that only surfaces when you actually run the classifier
# over realistic phrasings — which is why the test suite includes both forms.
_INTERROGATIVE_OPENERS = (
    "can i", "could i", "can you tell", "could you tell", "am i", "is it",
    "is there", "how do i", "how can i", "how long", "how much", "what",
    "when", "where", "why", "which", "who", "do i", "does", "will i",
    "would i", "should i", "are there", "may i",
)


def _is_question_not_command(text: str) -> bool:
    """True if the text opens like a question rather than an instruction."""
    if text.endswith("?"):
        # An explicit question mark settles it.
        return True
    return text.startswith(_INTERROGATIVE_OPENERS)


def classify_intent(question: str) -> Classification:
    """Decide what kind of question this is.

    THE ORDER OF THE CHECKS IS THE PRIORITY ORDER, and it is deliberate.
    """
    text = question.lower().strip()
    refs = extract_order_refs(question)

    if not text:
        return Classification(Intent.UNKNOWN, 0.0, refs, "empty question")

    # ---------------------------------------------------------------
    # CHECK 1 (highest priority): an explicit order reference.
    #
    # If the user quoted an order number, they are asking about THAT ORDER.
    # This signal is strong enough to override phrasing: "what is your refund
    # policy for ORD-ABC123?" is best served by first looking up that order,
    # because the correct policy answer depends on its current status (a
    # PENDING order can be cancelled; a DELIVERED one must be returned).
    #
    # Confidence is high because an order number is unambiguous — people do not
    # type one by accident.
    # ---------------------------------------------------------------
    if refs:
        return Classification(Intent.ORDER_STATUS, 0.95, refs, f"order reference {refs[0]}")

    # ---------------------------------------------------------------
    # CHECK 2: an action request — but ONLY if it is phrased as a command.
    #
    # Checked BEFORE the status and policy phrases, because "cancel my order"
    # contains "my order" and would otherwise be misread as a status question.
    # Specific intents must be tested before general ones.
    #
    # The interrogative guard is what keeps "can I cancel my order?" out of
    # this branch and routes it to RAG, where the cancellation policy actually
    # answers it. See _is_question_not_command above.
    # ---------------------------------------------------------------
    asking = _is_question_not_command(text)
    for phrase in _ACTION_PHRASES:
        if phrase in text:
            if asking:
                return Classification(
                    Intent.POLICY_QUESTION, 0.75, refs,
                    f"question about the action '{phrase}' (interrogative phrasing)")
            return Classification(Intent.ACTION_REQUEST, 0.85, refs, f"action phrase '{phrase}'")

    # ---------------------------------------------------------------
    # CHECK 3: a status question with no reference quoted.
    #
    # We can recognise the intent but cannot answer it — so the generator will
    # ask for the order number. That is a far better outcome than retrieving
    # the shipping policy and pretending it is an answer.
    # ---------------------------------------------------------------
    for phrase in _STATUS_PHRASES:
        if phrase in text:
            return Classification(Intent.ORDER_STATUS, 0.6, refs, f"status phrase '{phrase}'")

    # CHECK 4: a policy question — the RAG path.
    for phrase in _POLICY_PHRASES:
        if phrase in text:
            return Classification(Intent.POLICY_QUESTION, 0.8, refs, f"policy phrase '{phrase}'")

    # CHECK 5: a greeting. Checked LAST among the positive signals and matched
    # only against SHORT text, because "hi, where is my order?" starts with a
    # greeting but is obviously a status question. Length is a cheap, effective
    # guard against a greeting swallowing a real question.
    if len(text) < 30:
        for greeting in _GREETINGS:
            if text.startswith(greeting):
                return Classification(Intent.GREETING, 0.9, refs, f"greeting '{greeting}'")

    # ---------------------------------------------------------------
    # DEFAULT: assume it is a policy question and let RAG try.
    #
    # WHY DEFAULT TO RAG rather than to UNKNOWN: retrieval has its OWN safety
    # net (the minimum-score threshold). If the corpus has nothing relevant,
    # the retriever returns nothing and the generator says "I don't know" —
    # so an unanticipated phrasing degrades into an honest non-answer rather
    # than a refusal to engage.
    #
    # Note the LOW confidence (0.3): it is reported in the API response, so a
    # consumer can route low-confidence answers to a human.
    # ---------------------------------------------------------------
    return Classification(Intent.POLICY_QUESTION, 0.3, refs, "default (no strong signal matched)")
