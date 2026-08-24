package main

import (
	"math"
	"testing"
)

func TestThinkMultiplierTargetsCommandDuty(t *testing.T) {
	const (
		target    = 0.12
		timeScale = 0.05
	)
	multiplier := targetThinkMultiplier(target, timeScale)
	command := expectedCommandSecondsPerTurn * timeScale
	think := expectedThinkSeconds * multiplier
	got := command / (command + think)
	if math.Abs(got-target) > 1e-9 {
		t.Fatalf("planned command duty = %.12f, want %.12f", got, target)
	}
}
