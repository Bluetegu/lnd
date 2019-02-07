package routing

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"

	"container/heap"

	"github.com/btcsuite/btcd/btcec"
	"github.com/coreos/bbolt"

	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
)

const (
	// HopLimit is the maximum number hops that is permissible as a route.
	// Any potential paths found that lie above this limit will be rejected
	// with an error. This value is computed using the current fixed-size
	// packet length of the Sphinx construction.
	HopLimit = 20

	// infinity is used as a starting distance in our shortest path search.
	infinity = math.MaxInt64

	// RiskFactorBillionths controls the influence of time lock delta
	// of a channel on route selection. It is expressed as billionths
	// of msat per msat sent through the channel per time lock delta
	// block. See edgeWeight function below for more details.
	// The chosen value is based on the previous incorrect weight function
	// 1 + timelock + fee * fee. In this function, the fee penalty
	// diminishes the time lock penalty for all but the smallest amounts.
	// To not change the behaviour of path finding too drastically, a
	// relatively small value is chosen which is still big enough to give
	// some effect with smaller time lock values. The value may need
	// tweaking and/or be made configurable in the future.
	RiskFactorBillionths = 15

	// noFeeLimit is the maximum value of a payment through Lightning. We
	// can use this value to signal there is no fee limit since payments
	// should never be larger than this.
	noFeeLimit = lnwire.MilliSatoshi(math.MaxUint32)
)

// HopHint is a routing hint that contains the minimum information of a channel
// required for an intermediate hop in a route to forward the payment to the
// next. This should be ideally used for private channels, since they are not
// publicly advertised to the network for routing.
type HopHint struct {
	// NodeID is the public key of the node at the start of the channel.
	NodeID *btcec.PublicKey

	// ChannelID is the unique identifier of the channel.
	ChannelID uint64

	// FeeBaseMSat is the base fee of the channel in millisatoshis.
	FeeBaseMSat uint32

	// FeeProportionalMillionths is the fee rate, in millionths of a
	// satoshi, for every satoshi sent through the channel.
	FeeProportionalMillionths uint32

	// CLTVExpiryDelta is the time-lock delta of the channel.
	CLTVExpiryDelta uint16
}

// HopPeg is a routing peg that the route must pass through. It includes a
// Node ID and an optional Channel ID that leads to it.
type HopPeg struct {
	// NodeID is the public key of the node at the start of the channel.
	NodeID *btcec.PublicKey

	// ChannelID is the unique identifier of the channel, or 0 if no
	// specific is pegged.
	ChannelID uint64
}

// Hop represents an intermediate or final node of the route. This naming
// is in line with the definition given in BOLT #4: Onion Routing Protocol.
// The struct houses the channel along which this hop can be reached and
// the values necessary to create the HTLC that needs to be sent to the
// next hop. It is also used to encode the per-hop payload included within
// the Sphinx packet.
type Hop struct {
	// PubKeyBytes is the raw bytes of the public key of the target node.
	PubKeyBytes Vertex

	// ChannelID is the unique channel ID for the channel. The first 3
	// bytes are the block height, the next 3 the index within the block,
	// and the last 2 bytes are the output index for the channel.
	ChannelID uint64

	// OutgoingTimeLock is the timelock value that should be used when
	// crafting the _outgoing_ HTLC from this hop.
	OutgoingTimeLock uint32

	// AmtToForward is the amount that this hop will forward to the next
	// hop. This value is less than the value that the incoming HTLC
	// carries as a fee will be subtracted by the hop.
	AmtToForward lnwire.MilliSatoshi
}

// edgePolicyWithSource is a helper struct to keep track of the source node
// of a channel edge. ChannelEdgePolicy only contains to destination node
// of the edge.
type edgePolicyWithSource struct {
	sourceNode *channeldb.LightningNode
	edge       *channeldb.ChannelEdgePolicy
}

// computeFee computes the fee to forward an HTLC of `amt` milli-satoshis over
// the passed active payment channel. This value is currently computed as
// specified in BOLT07, but will likely change in the near future.
func computeFee(amt lnwire.MilliSatoshi,
	edge *channeldb.ChannelEdgePolicy) lnwire.MilliSatoshi {

	return edge.FeeBaseMSat + (amt*edge.FeeProportionalMillionths)/1000000
}

func computePathFee(amt lnwire.MilliSatoshi,
	pathEdges []*channeldb.ChannelEdgePolicy) lnwire.MilliSatoshi {

	var nextIncomingAmount lnwire.MilliSatoshi
	pathLength := len(pathEdges)
	for i := pathLength - 1; i >= 0; i-- {

		// If this is the last hop, then the hop payload will contain
		// the exact amount. In BOLT #4: Onion Routing
		// Protocol / "Payload for the Last Node", this is detailed.
		amtToForward := amt

		// Fee is not part of the hop payload, but only used for
		// reporting through RPC. Set to zero for the final hop.
		fee := lnwire.MilliSatoshi(0)

		// If the current hop isn't the last hop, then add enough funds
		// to pay for transit over the next link.
		if i != len(pathEdges)-1 {
			// The amount that the current hop needs to forward is
			// equal to the incoming amount of the next hop.
			amtToForward = nextIncomingAmount

			// The fee that needs to be paid to the current hop is
			// based on the amount that this hop needs to forward
			// and its policy for the outgoing channel. This policy
			// is stored as part of the incoming channel of
			// the next hop.
			fee = computeFee(amtToForward, pathEdges[i+1])
		}

		// Finally, we update the amount that needs to flow into the
		// *next* hop, which is the amount this hop needs to forward,
		// accounting for the fee that it takes.
		nextIncomingAmount = amtToForward + fee
	}
	return nextIncomingAmount - amt
}

// isSamePath returns true if path1 and path2 travel through the exact same
// edges, and false otherwise.
func isSamePath(path1, path2 []*channeldb.ChannelEdgePolicy) bool {
	if len(path1) != len(path2) {
		return false
	}

	for i := 0; i < len(path1); i++ {
		if path1[i].ChannelID != path2[i].ChannelID {
			return false
		}
	}

	return true
}

