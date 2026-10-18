// Package idgen creates identifiers.
//
// WHY NOT AN AUTO-INCREMENT DATABASE ID AS THE PUBLIC ID:
//  1. It leaks business volume. /orders/4172 tells a competitor you have had
//     4172 orders. This is a real class of information disclosure.
//  2. It is guessable. A customer can try /orders/4171 and probe someone
//     else's data (an IDOR vulnerability).
//  3. It requires a database round-trip before you can know the id, which
//     makes idempotent retries and client-generated ids impossible.
//
// So: a random UUIDv4 is the primary key, and a short human-friendly
// OrderNumber is what we print on the receipt.
//
// WHY NOT github.com/google/uuid: it is 300 lines we would not read. This is
// 40 lines of standard library we can fully explain in an interview.
package idgen

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/chahat/order-processing-system/internal/platform/constants"
)

// NewUUID returns a RFC-4122 version-4 (random) UUID.
//
// It uses crypto/rand, not math/rand. math/rand is PREDICTABLE: given a few
// outputs an attacker can compute the rest of the sequence and guess future
// order ids. crypto/rand reads from the OS entropy pool.
func NewUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing means the OS entropy source is broken. There is
		// no safe fallback (a predictable id is a security hole), so we panic.
		// This is one of the few places a panic is the right answer.
		panic(fmt.Sprintf("idgen: crypto/rand unavailable: %v", err))
	}

	// Set the version and variant bits that make this a valid UUIDv4.
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant

	h := hex.EncodeToString(b)
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

// crockfordAlphabet omits I, L, O and U.
//
// WHY: a customer reads this number aloud down a phone line to support.
// "I" vs "1" and "O" vs "0" cause real mis-keying, and removing U avoids
// accidentally generating offensive words. This is Crockford's Base32 —
// a tiny detail that shows you have thought about the human using the system.
const crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewOrderNumber returns a short, human-friendly order reference such as
// "ORD-7K2M9QX4". 8 characters of 32-symbol alphabet is 32^8 ≈ 1.1e12
// combinations — ample, and we enforce a UNIQUE constraint in the database
// as the real guarantee rather than relying on probability.
func NewOrderNumber() string {
	const n = 8
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("idgen: crypto/rand unavailable: %v", err))
	}

	var sb strings.Builder
	sb.WriteString(constants.OrderNumberPrefix)
	for _, v := range b {
		// Modulo bias is negligible here (256 % 32 == 0, so it is actually
		// exactly uniform) — worth knowing that this is not always true.
		sb.WriteByte(crockfordAlphabet[int(v)%len(crockfordAlphabet)])
	}
	return sb.String()
}

// NewRequestID returns a short id for correlating log lines of one request.
// It does not need to be a full UUID — it is never persisted or exposed as a
// resource identifier, it just needs to be unique enough to grep.
func NewRequestID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "req-unknown"
	}
	return "req-" + hex.EncodeToString(b)
}
