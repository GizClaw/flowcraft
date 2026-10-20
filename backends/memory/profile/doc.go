// Package profile stores and renders one conversation's bounded profile block:
// the resident lines about the conversation's participants that a context
// request serves without retrieving them.
//
// The block exists because retrieval is query-shaped and a profile is not. A
// stable preference -- what someone loves, works as, has been doing for years,
// is working toward -- answers questions that share almost none of its words,
// so a retrieval lane serves it only when the request happens to look like it.
// The block is the other half of that contract: a small, hard-bounded set of
// lines that rides along with every request, so the stable material is present
// whether or not the query resembles it.
//
// It is bounded by construction, not by hope. Every write is validated against
// the block's token and entry caps before it lands, a write that cannot fit is
// reported instead of half-applied, and what a request reads is what the caps
// allow. That is what keeps a resident block from turning into a second, worse
// retrieval pool: it cannot grow with the material it summarizes.
//
// Two writers share one block, and the split is the point. The host writes the
// explicit half -- lines it wants the model to keep reading, which no pass ever
// rewrites. The maintenance pass writes the distilled half, and it stamps it
// with the policy that judged it (the vocabulary, the caps, the dedupe
// threshold): a block whose distilled half was written by another policy is not
// served, so changing the vocabulary hides stale judgments without having to
// wait for a pass, exactly the way the maintenance overlay's stamp works. The
// pass's half is a judgment, and a judgment is served only under the policy
// that made it.
//
// Nothing here edits canonical state. The facts a distilled entry quotes stay
// where they are, with their text, their dates and their place in the read
// path; the block is a derived, bounded reading of them, and a host that wants
// a line gone either edits the block or forgets the facts behind it.
package profile
