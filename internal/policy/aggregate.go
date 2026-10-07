// Package policy implements the generic verdict aggregation applied once over
// a batch, in the dispatcher. It is pure: no I/O, no backends, no clocks.
package policy

import "tinybouncer/internal/core"

// Aggregate folds a batch of verdicts into the single effect the plugin
// applies: any deny → deny; else any ask → ask; else allow. An empty batch
// aggregates to allow with an empty reason.
func Aggregate(verdicts []core.Verdict) core.Verdict {
	effects := make([]core.Effect, len(verdicts))
	for i, v := range verdicts {
		effects[i] = v.Effect
	}
	effect := AggregateEffect(effects)

	var reason string
	for _, v := range verdicts {
		if v.Effect == effect && effect != core.Allow && v.Reason != "" {
			reason = v.Reason
			break
		}
	}

	cats := make([]string, 0, len(verdicts))
	seen := make(map[string]bool)
	for _, v := range verdicts {
		if v.Effect == effect && effect != core.Allow {
			for _, c := range v.Categories {
				if !seen[c] {
					seen[c] = true
					cats = append(cats, c)
				}
			}
		}
	}

	return core.Verdict{Effect: effect, Categories: cats, Reason: reason}
}

// AggregateEffect maps a list of effects onto the strictest one: any deny →
// deny; else any ask → ask; else allow. Empty input → allow.
func AggregateEffect(effects []core.Effect) core.Effect {
	result := core.Allow
	for _, e := range effects {
		switch e {
		case core.Deny:
			return core.Deny
		case core.Ask:
			result = core.Ask
		}
	}
	return result
}
