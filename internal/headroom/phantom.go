package headroom

// HeadroomSizes carries the outbound byte sizes around a compression pass.
// bodyBytes is the whole request JSON; messageBytes is the messages array
// alone (upstream db81a8c9: large unchanged tool schemas dominate the body,
// so the body barely shrinks even when the messages compress well).
type HeadroomSizes struct {
	BodyBytes    float64
	MessageBytes float64
}

// DefaultMinShrinkRatio is the shrink below which a reported token delta is
// treated as phantom: both messages and body must shrink less than this.
const DefaultMinShrinkRatio = 0.05

// IsPhantomSavings reports whether a claimed token saving is phantom: the
// compressor reported tokens_saved but neither the messages nor the body
// actually shrank by minShrinkRatio.
//
// Both sides must fail to shrink before warning. The body check alone
// false-fires when large tool schemas dominate the outbound JSON; the message
// check alone misses savings that live in Claude's top-level system prompt
// (upstream db81a8c9 keeps the body check for exactly that case).
func IsPhantomSavings(tokensSaved float64, before, after HeadroomSizes, minShrinkRatio float64) bool {
	if tokensSaved <= 0 {
		return false
	}
	if minShrinkRatio <= 0 {
		minShrinkRatio = DefaultMinShrinkRatio
	}
	for _, sizes := range [][2]float64{{before.BodyBytes, after.BodyBytes}, {before.MessageBytes, after.MessageBytes}} {
		b, a := sizes[0], sizes[1]
		if b <= 0 || a <= 0 || a < b*(1-minShrinkRatio) {
			return false
		}
	}
	return true
}