// Route represents a path through the channel graph which runs over one or
// more channels in succession. This struct carries all the information
// required to craft the Sphinx onion packet, and send the payment along the
// first hop in the path. A route is only selected as valid if all the channels
// have sufficient capacity to carry the initial payment amount after fees are
// accounted for.
type Route struct {
	// TotalTimeLock is the cumulative (final) time lock across the entire
	// route. This is the CLTV value that should be extended to the first
	// hop in the route. All other hops will decrement the time-lock as
	// advertised, leaving enough time for all hops to wait for or present
	// the payment preimage to complete the payment.
	TotalTimeLock uint32

	// TotalFees is the sum of the fees paid at each hop within the final
	// route. In the case of a one-hop payment, this value will be zero as
	// we don't need to pay a fee to ourself.
	TotalFees lnwire.MilliSatoshi

	// TotalAmount is the total amount of funds required to complete a
	// payment over this route. This value includes the cumulative fees at
	// each hop. As a result, the HTLC extended to the first-hop in the
	// route will need to have at least this many satoshis, otherwise the
	// route will fail at an intermediate node due to an insufficient
	// amount of fees.
	TotalAmount lnwire.MilliSatoshi

	// SourcePubKey is the pubkey of the node where this route originates
	// from.
	SourcePubKey Vertex

	// Hops contains details concerning the specific forwarding details at
	// each hop.
	Hops []*Hop

	// nodeIndex is a map that allows callers to quickly look up if a node
	// is present in this computed route or not.
	nodeIndex map[Vertex]struct{}

	// chanIndex is an index that allows callers to determine if a channel
	// is present in this route or not. Channels are identified by the
	// uint64 version of the short channel ID.
	chanIndex map[uint64]struct{}
}

// HopFee returns the fee charged by the route hop indicated by hopIndex.
func (r *Route) HopFee(hopIndex int) lnwire.MilliSatoshi {
	var incomingAmt lnwire.MilliSatoshi
	if hopIndex == 0 {
		incomingAmt = r.TotalAmount
	} else {
		incomingAmt = r.Hops[hopIndex-1].AmtToForward
	}

	// Fee is calculated as difference between incoming and outgoing amount.
	return incomingAmt - r.Hops[hopIndex].AmtToForward
}

// containsNode returns true if a node is present in the target route, and
// false otherwise.
func (r *Route) containsNode(v Vertex) bool {
	_, ok := r.nodeIndex[v]
	return ok
}

// containsChannel returns true if a channel is present in the target route,
// and false otherwise. The passed chanID should be the converted uint64 form
// of lnwire.ShortChannelID.
func (r *Route) containsChannel(chanID uint64) bool {
	_, ok := r.chanIndex[chanID]
	return ok
}

// ToHopPayloads converts a complete route into the series of per-hop payloads
// that is to be encoded within each HTLC using an opaque Sphinx packet.
func (r *Route) ToHopPayloads() []sphinx.HopData {
	hopPayloads := make([]sphinx.HopData, len(r.Hops))

	// For each hop encoded within the route, we'll convert the hop struct
	// to the matching per-hop payload struct as used by the sphinx
	// package.
	for i, hop := range r.Hops {
		hopPayloads[i] = sphinx.HopData{
			// TODO(roasbeef): properly set realm, make sphinx type
			// an enum actually?
			Realm:         0,
			ForwardAmount: uint64(hop.AmtToForward),
			OutgoingCltv:  hop.OutgoingTimeLock,
		}

		// As a base case, the next hop is set to all zeroes in order
		// to indicate that the "last hop" as no further hops after it.
		nextHop := uint64(0)

		// If we aren't on the last hop, then we set the "next address"
		// field to be the channel that directly follows it.
		if i != len(r.Hops)-1 {
			nextHop = r.Hops[i+1].ChannelID
		}

		binary.BigEndian.PutUint64(hopPayloads[i].NextAddress[:],
			nextHop)
	}

	return hopPayloads
}

