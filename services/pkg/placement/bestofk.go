package placement

import (
	"errors"
	"math"
	"math/rand"
)

// ErrNoNode is returned by Choose when no sampled node is eligible (all excluded, not ready, or
// draining). It is E2B's FailedToPlaceSandboxError -- the api maps it to a 503-class failure.
var ErrNoNode = errors.New("placement: no eligible node available")

// BestOfK is E2B's "power of K choices" placement: rather than argmin over the whole fleet
// (which stampedes every concurrent create onto the single emptiest node), sample K nodes at
// random and take the least-loaded of those. Faithful to
// e2b-dev/infra: packages/api/internal/orchestrator/placement/placement_best_of_K.go.
type BestOfK struct {
	// K is the number of candidates sampled per placement. <=0 makes Choose place nothing.
	K int
	// rnd picks an int in [0,n); it is rand.Intn in production and injected in tests so the
	// sampling is deterministic. Mirrors E2B's use of math/rand in sample().
	rnd func(n int) int
}

// NewBestOfK returns a BestOfK sampling k candidates (k<=0 falls back to DefaultK).
func NewBestOfK(k int) *BestOfK {
	if k <= 0 {
		k = DefaultK
	}
	return &BestOfK{K: k, rnd: rand.Intn}
}

// Choose samples up to K eligible nodes and returns the one with the lowest Load. excluded is
// a per-request set of node IDs to skip (the create handler adds a node here after its Create
// fails, then retries Choose -- E2B's excludedNodes). Returns ErrNoNode if no sampled node is
// eligible.
func (b *BestOfK) Choose(nodes []*Node, excluded map[string]struct{}) (*Node, error) {
	candidates := b.sample(nodes, excluded)
	var best *Node
	bestScore := math.MaxFloat64
	for _, n := range candidates {
		if s := n.Load(); s < bestScore {
			best, bestScore = n, s
		}
	}
	if best == nil {
		return nil, ErrNoNode
	}
	return best, nil
}

// sample returns up to K nodes drawn uniformly at random from nodes, skipping any that are
// excluded, not ready, or draining. It is a partial Fisher-Yates over an index slice (each node is drawn
// at most once); a skipped node is consumed from the pool but does not count toward K, so the
// loop keeps drawing until it has K eligible candidates or the pool is exhausted -- exactly
// E2B's sample(). rnd is the injected RNG.
func (b *BestOfK) sample(nodes []*Node, excluded map[string]struct{}) []*Node {
	if b.K <= 0 || len(nodes) == 0 {
		return nil
	}
	indices := make([]int, len(nodes))
	for i := range indices {
		indices[i] = i
	}
	out := make([]*Node, 0, b.K)
	remaining := len(indices) // active pool is indices[:remaining]
	for len(out) < b.K && remaining > 0 {
		j := b.rnd(remaining)
		pick := indices[j]
		// Remove j from the pool by swapping it to the tail and shrinking.
		indices[j], indices[remaining-1] = indices[remaining-1], indices[j]
		remaining--

		n := nodes[pick]
		if _, ex := excluded[n.ID]; ex {
			continue
		}
		// Eligibility = reachable AND not draining -- the faithful reduction of E2B's single
		// `Status() == NodeStatusReady` check (placement_best_of_K.go sample()): our ready flag is
		// its reachability half (Unhealthy/Connecting), Draining its self-reported half (Stage 25).
		if !n.Ready() || n.Draining() {
			continue
		}
		out = append(out, n)
	}
	return out
}
