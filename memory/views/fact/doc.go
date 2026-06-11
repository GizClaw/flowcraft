// Package fact contains derived fact ledger and graph view contracts.
//
// Ledger persists longer-lived facts reconciled from observation ledger outputs
// and canonical evidence refs. It is not canonical truth and it is not a
// retrieval projection; records remain rebuildable and reconcilable from their
// observation lineage.
// Graph persists the semantic entity/value nodes and relation edges derived from
// fact ledger outputs for downstream retrieval or reasoning views.
package fact
