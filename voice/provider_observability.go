package speech

import "github.com/GizClaw/flowcraft/voice/provider"

const providerReportDataKey = "provider_report"

func withProviderReport(ev Event, report provider.Report) Event {
	if report.Operation == "" {
		return ev
	}
	if ev.Data == nil {
		ev.Data = make(map[string]any, 1)
	}
	ev.Data[providerReportDataKey] = report
	return ev
}

func providerReportFromEvent(ev Event) (provider.Report, bool) {
	if ev.Data == nil {
		return provider.Report{}, false
	}
	report, ok := ev.Data[providerReportDataKey]
	if !ok {
		return provider.Report{}, false
	}
	switch v := report.(type) {
	case provider.Report:
		return v, true
	case *provider.Report:
		if v == nil {
			return provider.Report{}, false
		}
		return *v, true
	default:
		return provider.Report{}, false
	}
}