// newRoute returns a fully valid route between the source and target that's
// capable of supporting a payment of `amtToSend` after fees are fully
// computed. If the route is too long, or the selected path cannot support the
// fully payment including fees, then a non-nil error is returned.
//
// NOTE: The passed slice of ChannelHops MUST be sorted in forward order: from
// the source to the target node of the path finding attempt.
func newRoute(amtToSend, feeLimit lnwire.MilliSatoshi, sourceVertex Vertex,
	pathEdges []*channeldb.ChannelEdgePolicy, currentHeight uint32,
	finalCLTVDelta uint16) (*Route, error) {

	var (
		hops []*Hop

		// totalTimeLock will accumulate the cumulative time lock
		// across the entire route. This value represents how long the
		// sender will need to wait in the *worst* case.
		totalTimeLock = currentHeight

		// nextIncomingAmount is the amount that will need to flow into
		// the *next* hop. Since we're going to be walking the route
		// backwards below, this next hop gets closer and closer to the
		// sender of the payment.
		nextIncomingAmount lnwire.MilliSatoshi
	)

	pathLength := len(pathEdges)
	for i := pathLength - 1; i >= 0; i-- {
		// Now we'll start to calculate the items within the per-hop
		// payload for the hop this edge is leading to.
		edge := pathEdges[i]

		// If this is the last hop, then the hop payload will contain
		// the exact amount. In BOLT #4: Onion Routing
		// Protocol / "Payload for the Last Node", this is detailed.
		amtToForward := amtToSend

		// Fee is not part of the hop payload, but only used for
		// reporting through RPC. Set to zero for the final hop.
		fee := lnwire.MilliSatoshi(0)

		// If the current hop isn't the last hop, then add enough funds
		// to pay for transit over the next link.
		if i != len(pathEdges)-1 {
			// The amount that the current hop needs to forward is
			// equal to the incoming amount of the next hop.
			amtToForward = nextIncomingAmount

			// The fee that needs to be paid to the current hop is
			// based on the amount that this hop needs to forward
			// and its policy for the outgoing channel. This policy
			// is stored as part of the incoming channel of
			// the next hop.
			fee = computeFee(amtToForward, pathEdges[i+1])
		}

		// If this is the last hop, then for verification purposes, the
		// value of the outgoing time-lock should be _exactly_ the
		// absolute time out they'd expect in the HTLC.
		var outgoingTimeLock uint32
		if i == len(pathEdges)-1 {
			// As this is the last hop, we'll use the specified
			// final CLTV delta value instead of the value from the
			// last link in the route.
			totalTimeLock += uint32(finalCLTVDelta)

			outgoingTimeLock = currentHeight + uint32(finalCLTVDelta)
		} else {
			// Next, increment the total timelock of the entire
			// route such that each hops time lock increases as we
			// walk backwards in the route, using the delta of the
			// previous hop.
			delta := uint32(pathEdges[i+1].TimeLockDelta)
			totalTimeLock += delta

			// Otherwise, the value of the outgoing time-lock will
			// be the value of the time-lock for the _outgoing_
			// HTLC, so we factor in their specified grace period
			// (time lock delta).
			outgoingTimeLock = totalTimeLock - delta
		}

		// Since we're traversing the path backwards atm, we prepend
		// each new hop such that, the final slice of hops will be in
		// the forwards order.
		currentHop := &Hop{
			PubKeyBytes:      Vertex(edge.Node.PubKeyBytes),
			ChannelID:        edge.ChannelID,
			AmtToForward:     amtToForward,
			OutgoingTimeLock: outgoingTimeLock,
		}
		hops = append([]*Hop{currentHop}, hops...)

		// Finally, we update the amount that needs to flow into the
		// *next* hop, which is the amount this hop needs to forward,
		// accounting for the fee that it takes.
		nextIncomingAmount = amtToForward + fee
	}

	// With the base routing data expressed as hops, build the full route
	newRoute, err := NewRouteFromHops(
		nextIncomingAmount, totalTimeLock, sourceVertex, hops,
	)
	if err != nil {
		return nil, err
	}

	// Invalidate this route if its total fees exceed our fee limit.
	if newRoute.TotalFees > feeLimit {
		err := fmt.Sprintf("total route fees exceeded fee "+
			"limit of %v", feeLimit)
		return nil, newErrf(ErrFeeLimitExceeded, err)
	}

	return newRoute, nil
}

// NewRouteFromHops creates a new Route structure from the minimally required
// information to perform the payment. It infers fee amounts and populates the
// node, chan and prev/next hop maps.
func NewRouteFromHops(amtToSend lnwire.MilliSatoshi, timeLock uint32,
	sourceVertex Vertex, hops []*Hop) (*Route, error) {

	if len(hops) == 0 {
		return nil, ErrNoRouteHopsProvided
	}

	// First, we'll create a route struct and populate it with the fields
	// for which the values are provided as arguments of this function.
	// TotalFees is determined based on the difference between the amount
	// that is send from the source and the final amount that is received
	// by the destination.
	route := &Route{
		SourcePubKey:  sourceVertex,
		Hops:          hops,
		TotalTimeLock: timeLock,
		TotalAmount:   amtToSend,
		TotalFees:     amtToSend - hops[len(hops)-1].AmtToForward,
		nodeIndex:     make(map[Vertex]struct{}),
		chanIndex:     make(map[uint64]struct{}),
	}

	// Then we'll update the node and channel index, to indicate that this
	// Vertex and incoming channel link are present within this route.
	for _, hop := range hops {
		v := Vertex(hop.PubKeyBytes)
		route.nodeIndex[v] = struct{}{}
		route.chanIndex[hop.ChannelID] = struct{}{}
	}

	return route, nil
}

// Vertex is a simple alias for the serialization of a compressed Bitcoin
// public key.
type Vertex [33]byte

// NewVertex returns a new Vertex given a public key.
func NewVertex(pub *btcec.PublicKey) Vertex {
	var v Vertex
	copy(v[:], pub.SerializeCompressed())
	return v
}

// String returns a human readable version of the Vertex which is the
// hex-encoding of the serialized compressed public key.
func (v Vertex) String() string {
	return fmt.Sprintf("%x", v[:])
}

// edgeWeight computes the weight of an edge. This value is used when searching
// for the shortest path within the channel graph between two nodes. Weight is
// is the fee itself plus a time lock penalty added to it. This benefits
// channels with shorter time lock deltas and shorter (hops) routes in general.
// RiskFactor controls the influence of time lock on route selection. This is
// currently a fixed value, but might be configurable in the future.
func edgeWeight(lockedAmt lnwire.MilliSatoshi, fee lnwire.MilliSatoshi,
	timeLockDelta uint16) int64 {
	// timeLockPenalty is the penalty for the time lock delta of this channel.
	// It is controlled by RiskFactorBillionths and scales proportional
	// to the amount that will pass through channel. Rationale is that it if
	// a twice as large amount gets locked up, it is twice as bad.
	timeLockPenalty := int64(lockedAmt) * int64(timeLockDelta) *
		RiskFactorBillionths / 1000000000

	return int64(fee) + timeLockPenalty
}

// graphParams wraps the set of graph parameters passed to findPath.
type graphParams struct {
	// tx can be set to an existing db transaction. If not set, a new
	// transaction will be started.
	tx *bbolt.Tx

	// graph is the ChannelGraph to be used during path finding.
	graph *channeldb.ChannelGraph

	// additionalEdges is an optional set of edges that should be
	// considered during path finding, that is not already found in the
	// channel graph.
	additionalEdges map[Vertex][]*channeldb.ChannelEdgePolicy

	// bandwidthHints is an optional map from channels to bandwidths that
	// can be populated if the caller has a better estimate of the current
	// channel bandwidth than what is found in the graph. If set, it will
	// override the capacities and disabled flags found in the graph for
	// local channels when doing path finding. In particular, it should be
	// set to the current available sending bandwidth for active local
	// channels, and 0 for inactive channels.
	bandwidthHints map[uint64]lnwire.MilliSatoshi

	// originVertex, if not null, points to the origin of the path. The
	// distinction between the source and the origin is required when
	// calculating spur paths and when stitching together paths to
	// pass through peg nodes. It is used to infer whether disabled
	// channels should be included in the path, for fee calcuation of
	// stitched paths, etc.
	originVertex *Vertex
}

