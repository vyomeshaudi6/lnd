package routing

import (
	"math"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// probabilityEstimator returns node and pair probabilities based on historical
// payment results.
type probabilityEstimator struct {
	// penaltyHalfLife defines after how much time a penalized node or
	// channel is back at 50% probability.
	penaltyHalfLife time.Duration

	// aprioriHopProbability is the assumed success probability of a hop in
	// a route when no other information is available.
	aprioriHopProbability float64

	// aprioriWeight is a value in the range [0, 1] that defines to what
	// extent historical results should be extrapolated to untried
	// connections. Setting it to one will completely ignore historical
	// results and always assume the configured a priori probability for
	// untried connections. A value of zero will ignore the a priori
	// probability completely and only base the probability on historical
	// results, unless there are none available.
	aprioriWeight float64

	// prevSuccessProbability is the assumed probability for node pairs that
	// successfully relayed the previous attempt.
	prevSuccessProbability float64
}

// getNodeProbability calculates the probability for connections from a node
// that have not been tried before. The results parameter is a list of last
// payment results for that node.
func (p *probabilityEstimator) getNodeProbability(now time.Time,
	results NodeResults, amt lnwire.MilliSatoshi) float64 {

	// If the channel history is not to be taken into account, we can return
	// early here with the configured a priori probability.
	if p.aprioriWeight == 1 {
		return p.aprioriHopProbability
	}

	// If there is no channel history, our best estimate is still the a
	// priori probability.
	if len(results) == 0 {
		return p.aprioriHopProbability
	}

	// The value of the apriori weight is in the range [0, 1]. Convert it to
	// a factor that properly expresses the intention of the weight in the
	// following weight average calculation. When the apriori weight is 0,
	// the apriori factor is also 0. This means it won't have any effect on
	// the weighted average calculation below. When the apriori weight
	// approaches 1, the apriori factor goes to infinity. It will heavily
	// outweigh any observations that have been collected.
	aprioriFactor := 1/(1-p.aprioriWeight) - 1

	// Calculate a weighted average consisting of the apriori probability
	// and historical observations. This is the part that incentivizes nodes
	// to make sure that all (not just some) of their channels are in good
	// shape. Senders will steer around nodes that have shown a few
	// failures, even though there may be many channels still untried.
	//
	// If there is just a single observation and the apriori weight is 0,
	// this single observation will totally determine the node probability.
	// The node probability is returned for all other channels of the node.
	// This means that one failure will lead to the success probability
	// estimates for all other channels being 0 too. The probability for the
	// channel that was tried will not even recover, because it is
	// recovering to the node probability (which is zero). So one failure
	// effectively prunes all channels of the node forever. This is the most
	// aggressive way in which we can penalize nodes and unlikely to yield
	// good results in a real network.
	probabilitiesTotal := p.aprioriHopProbability * aprioriFactor
	totalWeight := aprioriFactor

	for _, result := range results {
		age := now.Sub(result.Timestamp)

		switch {
		// Weigh success with a constant high weight of 1. There is no
		// decay.
		case result.Success:
			totalWeight++
			probabilitiesTotal += p.prevSuccessProbability

		// Weigh failures in accordance with their age. The base
		// probability of a failure is considered zero, so nothing needs
		// to be added to probabilitiesTotal.
		case amt >= result.MinPenalizeAmt:
			totalWeight += p.getWeight(age)
		}
	}

	return probabilitiesTotal / totalWeight
}

// getWeight calculates a weight in the range [0, 1] that should be assigned to
// a payment result. Weight follows an exponential curve that starts at 1 when
// the result is fresh and asymptotically approaches zero over time. The rate at
// which this happens is controlled by the penaltyHalfLife parameter.
func (p *probabilityEstimator) getWeight(age time.Duration) float64 {
	exp := -age.Hours() / p.penaltyHalfLife.Hours()
	return math.Pow(2, exp)
}

// getPairProbability estimates the probability of successfully traversing to
// toNode based on historical payment outcomes for the from node. Those outcomes
// are passed in via the results parameter.
func (p *probabilityEstimator) getPairProbability(
	now time.Time, results NodeResults,
	toNode route.Vertex, amt lnwire.MilliSatoshi) float64 {

	// Retrieve the last pair outcome.
	lastPairResult, ok := results[toNode]

	// If there is no history for this pair, return the node probability
	// that is a probability estimate for untried channel.
	if !ok {
		return p.getNodeProbability(now, results, amt)
	}

	// For successes, we have a fixed (high) probability. Those pairs
	// will be assumed good until proven otherwise.
	if lastPairResult.Success {
		return p.prevSuccessProbability
	}

	nodeProbability := p.getNodeProbability(now, results, amt)

	// Take into account a minimum penalize amount. For balance errors, a
	// failure may be reported with such a minimum to prevent too aggressive
	// penalization. If the current amount is smaller than the amount that
	// previously triggered a failure, we act as if this is an untried
	// channel.
	if amt < lastPairResult.MinPenalizeAmt {
		return nodeProbability
	}

	timeSinceLastFailure := now.Sub(lastPairResult.Timestamp)

	// Calculate success probability based on the weight of the last
	// failure. When the failure is fresh, its weight is 1 and we'll return
	// probability 0. Over time the probability recovers to the node
	// probability. It would be as if this channel was never tried before.
	weight := p.getWeight(timeSinceLastFailure)
	probability := nodeProbability * (1 - weight)

	return probability
}
