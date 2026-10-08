package hostmcp

// CharterRow documents one host primitive and the grant it needs.
type CharterRow struct {
	Tool  string
	Grant string
}

// Charter is the primitive table; standard_charter_test scans it
// against the tools Standard registers.
var Charter = []CharterRow{
	{Tool: "host_about"},
	{Tool: "secret_get", Grant: "secrets:auth"},
	{Tool: "secret_set", Grant: "secrets:auth"},
	{Tool: "secret_delete", Grant: "secrets:auth"},
	{Tool: "workspace_current"},
	{Tool: "open_url", Grant: "host:open_url"},
	{Tool: "inference_upsert", Grant: "inference:write"},
	{Tool: "inference_remove", Grant: "inference:write"},
	{Tool: "session_import", Grant: "sessions:import"},
	{Tool: "session_imported_sources", Grant: "sessions:import"},
	{Tool: "telemetry_configure", Grant: "telemetry:export"},
	{Tool: "telemetry_disable", Grant: "telemetry:export"},
	{Tool: "emit_event", Grant: "events:emit"},
}

// CharterTools returns the sorted tool names of the table.
func CharterTools() []string {
	out := make([]string, 0, len(Charter))
	for _, row := range Charter {
		out = append(out, row.Tool)
	}
	return out
}