// restrictParams wraps the set of restrictions passed to findPath that the
// found path must adhere to.
type restrictParams struct {
	// ignoredNodes is an optional set of nodes that should be ignored if
	// encountered during path finding.
	ignoredNodes map[Vertex]struct{}

	// ignoredEdges is an optional set of edges that should be ignored if
	// encountered during path finding.
	ignoredEdges map[edgeLocator]struct{}

	// feeLimit is a maximum fee amount allowed to be used on the path from
	// the source to the target.
	feeLimit lnwire.MilliSatoshi

	// outgoingChannelID is the channel that needs to be taken to the first
	// hop. If nil, any channel may be used.
	outgoingChannelID *uint64

	// stopAtMaxHopsExceeded instructs the find path algorithm to
	// stop further search attempts if the number of hops of the shortest
	// path found is above the maximum. It's main purpose is to prevent
	// the search algorithm from entering an infinite recurrsion loop.
	stopAtMaxHopsExceeded bool
}

// findPath attempts to find a path from the source node within the
// ChannelGraph to the target node that's capable of supporting a payment of
// `amt` value. The current approach implemented is modified version of
// Dijkstra's algorithm to find a single shortest path between the source node
// and the destination. The distance metric used for edges is related to the
// time-lock+fee costs along a particular edge. If a path is found, this
// function returns a slice of ChannelHop structs which encoded the chosen path
// from the target to the source. The search is performed backwards from
// destination node back to source. This is to properly accumulate fees
// that need to be paid along the path and accurately check the amount
// to forward at every node against the available bandwidth.
func findPath(g *graphParams, r *restrictParams,
	sourceNode *channeldb.LightningNode, target *btcec.PublicKey,
	amt lnwire.MilliSatoshi) ([]*channeldb.ChannelEdgePolicy, error) {

	var err error
	tx := g.tx
	if tx == nil {
		tx, err = g.graph.Database().Begin(false)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
	}

	// First we'll initialize an empty heap which'll help us to quickly
	// locate the next edge we should visit next during our graph
	// traversal.
	var nodeHeap distanceHeap

	// For each node in the graph, we create an entry in the distance map
	// for the node set with a distance of "infinity". graph.ForEachNode
	// also returns the source node, so there is no need to add the source
	// node explicitly.
	distance := make(map[Vertex]nodeWithDist)
	if err := g.graph.ForEachNode(tx, func(_ *bbolt.Tx,
		node *channeldb.LightningNode) error {
		// TODO(roasbeef): with larger graph can just use disk seeks
		// with a visited map
		distance[Vertex(node.PubKeyBytes)] = nodeWithDist{
			dist: infinity,
			node: node,
		}
		return nil
	}); err != nil {
		return nil, err
	}

	additionalEdgesWithSrc := make(map[Vertex][]*edgePolicyWithSource)
	for vertex, outgoingEdgePolicies := range g.additionalEdges {
		// We'll also include all the nodes found within the additional
		// edges that are not known to us yet in the distance map.
		node := &channeldb.LightningNode{PubKeyBytes: vertex}
		distance[vertex] = nodeWithDist{
			dist: infinity,
			node: node,
		}

		// Build reverse lookup to find incoming edges. Needed because
		// search is taken place from target to source.
		for _, outgoingEdgePolicy := range outgoingEdgePolicies {
			toVertex := outgoingEdgePolicy.Node.PubKeyBytes
			incomingEdgePolicy := &edgePolicyWithSource{
				sourceNode: node,
				edge:       outgoingEdgePolicy,
			}

			additionalEdgesWithSrc[toVertex] =
				append(additionalEdgesWithSrc[toVertex],
					incomingEdgePolicy)
		}
	}

	sourceVertex := Vertex(sourceNode.PubKeyBytes)
	originVertex := sourceVertex
	if g.originVertex != nil {
		originVertex = *g.originVertex
	}

	// We can't always assume that the end destination is publicly
	// advertised to the network and included in the graph.ForEachNode call
	// above, so we'll manually include the target node. The target node
	// charges no fee. Distance is set to 0, because this is the starting
	// point of the graph traversal. We are searching backwards to get the
	// fees first time right and correctly match channel bandwidth.
	targetVertex := NewVertex(target)
	targetNode := &channeldb.LightningNode{PubKeyBytes: targetVertex}
	distance[targetVertex] = nodeWithDist{
		node:            targetNode,
		amountToReceive: amt,
		fee:             0,
	}

	// We'll use this map as a series of "next" hop pointers. So to get
	// from `Vertex` to the target node, we'll take the edge that it's
	// mapped to within `next`.
	next := make(map[Vertex]*channeldb.ChannelEdgePolicy)

	// processEdge is a helper closure that will be used to make sure edges
	// satisfy our specific requirements.
	processEdge := func(fromNode *channeldb.LightningNode,
		edge *channeldb.ChannelEdgePolicy,
		bandwidth lnwire.MilliSatoshi, toNode Vertex) {

		fromVertex := Vertex(fromNode.PubKeyBytes)

		// If this is not a local channel and it is disabled, we will
		// skip it.
		// TODO(halseth): also ignore disable flags for non-local
		// channels if bandwidth hint is set?
		isSourceChan := fromVertex == originVertex
		edgeFlags := edge.ChannelFlags
		isDisabled := edgeFlags&lnwire.ChanUpdateDisabled != 0

		if !isSourceChan && isDisabled {
			return
		}

		// If we have an outgoing channel restriction and this is not
		// the specified channel, skip it.
		if isSourceChan && r.outgoingChannelID != nil &&
			*r.outgoingChannelID != edge.ChannelID {

			return
		}

		// If this vertex or edge has been black listed, then we'll
		// skip exploring this edge.
		if _, ok := r.ignoredNodes[fromVertex]; ok {
			return
		}

		locator := newEdgeLocator(edge)
		if _, ok := r.ignoredEdges[*locator]; ok {
			return
		}

		toNodeDist := distance[toNode]

		amountToSend := toNodeDist.amountToReceive

		// If the estimated band width of the channel edge is not able
		// to carry the amount that needs to be send, return.
		if bandwidth < amountToSend {
			return
		}

		// If the amountToSend is less than the minimum required
		// amount, return.
		if amountToSend < edge.MinHTLC {
			return
		}

		// Compute fee that fromNode is charging. It is based on the
		// amount that needs to be sent to the next node in the route.
		//
		// Source node has no predecessor to pay a fee. Therefore set
		// fee to zero, because it should not be included in the fee
		// limit check and edge weight.
		//
		// Also determine the time lock delta that will be added to the
		// route if fromNode is selected. If fromNode is the source
		// node, no additional timelock is required.
		var fee lnwire.MilliSatoshi
		var timeLockDelta uint16
		if fromVertex != originVertex {
			fee = computeFee(amountToSend, edge)
			timeLockDelta = edge.TimeLockDelta
		}

		// amountToReceive is the amount that the node that is added to
		// the distance map needs to receive from a (to be found)
		// previous node in the route. That previous node will need to
		// pay the amount that this node forwards plus the fee it
		// charges.
		amountToReceive := amountToSend + fee

		// Check if accumulated fees would exceed fee limit when this
		// node would be added to the path.
		totalFee := amountToReceive - amt
		if totalFee > r.feeLimit {
			return
		}

		// By adding fromNode in the route, there will be an extra
		// weight composed of the fee that this node will charge and
		// the amount that will be locked for timeLockDelta blocks in
		// the HTLC that is handed out to fromNode.
		weight := edgeWeight(amountToReceive, fee, timeLockDelta)

		// Compute the tentative distance to this new channel/edge
		// which is the distance from our toNode to the target node
		// plus the weight of this edge.
		tempDist := toNodeDist.dist + weight

		// If this new tentative distance is not better than the current
		// best known distance to this node, return.
		if tempDist >= distance[fromVertex].dist {
			return
		}

		// If the edge has no time lock delta, the payment will always
		// fail, so return.
		//
		// TODO(joostjager): Is this really true? Can't it be that
		// nodes take this risk in exchange for a extraordinary high
		// fee?
		if edge.TimeLockDelta == 0 {
			return
		}

		// All conditions are met and this new tentative distance is
		// better than the current best known distance to this node.
		// The new better distance is recorded, and also our "next hop"
		// map is populated with this edge.
		distance[fromVertex] = nodeWithDist{
			dist:            tempDist,
			node:            fromNode,
			amountToReceive: amountToReceive,
			fee:             fee,
		}

		next[fromVertex] = edge

		// Add this new node to our heap as we'd like to further
		// explore backwards through this edge.
		heap.Push(&nodeHeap, distance[fromVertex])
	}

	// TODO(roasbeef): also add path caching
	//  * similar to route caching, but doesn't factor in the amount

	// To start, our target node will the sole item within our distance
	// heap.
	heap.Push(&nodeHeap, distance[targetVertex])

	for nodeHeap.Len() != 0 {
		// Fetch the node within the smallest distance from our source
		// from the heap.
		partialPath := heap.Pop(&nodeHeap).(nodeWithDist)
		bestNode := partialPath.node

		// If we've reached our source (or we don't have any incoming
		// edges), then we're done here and can exit the graph
		// traversal early.
		if bytes.Equal(bestNode.PubKeyBytes[:], sourceVertex[:]) {
			break
		}

		// Now that we've found the next potential step to take we'll
		// examine all the incoming edges (channels) from this node to
		// further our graph traversal.
		pivot := Vertex(bestNode.PubKeyBytes)
		err := bestNode.ForEachChannel(tx, func(tx *bbolt.Tx,
			edgeInfo *channeldb.ChannelEdgeInfo,
			_, inEdge *channeldb.ChannelEdgePolicy) error {

			// If there is no edge policy for this candidate
			// node, skip. Note that we are searching backwards
			// so this node would have come prior to the pivot
			// node in the route.
			if inEdge == nil {
				return nil
			}

			// We'll query the lower layer to see if we can obtain
			// any more up to date information concerning the
			// bandwidth of this edge.
			edgeBandwidth, ok := g.bandwidthHints[edgeInfo.ChannelID]
			if !ok {
				// If we don't have a hint for this edge, then
				// we'll just use the known Capacity as the
				// available bandwidth.
				edgeBandwidth = lnwire.NewMSatFromSatoshis(
					edgeInfo.Capacity,
				)
			}

			// Before we can process the edge, we'll need to fetch
			// the node on the _other_ end of this channel as we
			// may later need to iterate over the incoming edges of
			// this node if we explore it further.
			channelSource, err := edgeInfo.FetchOtherNode(
				tx, pivot[:],
			)
			if err != nil {
				return err
			}

			// Check if this candidate node is better than what we
			// already have.
			processEdge(channelSource, inEdge, edgeBandwidth, pivot)
			return nil
		})
		if err != nil {
			return nil, err
		}

		// Then, we'll examine all the additional edges from the node
		// we're currently visiting. Since we don't know the capacity
		// of the private channel, we'll assume it was selected as a
		// routing hint due to having enough capacity for the payment
		// and use the payment amount as its capacity.
		bandWidth := partialPath.amountToReceive
		for _, reverseEdge := range additionalEdgesWithSrc[bestNode.PubKeyBytes] {
			processEdge(reverseEdge.sourceNode, reverseEdge.edge,
				bandWidth, pivot)
		}
	}

	// If the source node isn't found in the next hop map, then a path
	// doesn't exist, so we terminate in an error.
	if _, ok := next[sourceVertex]; !ok {
		return nil, newErrf(ErrNoPathFound, "unable to find a path to "+
			"destination")
	}

	// Use the nextHop map to unravel the forward path from source to
	// target.
	pathEdges := make([]*channeldb.ChannelEdgePolicy, 0, len(next))
	currentNode := sourceVertex
	for currentNode != targetVertex { // TODO(roasbeef): assumes no cycles
		// Determine the next hop forward using the next map.
		nextNode := next[currentNode]

		// Add the next hop to the list of path edges.
		pathEdges = append(pathEdges, nextNode)

		// Advance current node.
		currentNode = Vertex(nextNode.Node.PubKeyBytes)
	}

	// The route is invalid if it spans more than 20 hops. The current
	// Sphinx (onion routing) implementation can only encode up to 20 hops
	// as the entire packet is fixed size. If this route is more than 20
	// hops, then it's invalid.
	numEdges := len(pathEdges)
	if numEdges > HopLimit {
		if r.stopAtMaxHopsExceeded {
			return pathEdges, newErr(ErrMaxHopsExceeded,
				"potential path has too many hops")

		}
		// Make another attempt to find a route which is not the
		// shortest path in terms of fees, but with number of
		// hops below the max.
		pathKEdges, err := findKPaths(
			g,
			r,
			sourceNode, target,
			amt,
			pathEdges,
			1,
		)
		if err != nil || len(pathKEdges) == 0 {
			return nil, newErr(ErrMaxHopsExceeded, "shortest"+
				" path has too many hops. No alternate "+
				" path found.")
		}
		// Remove the first artificial node added at path
		// beginning.
		pathEdges = pathKEdges[0][1:]
	}

	return pathEdges, nil
}

