// Package knowledge — deprecation index.
//
// This file is the single source of truth for the v0.2.x → v0.3.0
// migration. Each symbol below is marked // Deprecated: at its
// declaration site so that staticcheck (SA1019) flags callers; this
// file collects the full mapping so reviewers can see the surface area
// at a glance.
//
// === Deprecated symbols (removed in v0.3.0) ===
//
//	Storage / orchestration
//	  Store                    -> *Service                      (sdk/knowledge)
//	  FSStore                  -> factory.NewLocal              (sdk/knowledge/factory)
//	  RetrievalStore           -> factory.NewRetrieval          (sdk/knowledge/factory)
//	  CachedStore              -> (none — fold caching into the repo)
//
//	Data models
//	  Document                 -> SourceDocument + DerivedLayer
//	  SearchResult             -> Hit
//	  SearchOptions            -> Query (with Scope/Mode/Layer)
//	  Chunk                    -> DerivedChunk
//	  ContextLayer             -> Layer        (alias kept for transition)
//	  SearchMode               -> Mode         (alias kept for transition)
//	  ModeSemantic             -> ModeVector
//
//	Graph node
//	  KnowledgeConfig          -> KnowledgeNodeConfig
//	  KnowledgeNode            -> KnowledgeServiceNode
//	  NewKnowledgeNode         -> NewKnowledgeServiceNode
//	  KnowledgeConfigFromMap   -> KnowledgeNodeConfigFromMap
//	  RegisterNode             -> RegisterServiceNode
//	  KnowledgeNodeSchema      -> KnowledgeServiceNodeSchema
//
//	LLM tools
//	  NewSearchTool            -> NewSearchServiceTool
//	  NewAddTool               -> NewPutServiceTool
//
//	Reload pipeline
//	  ChangeNotifier           -> EventNotifier  (typed ChangeEvent stream)
//	  Reloader                 -> EventReloader  (scope-aware, serialised)
//	  NewReloader              -> NewEventReloader
//
// === Behaviour bridges that survive v0.3.0 ===
//
//	ResolveMode("")           -> ModeBM25
//	ResolveMode("semantic")   -> ModeVector
//	KnowledgeNodeConfigFromMap reads "max_layer" as "layer" when
//	  "layer" is absent.
//
// === Things that are NOT deprecated ===
//
//	GenerateDocumentContext / GenerateDatasetContext — the L0/L1
//	  derivation helpers remain external to Service so callers control
//	  scheduling, retry and persistence policy.
//	DatasetQuery — re-used by KnowledgeNodeConfig.
//	Tokenizer / textsearch.Tokenizer — backend-neutral utility.
package knowledge
