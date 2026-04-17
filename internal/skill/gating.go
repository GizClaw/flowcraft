package skill

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// evaluateGating checks whether the host environment satisfies a skill's
// declared requirements. The result is attached to SkillMeta.Gating
// during BuildIndex.
func evaluateGating(meta *SkillMeta) *SkillGating {
	if meta.Requires == nil {
		return &SkillGating{Available: true}
	}

	g := &SkillGating{Available: true}
	req := meta.Requires

	if len(req.OS) > 0 {
		matched := false
		for _, osName := range req.OS {
			if strings.EqualFold(osName, runtime.GOOS) {
				matched = true
				break
			}
		}
		if !matched {
			return &SkillGating{
				Available: false,
				Reason:    "unsupported OS: " + runtime.GOOS,
			}
		}
	}

	for _, bin := range req.Bins {
		if _, err := exec.LookPath(bin); err != nil {
			g.MissingBins = append(g.MissingBins, bin)
		}
	}

	if len(req.AnyBins) > 0 {
		found := false
		for _, bin := range req.AnyBins {
			if _, err := exec.LookPath(bin); err == nil {
				found = true
				break
			}
		}
		if !found {
			g.MissingAnyBins = append(g.MissingAnyBins, req.AnyBins...)
		}
	}

	for _, env := range req.Env {
		if _, ok := os.LookupEnv(env); !ok {
			g.MissingEnv = append(g.MissingEnv, env)
		}
	}

	if len(g.MissingBins) > 0 || len(g.MissingAnyBins) > 0 || len(g.MissingEnv) > 0 {
		g.Available = false
	}

	return g
}

// gatingDeps extracts a human-readable missing dependencies list from SkillGating.
func gatingDeps(g *SkillGating) []string {
	if g == nil || g.Available {
		return nil
	}
	var deps []string
	deps = append(deps, g.MissingBins...)
	if len(g.MissingAnyBins) > 0 {
		deps = append(deps, "one of: "+strings.Join(g.MissingAnyBins, "|"))
	}
	deps = append(deps, g.MissingEnv...)
	return deps
}

// formatGatingMessage builds a warning string for unavailable skills.
func formatGatingMessage(meta *SkillMeta) string {
	if meta.Gating == nil || meta.Gating.Available {
		return ""
	}
	if meta.Gating.Reason != "" {
		return meta.Gating.Reason
	}
	var parts []string
	if len(meta.Gating.MissingBins) > 0 {
		parts = append(parts, "missing binaries: "+strings.Join(meta.Gating.MissingBins, ", "))
	}
	if len(meta.Gating.MissingAnyBins) > 0 {
		parts = append(parts, "need one of: "+strings.Join(meta.Gating.MissingAnyBins, ", "))
	}
	if len(meta.Gating.MissingEnv) > 0 {
		parts = append(parts, "missing env vars: "+strings.Join(meta.Gating.MissingEnv, ", "))
	}
	return strings.Join(parts, "; ")
}