// prepareHopPegs adds pegged hops for pegged channel ids.
func prepareHopPegs(graph *channeldb.ChannelGraph,
	in []HopPeg) ([]HopPeg, error) {

	var (
		expanded []HopPeg
		out      []HopPeg
	)

	// If channel id is pegged too, find the node connected to it and
	// add it as pegged node.
	for i, peg := range in {
		if peg.ChannelID != 0 {
			if i == 0 {
				return nil, newErrf(ErrPegNotInNetwork,
					fmt.Sprintf("Error processing "+
						" pegged hops"))
			}
			edgeInfo, _, _, err :=
				graph.FetchChannelEdgesByID(peg.ChannelID)
			if err != nil {
				return nil, err
			}
			pegVertex := NewVertex(peg.NodeID)
			prevHopBytes, err :=
				edgeInfo.OtherNodeKeyBytes(pegVertex[:])
			if err != nil {
				return nil, err
			}
			pub, err :=
				btcec.ParsePubKey(prevHopBytes, btcec.S256())
			if err != nil {
				return nil, err
			}
			pPeg := HopPeg{
				NodeID: pub,
			}
			expanded = append(expanded, pPeg)
		}
		expanded = append(expanded, peg)
	}

	// This should never happen, as both source and target should be
	// included in pegged hops.
	if len(expanded) < 2 {
		return expanded, nil
	}

	// Remove duplicate pegs. If a previous hop was added as peg due to
	// pegged channel ID, an already existing pegged hop may exist and
	// should be removed.
	for i, peg := range expanded {
		if peg.ChannelID == 0 && i > 0 &&
			NewVertex(peg.NodeID) ==
				NewVertex(expanded[i-1].NodeID) {
			continue
		}
		out = append(out, peg)
	}
	return out, nil
}

