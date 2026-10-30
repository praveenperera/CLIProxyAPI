package quota

import (
	"errors"
	"sort"
)

// Candidate is a hard-eligible credential supplied by the auth manager
// Quota ranking cannot establish policy, model, executor, or cooldown eligibility
type Candidate struct {
	id       string
	priority int
	weight   float64
	view     View
}

// NewCandidate validates selection metadata after the manager applies hard eligibility
func NewCandidate(id string, priority int, weight float64, view View) (Candidate, error) {
	if !text(id, 512) || !finite(weight) || weight < 0 {
		return Candidate{}, errors.New("invalid quota candidate")
	}
	return Candidate{id, priority, weight, view}, nil
}

// ID returns the deterministic token-store identity
func (c Candidate) ID() string { return c.id }

// Admission contains either one ranked winner or a highest-tier unknown fallback pool
type Admission struct {
	winner   string
	fallback []string
}

// Winner returns the ranked winner, or empty when the configured fallback must select
func (a Admission) Winner() string { return a.winner }

// Fallback returns a copy of the eligible unknown/stale/unsupported candidate IDs
func (a Admission) Fallback() []string { return append([]string(nil), a.fallback...) }

// Rank retains the highest available priority tier and explicit positive weights
// It uses earliest reset, comparable group winners, then deterministic ID order
// An empty result means all hard-eligible candidates were excluded or denied
func Rank(candidates []Candidate) Admission {
	eligible := make([]Candidate, 0, len(candidates))
	highest := 0
	haveTier := false
	for _, c := range candidates {
		if c.id == "" || c.weight <= 0 || c.view.denied {
			continue
		}
		if !haveTier || c.priority > highest {
			highest = c.priority
			haveTier = true
		}
	}
	for _, c := range candidates {
		if c.id != "" && c.weight > 0 && !c.view.denied && c.priority == highest {
			eligible = append(eligible, c)
		}
	}
	sort.Slice(eligible, func(i, j int) bool { return eligible[i].id < eligible[j].id })
	var cohort []Candidate
	for _, c := range eligible {
		if c.view.nextReset.IsZero() {
			continue
		}
		if len(cohort) == 0 || c.view.nextReset.Before(cohort[0].view.nextReset) {
			cohort = []Candidate{c}
			continue
		}
		if c.view.nextReset.Equal(cohort[0].view.nextReset) {
			cohort = append(cohort, c)
		}
	}
	if len(cohort) == 0 {
		result := Admission{}
		for _, c := range eligible {
			result.fallback = append(result.fallback, c.id)
		}
		return result
	}
	// choose within equivalence groups first to avoid a non-transitive pairwise comparator
	winners := make([]Candidate, 0, len(cohort))
	type group struct {
		key    ComparisonKey
		amount bool
		unit   string
	}
	positions := make(map[group]int)
	for _, c := range cohort {
		remaining := c.view.allowance
		if remaining.knowledge != Known || remaining.group.value == "" {
			winners = append(winners, c)
			continue
		}
		key := group{remaining.group, remaining.amount, remaining.unit}
		pos, found := positions[key]
		if !found {
			positions[key] = len(winners)
			winners = append(winners, c)
			continue
		}
		if remaining.value > winners[pos].view.allowance.value {
			winners[pos] = c
		}
	}
	sort.Slice(winners, func(i, j int) bool { return winners[i].id < winners[j].id })
	return Admission{winner: winners[0].id}
}
