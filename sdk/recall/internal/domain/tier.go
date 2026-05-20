package domain

// Save-tier intent labels (Phase D.3). These are caller-supplied
// importance hints on SaveRequest only — they are not persisted on
// TemporalFact. Map to Confidence adjustments in ingest/salience.go.
const (
	TierCore     = "core"
	TierGeneral  = "general"
	TierData     = "data"
	TierStorage  = "storage"
)

// NormalizeSaveTier returns the effective tier for an empty or
// unknown input (defaults to general).
func NormalizeSaveTier(tier string) string {
	switch tier {
	case TierCore, TierGeneral, TierData, TierStorage:
		return tier
	default:
		return TierGeneral
	}
}
