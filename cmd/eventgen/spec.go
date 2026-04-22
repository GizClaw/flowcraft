package main

// Spec is the fully-resolved contract after loading manifest + includes + payload refs.
type Spec struct {
	SpecVersion int
	Partitions  []PartitionDef
	Categories  map[string]CategoryDef
	Lint        LintConfig
	Events      []EventDef
	Payloads    map[string]PayloadDef // Go struct name -> definition
}

type PartitionDef struct {
	Name   string `yaml:"name"`
	Format string `yaml:"format"`
	Doc    string `yaml:"doc,omitempty"`
}

type CategoryDef struct {
	DefaultTTL string `yaml:"default_ttl"`
	Doc        string `yaml:"doc,omitempty"`
}

type LintConfig struct {
	EnforceVerbWhitelist            bool `yaml:"enforce_verb_whitelist"`
	EnforcePartitionMatch           bool `yaml:"enforce_partition_match"`
	EnforceCategoryInCategories     bool `yaml:"enforce_category_in_categories"`
	EnforceAuditSummaryWhenRequired bool `yaml:"enforce_audit_summary_when_required"`
}

type EventDef struct {
	Name          string   `yaml:"-"`
	Domain        string   `yaml:"-"`
	Version       int      `yaml:"version"`
	Category      string   `yaml:"category"`
	Partition     string   `yaml:"partition"` // partition template name, e.g. runtime
	PayloadRef    string   `yaml:"payload_ref"`
	PayloadType   string   `yaml:"-"` // resolved Go/TS type name
	Producers     []string `yaml:"producers"`
	Consumers     []string `yaml:"consumers"`
	Transports    []string `yaml:"transports"`
	AuditRequired bool     `yaml:"audit_required"`
	AuditSummary  string   `yaml:"audit_summary"`
	Doc           string   `yaml:"doc"`
	Deprecated    bool     `yaml:"deprecated"`
}

type PayloadDef struct {
	Name   string
	Fields map[string]FieldDef
}

type FieldDef struct {
	Type      string // string, int64, int32, bool, timestamp, array, map, any
	ItemType  string `yaml:"item_type"`  // for array
	ValueType string `yaml:"value_type"` // for map
	Required  bool   `yaml:"required"`
}
