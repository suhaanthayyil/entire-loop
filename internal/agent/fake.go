package agent

import (
	"context"
	"sync"
)

// FakeRunner is a deterministic Runner for tests and dry runs. It returns a
// canned SeatResult per role (falling back to Default), records the seats it was
// asked to run, and never spawns a process. The loop depends on the Runner
// interface precisely so this can stand in for ExecRunner without touching
// `claude`.
type FakeRunner struct {
	// Results maps a seat role to the result to return for it.
	Results map[string]SeatResult
	// Default is returned (with Role filled in) when Results has no entry.
	Default SeatResult

	mu    sync.Mutex
	calls []SeatSpec
}

// Run records the call and returns the canned result for the seat's role.
func (f *FakeRunner) Run(_ context.Context, spec SeatSpec, _ string) SeatResult {
	f.mu.Lock()
	f.calls = append(f.calls, spec)
	f.mu.Unlock()

	if res, ok := f.Results[spec.Role]; ok {
		res.Role = spec.Role
		return res
	}
	res := f.Default
	res.Role = spec.Role
	if !res.OK && len(res.Warnings) == 0 {
		// A zero-value Default still counts as a completed (OK) empty result.
		res.OK = true
	}
	return res
}

// Calls returns a copy of the seats the runner was asked to run, in order.
func (f *FakeRunner) Calls() []SeatSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]SeatSpec, len(f.calls))
	copy(out, f.calls)
	return out
}
