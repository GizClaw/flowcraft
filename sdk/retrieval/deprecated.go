// Package retrieval is deprecated.
//
// Deprecated: use github.com/GizClaw/flowcraft/memory/retrieval instead.
// This compatibility package will be removed in v0.5.0.
package retrieval

import target "github.com/GizClaw/flowcraft/memory/retrieval"

type (
	Capabilities          = target.Capabilities
	Capability            = target.Capability
	Countable             = target.Countable
	DeletableByFilter     = target.DeletableByFilter
	Doc                   = target.Doc
	DocGetter             = target.DocGetter
	DocUpsertResult       = target.DocUpsertResult
	Droppable             = target.Droppable
	ExtensionCapabilities = target.ExtensionCapabilities
	Filter                = target.Filter
	Filterable            = target.Filterable
	Hit                   = target.Hit
	HybridMode            = target.HybridMode
	HybridRequest         = target.HybridRequest
	Hybridable            = target.Hybridable
	Index                 = target.Index
	Iterable              = target.Iterable
	LaneKey               = target.LaneKey
	LaneResult            = target.LaneResult
	ListOrderBy           = target.ListOrderBy
	ListRequest           = target.ListRequest
	ListResponse          = target.ListResponse
	PartialError          = target.PartialError
	Range                 = target.Range
	SearchDebug           = target.SearchDebug
	SearchExecution       = target.SearchExecution
	SearchRequest         = target.SearchRequest
	SearchResponse        = target.SearchResponse
	Snapshottable         = target.Snapshottable
	StageResult           = target.StageResult
	Vectorizable          = target.Vectorizable
)

const (
	CapabilityBM25                 = target.CapabilityBM25
	CapabilityCount                = target.CapabilityCount
	CapabilityDebug                = target.CapabilityDebug
	CapabilityDeleteByFilter       = target.CapabilityDeleteByFilter
	CapabilityDocGetter            = target.CapabilityDocGetter
	CapabilityDropNamespace        = target.CapabilityDropNamespace
	CapabilityFilterPushdown       = target.CapabilityFilterPushdown
	CapabilityHybrid               = target.CapabilityHybrid
	CapabilityIterable             = target.CapabilityIterable
	CapabilityNativeDeleteByFilter = target.CapabilityNativeDeleteByFilter
	CapabilitySnapshot             = target.CapabilitySnapshot
	CapabilitySparse               = target.CapabilitySparse
	CapabilityVector               = target.CapabilityVector
	CapabilityVectorizable         = target.CapabilityVectorizable
	HybridConvex                   = target.HybridConvex
	HybridDefault                  = target.HybridDefault
	HybridRRF                      = target.HybridRRF
	HybridWeighted                 = target.HybridWeighted
	LaneBM25                       = target.LaneBM25
	LaneEntity                     = target.LaneEntity
	LaneEntityLink                 = target.LaneEntityLink
	LaneFusion                     = target.LaneFusion
	LaneHybrid                     = target.LaneHybrid
	LanePostFilter                 = target.LanePostFilter
	LaneRerank                     = target.LaneRerank
	LaneSparse                     = target.LaneSparse
	LaneVector                     = target.LaneVector
	MetaSlotKey                    = target.MetaSlotKey
	OrderByIDAsc                   = target.OrderByIDAsc
	OrderByTimestampAsc            = target.OrderByTimestampAsc
	OrderByTimestampDesc           = target.OrderByTimestampDesc
)

var (
	ErrEmptyDeleteFilter      = target.ErrEmptyDeleteFilter
	ErrNoQuery                = target.ErrNoQuery
	AsCountable               = target.AsCountable
	AsDeletableByFilter       = target.AsDeletableByFilter
	AsDocGetter               = target.AsDocGetter
	AsDroppable               = target.AsDroppable
	AsHybrid                  = target.AsHybrid
	AsIterable                = target.AsIterable
	CapabilitiesOf            = target.CapabilitiesOf
	CloneDoc                  = target.CloneDoc
	CloneHit                  = target.CloneHit
	CloneHits                 = target.CloneHits
	DecodeListPageToken       = target.DecodeListPageToken
	DecodeListPageTokenFor    = target.DecodeListPageTokenFor
	DefaultMemoryCapabilities = target.DefaultMemoryCapabilities
	DocMatchesFilter          = target.DocMatchesFilter
	EncodeListPageToken       = target.EncodeListPageToken
	EncodeListPageTokenFor    = target.EncodeListPageTokenFor
	SlotKeyOf                 = target.SlotKeyOf
	Supports                  = target.Supports
)
