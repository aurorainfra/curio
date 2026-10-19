package mk12

// Aurora: lets aurora/marketlog record deal decisions.

import (
	"context"

	"github.com/filecoin-project/go-address"

	"github.com/filecoin-project/curio/aurora/marketlog"
)

// Outcome implements marketlog.Outcome.
func (r *ProviderDealRejectionInfo) Outcome() (bool, string) {
	if r == nil {
		return false, ""
	}
	return r.Accepted, r.Reason
}

// auroraDecision starts recording the decision for a deal proposal.
func (m *MK12) auroraDecision(ctx context.Context, dp *DealParams) (context.Context, *marketlog.Decision) {
	prop := dp.ClientDealProposal.Proposal
	spID, _ := address.IDFromAddress(prop.Provider)
	return marketlog.Start(ctx, m.db, "mk12", dp.DealUUID.String(), int64(spID), prop.Client.String(),
		prop.PieceCID.String(), int64(prop.PieceSize), prop.VerifiedDeal, dp.IsOffline)
}

func auroraNoteCIDGravity(ctx context.Context, r cidGravityResponse) {
	marketlog.NoteCIDGravity(ctx, marketlog.CIDGravity{
		Decision:                r.Decision,
		InternalDecision:        r.InternalDecision,
		ExternalDecision:        r.ExternalDecision,
		InternalMessage:         r.InternalMessage,
		ExternalMessage:         r.ExternalMessage,
		CustomMessage:           r.CustomMessage,
		MatchingRule:            r.MatchingRule,
		MatchingAcceptanceLogic: r.MatchingAcceptanceLogic,
		MatchingPricing:         r.MatchingPricing,
	})
}
