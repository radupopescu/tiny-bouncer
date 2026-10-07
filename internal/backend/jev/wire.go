package jev

// The System One wire types are defined once in internal/backend/systemone,
// shared with every other system-one backend (the decider sends the same
// request shape to its own endpoint). Jev re-exports them here so its
// transport, its tests and its reports keep reading in the vocabulary of the
// API they speak.
//
// These are type aliases, not new types: a *systemone.Response is a
// *jev.Response, so nothing converts or copies at the package boundary.

import "tinybouncer/internal/backend/systemone"

type (
	Request  = systemone.Request
	Question = systemone.Question
	Answer   = systemone.Answer
	Response = systemone.Response
	Usage    = systemone.Usage
)