// findPaths implements a k-shortest paths algorithm to find all the reachable
// paths between the passed source and target. First the shortest path is found
// and the k-shortest paths are then calculated based on it.
func findPaths(tx *bbolt.Tx, graph *channeldb.ChannelGraph,
	source *channeldb.LightningNode, target *btcec.PublicKey,
	amt lnwire.MilliSatoshi, feeLimit lnwire.MilliSatoshi, numPaths uint32,
	bandwidthHints map[uint64]lnwire.MilliSatoshi,
	additionalPegs []HopPeg) ([][]*channeldb.ChannelEdgePolicy, error) {

	ignoredVertexes := make(map[Vertex]struct{})
	ignoredEdges := make(map[edgeLocator]struct{})

	// TODO(roasbeef): modifying ordering within heap to eliminate final
	// sorting step?
	var (
		shortestPaths  [][]*channeldb.ChannelEdgePolicy
		candidatePaths pathHeap
		startingPath   []*channeldb.ChannelEdgePolicy
		pegs           []HopPeg
		segPaths       [][]*channeldb.ChannelEdgePolicy
	)
	heap.Init(&candidatePaths)

	// The path is computed in segments, from source to first peg, to
	// second peg, etc. until target. The different segments are then
	// stitched together to form an end to end shortest path.
	src, _ := source.PubKey()
	originVertex := NewVertex(src)

	// Include the source and destination as additional pegs.
	pegs = append(pegs, HopPeg{NodeID: src})
	pegs = append(pegs, additionalPegs...)
	pegs = append(pegs, HopPeg{NodeID: target})

	// If channel id is pegged too, find the node connected to it and
	// add it as pegged node.
	pegs, err := prepareHopPegs(graph, pegs)
	if err != nil {
		return nil, err
	}

	// Segment parameters used later to save re-calculations.
	type segmentParams struct {
		src    *channeldb.LightningNode
		dest   *btcec.PublicKey
		pegged bool
	}
	segParams := make([]*segmentParams, len(pegs)-1)
	// Break path to segments between pegs.
	for j := 1; j < len(pegs); j++ {
		var segPath []*channeldb.ChannelEdgePolicy
		peg := pegs[j]
		prevPeg := pegs[j-1]
		prevNode, err := graph.FetchLightningNode(prevPeg.NodeID)
		if err != nil {
			return nil, err
		}

		// First we'll find a single shortest path from the source
		// (prev step) to the target (next step) that's capable of
		// carrying amt satoshis along the path before fees are
		// calculated.
		if peg.ChannelID == 0 {
			segPath, err = findPath(
				&graphParams{
					tx:             tx,
					graph:          graph,
					bandwidthHints: bandwidthHints,
					originVertex:   &originVertex,
				},
				&restrictParams{
					ignoredNodes: ignoredVertexes,
					ignoredEdges: ignoredEdges,
					feeLimit:     feeLimit,
				},
				prevNode, peg.NodeID, amt,
			)
			if err != nil {
				log.Errorf("Unable to find path: %v", err)
				return nil, err
			}
		} else {
			// create a one-hop pegged path from previous
			// hop through pegged channel.
			info, p1, p2, err :=
				graph.FetchChannelEdgesByID(peg.ChannelID)
			if err != nil {
				return nil, err
			}

			// Select the relevant edge policy.
			policy := p2
			nodeID := NewVertex(peg.NodeID)
			if bytes.Equal(nodeID[:], info.NodeKey2Bytes[:]) {
				policy = p1
			}
			segPath = append(segPath, policy)
		}

		// Make sure the segment edges are not transversed
		// again in next segments of the path
		for _, edge := range segPath {
			locator := newEdgeLocator(edge)
			ignoredEdges[*locator] = struct{}{}
		}

		// Stitch the path to the previous steps
		startingPath = append(startingPath, segPath...)

		// Keep the shortest segment paths for later stitching
		// with alternate paths of other segments.
		segPaths = append(segPaths, segPath)

		// Keep segement parameters and save some cycles when
		// expanding to K-paths.
		segParams[j-1] = &segmentParams{
			src:    prevNode,
			dest:   peg.NodeID,
			pegged: peg.ChannelID != 0,
		}
	}

	// Check thta total fee of stitched path is below the limit.
	startingPathFee := computePathFee(amt, startingPath)
	if startingPathFee > feeLimit {
		log.Errorf("Shortest path exceeds path limit")
		return nil, newErr(ErrNoRouteFound, "shortest path exceeds "+
			"fee limit")
	}

	// Closure helper for stiching segment path to all other shortest
	// segments to form and end to end path.
	stitchPath := func(i int, path []*channeldb.ChannelEdgePolicy,
	) (stitched []*channeldb.ChannelEdgePolicy) {

		for j := 0; j < len(segPaths); j++ {
			seg := segPaths[j]
			if i == j {
				seg = path
			}
			stitched = append(stitched, seg...)
		}
		return stitched
	}

	// For each segment in the path, get up to additional numPath-1 paths.
	// Stitch each candidate of segment-i with the shortest path composed
	// of all other segments to form a candidate end-to-end path. Then
	// select up to numPath end to end shortest paths. This does not
	// necessarily end up with the shortest set of paths, rather is a
	// complexity compromise.
	for i := 0; i < len(segPaths); i++ {
		// no point in calculating alternate paths if
		// channel is fixed between source and destination.
		if segParams[i].pegged {
			continue
		}
		sSrc := segParams[i].src
		sDest := segParams[i].dest
		sFeeLimit := noFeeLimit
		if feeLimit != noFeeLimit {
			segFee := computePathFee(amt, segPaths[i])
			sFeeLimit = feeLimit - startingPathFee + segFee
		}
		// Copy ignored edges apart from edges included in this
		// segment.
		sIgnoredEdges := make(map[edgeLocator]struct{})
		for k, v := range ignoredEdges {
			sIgnoredEdges[k] = v
		}
		for _, e := range segPaths[i] {
			delete(sIgnoredEdges, *newEdgeLocator(e))
		}

		// Get additional paths for the segment.
		segKPaths, err := findKPaths(
			&graphParams{
				tx:             tx,
				graph:          graph,
				bandwidthHints: bandwidthHints,
				originVertex:   &originVertex,
			},
			&restrictParams{
				ignoredNodes: ignoredVertexes,
				ignoredEdges: sIgnoredEdges,
				feeLimit:     sFeeLimit,
			},
			sSrc, sDest,
			amt,
			segPaths[i],
			numPaths,
		)
		if err != nil {
			return nil, err
		}

		// Stitch the segment paths found for this segment with the
		// shortest paths of all other segments.
		for j, segKPath := range segKPaths {

			// Avoid duplicates. The shortest path is selected
			// already in the first iteration.
			if i != 0 && j == 0 {
				continue
			}

			// replace the first segment shortest path
			// with a replica of itself starting with an
			// artificial path from the source node.
			if i == 0 && j == 0 {
				segPaths[0] = segKPath
			}

			// Stitch the segments to an end to end path
			stitchedPath := stitchPath(i, segKPath)
			stitchedPathLen := len(stitchedPath)

			// Filter out those which are too long. Fee limit
			// was already taken into account when calculating
			// K paths for the segment.
			if stitchedPathLen > HopLimit {
				continue
			}

			// Push stitched path to list of candidates.
			newPath := path{
				hops: stitchedPath,
				dist: stitchedPathLen,
			}
			heap.Push(&candidatePaths, newPath)
		}
	}

	// Select best candidates.
	for k := uint32(0); k < numPaths; k++ {

		// If our min-heap of candidate paths is empty, then we can
		// exit early.
		if candidatePaths.Len() == 0 {
			break
		}
		nextShortestPath := heap.Pop(&candidatePaths).(path).hops
		shortestPaths = append(shortestPaths, nextShortestPath)
	}

	return shortestPaths, nil
}

