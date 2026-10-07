// Package systemone holds the judgment shared by every system-one backend:
// the reviewed hazard battery (architecture §5bis), the route thresholds and
// the verdict mapping onto the generic core.Verdict.
//
// Backends own their transport, their configuration, their health check and
// their operating point; they share this package so that two backends can be
// compared without the policy differing between them, and so that the
// reviewed question wording exists exactly once.
package systemone

// PolicyVersion is the battery's policy version. It names the reviewed battery
// text, not an endpoint: every backend that asks these questions records the
// same policy version, so their reports and cache keys share one policy
// identity. It joins the response cache key and the report meta.
//
// It was introduced as "jev-policy-1.0" and keeps that string after the move
// into this package (architecture §5bis): renaming it would invalidate every
// committed report's policy identity and every cache entry for no behavioural
// gain.
const PolicyVersion = "jev-policy-1.0"
