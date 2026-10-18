package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/netip"
	"strings"
	"testing"

	corenet "github.com/GizClaw/flowcraft/core/utils/net"
)

// The proxy dump is committed and compared byte for byte, so it has to be a
// function of core alone -- no DNS, no sockets, no clock. The checks below keep
// the *sample set* honest rather than the writer:
//
//   - a row that lost its fields would still compare equal to itself;
//   - a decision row that resolved a name would make the dump depend on the
//     machine, which is the one thing the gate cannot tolerate;
//   - a URL row that stopped covering a refusal would pass just as quietly.

func generateProxy(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if err := writeProxyContract(&buffer); err != nil {
		t.Fatalf("writeProxyContract: %v", err)
	}
	return buffer.Bytes()
}

func decodeProxy(t *testing.T) proxyContract {
	t.Helper()
	var document proxyContract
	if err := json.Unmarshal(generateProxy(t), &document); err != nil {
		t.Fatalf("the document does not decode: %v", err)
	}
	return document
}

func TestProxyContractIsReproducible(t *testing.T) {
	first, second := generateProxy(t), generateProxy(t)
	if !bytes.Equal(first, second) {
		t.Fatal("two dumps of the same inputs differ")
	}
}

// splitOracle answers only "how does core split this destination", and it is a
// proxy with no rules, so no row can reach the resolver through it.
func splitOracle(t *testing.T) *corenet.Proxy {
	t.Helper()
	proxy, err := corenet.Start(corenet.ProxyConfig{
		Mode:        corenet.NetAllowList,
		TCPLoopback: true,
	})
	if err != nil {
		t.Fatalf("start the split oracle: %v", err)
	}
	t.Cleanup(func() {
		if err := proxy.Close(); err != nil {
			t.Fatalf("close the split oracle: %v", err)
		}
	})
	return proxy
}

// TestProxyDecisionsNeverResolve is the invariant the whole table rests on: a
// hostname with IP/CIDR rules in the policy is the one path that calls the
// resolver, and a resolver's answer is not a contract. Such a row would make
// the committed dump machine-dependent -- and its dial target would differ
// between the machine that wrote the file and the one that checks it.
func TestProxyDecisionsNeverResolve(t *testing.T) {
	split := splitOracle(t)
	for _, sample := range proxyDecisionSamples {
		rules := proxyNetRules(sample.rules)
		hasIPRule := false
		for _, rule := range rules {
			pattern, err := corenet.Compile(rule.Host)
			if err != nil {
				continue // a refused pattern cannot be an address rule
			}
			if pattern.IsIP() {
				hasIPRule = true
			}
		}
		if !hasIPRule {
			continue
		}
		decision, err := split.Decide(context.Background(), sample.request, sample.defaultPort)
		if err != nil {
			t.Fatalf("decision %q: split: %v", sample.name, err)
		}
		if _, err := netip.ParseAddr(decision.Host); err == nil {
			continue // an address is judged without resolving
		}
		matcher, err := corenet.NewMatcher(corenet.NetPolicy{
			Mode:       sample.mode,
			AllowHosts: sample.allowHosts,
			Rules:      rules,
		})
		if err != nil {
			continue // an uncompilable rule set refuses before resolving
		}
		if action, _, _, _ := matcher.Match(decision.Host, decision.Port); action == corenet.NetDeny {
			continue // a hostname denial returns before resolving
		}
		t.Fatalf(
			"decision %q carries an IP rule and an unblocked name (%q): the dump would need DNS",
			sample.name, sample.request,
		)
	}
}

func TestProxyURLRowsAreOneOrTheOther(t *testing.T) {
	document := decodeProxy(t)
	if len(document.URLs) < 40 {
		t.Fatalf("the corpus has %d URLs", len(document.URLs))
	}
	for _, row := range document.URLs {
		if row.Error == "" {
			continue
		}
		if row.Scheme != "" || row.Host != "" || row.HasUser || row.Path != "" ||
			row.RawQuery != "" || row.Fragment != "" || row.Opaque != "" {
			t.Fatalf("%q is refused and still carries fields", row.Raw)
		}
	}
	// The refusals the grammar is here for: a malformed escape, an invalid
	// literal, a bad port, a control byte, and a colon in the first segment.
	for _, want := range []string{
		"invalid URL escape",
		"invalid IP-literal",
		"invalid port",
		"invalid control character",
		"first path segment in URL cannot contain colon",
	} {
		found := false
		for _, row := range document.URLs {
			if strings.Contains(row.Error, want) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("no URL row covers %q any more", want)
		}
	}
}

func TestProxyValidationRowsCoverTheOrder(t *testing.T) {
	document := decodeProxy(t)
	if len(document.Validations) < 20 {
		t.Fatalf("the corpus has %d policies", len(document.Validations))
	}
	// The rules loop runs before the socket loop; the row that carries both
	// mistakes has to keep reporting the rule's refusal, or the order is no
	// longer pinned.
	for _, row := range document.Validations {
		if row.Name != "rule-and-socket-both-wrong" {
			continue
		}
		if !strings.Contains(row.Error, "net rule 1 port 70000 out of range") {
			t.Fatalf("the order row reports %q", row.Error)
		}
		return
	}
	t.Fatal("the order row is gone")
}

func TestProxyDecisionRowsCoverEveryBranch(t *testing.T) {
	document := decodeProxy(t)
	if len(document.Decisions) < 25 {
		t.Fatalf("the corpus has %d decisions", len(document.Decisions))
	}
	seen := map[string]bool{}
	for _, row := range document.Decisions {
		switch {
		case row.Error != "":
			seen["error"] = true
		case row.Action == "deny" && row.Reason != "":
			seen["denied-with-reason"] = true
		case row.Action == "deny":
			seen["denied"] = true
		case row.Rule == "":
			seen["mode-default"] = true
		default:
			seen["rule"] = true
		}
		if strings.HasPrefix(row.DialHost, "[") {
			seen["bracketed-dial"] = true
		}
		if row.Host == "" {
			seen["empty-host"] = true
		}
	}
	for _, want := range []string{
		"error",
		"denied-with-reason",
		"denied",
		"mode-default",
		"rule",
		"bracketed-dial",
		"empty-host",
	} {
		if !seen[want] {
			t.Fatalf("no decision row covers %q any more", want)
		}
	}
}