// findKPaths implements a k-shortest paths algorithm to find all the reachable
// paths between the passed source and target. The algorithm will continue to
// traverse the graph until all possible candidate paths have been depleted.
// This function implements a modified version of Yen's. To find each path
// itself, we utilize our modified version of Dijkstra's found above. When
// examining possible spur and root paths, rather than removing edges or
// Vertexes from the graph, we instead utilize a Vertex+edge black-list that
// will be ignored by our modified Dijkstra's algorithm. With this approach, we
// make our inner path finding algorithm aware of our k-shortest paths
// algorithm, rather than attempting to use an unmodified path finding
// algorithm in a block box manner.
func findKPaths(g *graphParams, r *restrictParams,
	source *channeldb.LightningNode, target *btcec.PublicKey,
	amt lnwire.MilliSatoshi,
	startingPath []*channeldb.ChannelEdgePolicy,
	numPaths uint32) ([][]*channeldb.ChannelEdgePolicy, error) {

	sourceVertex := Vertex(source.PubKeyBytes)

	// TODO(roasbeef): modifying ordering within heap to eliminate final
	// sorting step?
	var (
		shortestPaths  [][]*channeldb.ChannelEdgePolicy
		candidatePaths pathHeap
		validPathsNum  uint32
	)
	heap.Init(&candidatePaths)

	// Manually insert a "self" edge emanating from ourselves. This
	// self-edge is required in order for the path finding algorithm to
	// function properly.
	firstPath := make([]*channeldb.ChannelEdgePolicy, 0,
		len(startingPath)+1)
	firstPath = append(firstPath, &channeldb.ChannelEdgePolicy{
		Node: source,
	})
	firstPath = append(firstPath, startingPath...)
	if len(firstPath) <= HopLimit+1 {
		validPathsNum++
	}

	shortestPaths = append(shortestPaths, firstPath)

	// While we still have candidate paths to explore we'll keep exploring
	// the sub-graphs created to find the next k-th shortest path.
	for k := uint32(1); validPathsNum < numPaths; k++ {
		prevShortest := shortestPaths[k-1]

		// We'll examine each edge in the previous iteration's shortest
		// path in order to find path deviations from each node in the
		// path.
		for i := 0; i < len(prevShortest)-1; i++ {
			// These two maps will mark the edges and Vertexes
			// we'll exclude from the next path finding attempt.
			// These are required to ensure the paths are unique
			// and loopless.
			ignoredEdges := make(map[edgeLocator]struct{})
			// Make sure to respect globally ignored edges.
			for k, v := range r.ignoredEdges {
				ignoredEdges[k] = v
			}
			ignoredVertexes := make(map[Vertex]struct{})

			// Our spur node is the i-th node in the prior shortest
			// path, and our root path will be all nodes in the
			// path leading up to our spurNode.
			spurNode := prevShortest[i].Node
			rootPath := prevShortest[:i+1]

			// Before we kickoff our next path finding iteration,
			// we'll find all the edges we need to ignore in this
			// next round. This ensures that we create a new unique
			// path.
			for _, path := range shortestPaths {
				// If our current rootPath is a prefix of this
				// shortest path, then we'll remove the edge
				// directly _after_ our spur node from the
				// graph so we don't repeat paths.
				if len(path) > i+1 &&
					isSamePath(rootPath, path[:i+1]) {

					locator := newEdgeLocator(path[i+1])
					ignoredEdges[*locator] = struct{}{}
				}
			}

			// Next we'll remove all entries in the root path that
			// aren't the current spur node from the graph. This
			// ensures we don't create a path with loops.
			for _, hop := range rootPath {
				node := hop.Node.PubKeyBytes
				if node == spurNode.PubKeyBytes {
					continue
				}

				ignoredVertexes[Vertex(node)] = struct{}{}
			}

			// With the edges that are part of our root path, and
			// the Vertexes (other than the spur path) within the
			// root path removed, we'll attempt to find another
			// shortest path from the spur node to the destination.
			spurPath, err := findPath(
				g,
				&restrictParams{
					ignoredNodes:          ignoredVertexes,
					ignoredEdges:          ignoredEdges,
					feeLimit:              r.feeLimit,
					stopAtMaxHopsExceeded: true,
				}, spurNode, target, amt,
			)

			// If we weren't able to find a path, we'll continue to
			// the next round.
			if IsError(err, ErrNoPathFound) {
				continue
			} else if err != nil && !IsError(err, ErrMaxHopsExceeded) {
				return nil, err
			}

			// Create the new combined path by concatenating the
			// rootPath to the spurPath.
			newPathLen := len(rootPath) + len(spurPath)
			if newPathLen > HopLimit+1 && validPathsNum > 0 {
				// Cannot provide alternate paths in presence of
				// shortest paths which are above hop limit. This
				// is a potential for DoS attack on computing
				// resources.
				break
			}

			newPath := path{
				hops: make([]*channeldb.ChannelEdgePolicy, 0, newPathLen),
				dist: newPathLen,
			}
			newPath.hops = append(newPath.hops, rootPath...)
			newPath.hops = append(newPath.hops, spurPath...)

			// TODO(roasbeef): add and consult path finger print

			// We'll now add this newPath to the heap of candidate
			// shortest paths.
			heap.Push(&candidatePaths, newPath)
		}

		// If our min-heap of candidate paths is empty, then we can
		// exit early.
		if candidatePaths.Len() == 0 {
			break
		}

		// To conclude this latest iteration, we'll take the shortest
		// path in our set of candidate paths and add it to our
		// shortestPaths list as the *next* shortest path.
		nextShortestPath := heap.Pop(&candidatePaths).(path).hops
		if len(nextShortestPath) <= HopLimit+1 {
			validPathsNum++
		}
		shortestPaths = append(shortestPaths, nextShortestPath)
	}

	// Filter out too-long paths.
	fShortestPaths := shortestPaths[:0]
	for _, path := range shortestPaths {
		if len(path) <= HopLimit+1 {
			fShortestPaths = append(fShortestPaths, path)
		}
	}

	// Pop the first artificially added edge if its not the first path
	// segment.
	if g.originVertex != nil && sourceVertex != *g.originVertex {
		for i, path := range fShortestPaths {
			path[0] = nil
			path = path[1:]
			fShortestPaths[i] = path
		}
	}

	return fShortestPaths, nil
}
