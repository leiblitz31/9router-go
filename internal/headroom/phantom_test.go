package headroom

import "testing"

// Mirrors upstream db81a8c9 isHeadroomPhantomSavings cases: warning fires
// only when BOTH messages and body shrink <5%.
func TestIsPhantomSavings(t *testing.T) {
	tests := []struct {
		name    string
		before  float64
		after   float64
		phantom bool
	}{
		{"healthy message compression with large tools", 1000, 500, false},
		{"barely smaller messages", 1000, 990, true},
		{"unchanged messages", 1000, 1000, true},
		{"larger messages", 1000, 1100, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsPhantomSavings(500,
				HeadroomSizes{BodyBytes: 100000, MessageBytes: tt.before},
				HeadroomSizes{BodyBytes: 99500, MessageBytes: tt.after},
				DefaultMinShrinkRatio)
			if got != tt.phantom {
				t.Errorf("IsPhantomSavings(messages %v->%v) = %v, want %v", tt.before, tt.after, got, tt.phantom)
			}
		})
	}
}

// Missing sizes must not warn: without a denominator there is no evidence.
func TestIsPhantomSavings_MissingSizesDoNotWarn(t *testing.T) {
	cases := []struct {
		name   string
		before HeadroomSizes
		after  HeadroomSizes
	}{
		{"missing message sizes", HeadroomSizes{BodyBytes: 100000}, HeadroomSizes{BodyBytes: 99500}},
		{"empty original messages", HeadroomSizes{BodyBytes: 100000, MessageBytes: 0}, HeadroomSizes{BodyBytes: 99500, MessageBytes: 10}},
		{"missing compressed size", HeadroomSizes{BodyBytes: 100000, MessageBytes: 1000}, HeadroomSizes{BodyBytes: 99500}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if IsPhantomSavings(500, tc.before, tc.after, DefaultMinShrinkRatio) {
				t.Errorf("IsPhantomSavings(%+v -> %+v) = true, want false", tc.before, tc.after)
			}
		})
	}
}

// No positive reported delta means nothing to question.
func TestIsPhantomSavings_NoDeltaNoWarning(t *testing.T) {
	d := struct {
		before, after HeadroomSizes
	}{
		HeadroomSizes{BodyBytes: 100000, MessageBytes: 1000},
		HeadroomSizes{BodyBytes: 100000, MessageBytes: 1000},
	}
	for _, saved := range []float64{0, -1} {
		if IsPhantomSavings(saved, d.before, d.after, DefaultMinShrinkRatio) {
			t.Errorf("IsPhantomSavings(saved=%v) = true, want false", saved)
		}
	}
}

// A tighter/looser threshold moves the verdict, so the ratio is honored.
func TestIsPhantomSavings_HonorsThreshold(t *testing.T) {
	before := HeadroomSizes{BodyBytes: 100000, MessageBytes: 1000}
	after := HeadroomSizes{BodyBytes: 99900, MessageBytes: 900}
	if IsPhantomSavings(100, before, after, DefaultMinShrinkRatio) {
		t.Error("10% message shrink at 5% threshold must not warn")
	}
	if !IsPhantomSavings(100, before, after, 0.2) {
		t.Error("10% message shrink at 20% threshold must warn")
	}
}

// Healthy message compression alongside a tools-dominated body is the case
// that motivated the two-sided check: the body barely moves while messages
// halve, and that must not warn.
func TestIsPhantomSavings_ToolsDominateBody(t *testing.T) {
	before := HeadroomSizes{BodyBytes: 100000, MessageBytes: 1000}
	after := HeadroomSizes{BodyBytes: 99500, MessageBytes: 500}
	if IsPhantomSavings(500, before, after, DefaultMinShrinkRatio) {
		t.Error("halved messages with a tools-dominated body must not warn")
	}
}
