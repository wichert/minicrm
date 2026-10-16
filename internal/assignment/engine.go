// Package assignment provides a pluggable chain-of-responsibility engine for resolving thread assignees.
package assignment

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// ErrNoAssigneeAvailable is returned when no resolver can determine a valid assignee.
var ErrNoAssigneeAvailable = errors.New("no active assignee available")

// EmailInput carries the email attributes passed to assignment resolvers (ready for V2 rules and ML).
type EmailInput struct {
	FromAddress       string
	FromName          string
	ToAddresses       []string
	Subject           string
	SubjectNormalized string
	BodyText          string
	PreviousThreadID  *uuid.UUID
}

// Candidate represents an assignee proposed by a Resolver.
type Candidate struct {
	AssigneeID uuid.UUID
	Source     string  // "rule", "ml", "default"
	Confidence float64 // 0.0 - 1.0
	Reason     string
}

// Resolver is implemented by each strategy in the assignment pipeline.
type Resolver interface {
	Name() string
	Resolve(ctx context.Context, input EmailInput) (*Candidate, error)
}

// Engine evaluates an ordered chain of Resolvers and returns the first high-confidence Candidate.
type Engine struct {
	resolvers     []Resolver
	minConfidence float64
}

// NewEngine constructs an assignment Engine with the given ordered resolvers.
func NewEngine(minConfidence float64, resolvers ...Resolver) *Engine {
	if minConfidence <= 0 {
		minConfidence = 0.5
	}
	return &Engine{
		resolvers:     resolvers,
		minConfidence: minConfidence,
	}
}

// Resolve evaluates each Resolver in order and returns the first matching Candidate.
func (e *Engine) Resolve(ctx context.Context, input EmailInput) (Candidate, error) {
	for _, r := range e.resolvers {
		cand, err := r.Resolve(ctx, input)
		if err != nil {
			return Candidate{}, fmt.Errorf("resolver %s: %w", r.Name(), err)
		}
		if cand != nil && cand.AssigneeID != uuid.Nil && cand.Confidence >= e.minConfidence {
			return *cand, nil
		}
	}
	return Candidate{}, ErrNoAssigneeAvailable
}
