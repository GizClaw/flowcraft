package main

import (
	"bytes"
	"encoding/json"
	"net/netip"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/utils/net"
)

// The net dump is committed and compared byte for byte, so it has to be a
// function of core alone -- no DNS, no host state. The checks below keep the
// sample set honest rather than the writer: a row that lost its vectors would
// still compare equal to itself, and a corpus that stopped covering one of the
// four pattern shapes would pass just as quietly.

func generateNet(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if err := writeNetContract(&buffer); err != nil {
		t.Fatalf("writeNetContract: %v", err)
	}
	return buffer.Bytes()
}

func decodeNet(t *testing.T) netContract {
	t.Helper()
	var document netContract
	if err := json.Unmarshal(generateNet(t), &document); err != nil {
		t.Fatalf("the document does not decode: %v", err)
	}
	return document
}

func TestNetContractIsReproducible(t *testing.T) {
	first, second := generateNet(t), generateNet(t)
	if !bytes.Equal(first, second) {
		t.Fatal("two dumps of the same inputs differ")
	}
}

func TestNetPatternRowsCarryTheirVectors(t *testing.T) {
	document := decodeNet(t)
	if len(document.Patterns) < 50 {
		t.Fatalf("the corpus has %d patterns", len(document.Patterns))
	}
	for _, row := range document.Patterns {
		if row.Error != "" {
			if row.Match != nil || row.MatchIP != nil {
				t.Fatalf("%q is refused and still carries matches", row.Raw)
			}
			continue
		}
		if len(row.Match) != len(document.Hosts) {
			t.Fatalf("%q has %d host answers for %d hosts", row.Raw, len(row.Match), len(document.Hosts))
		}
		if len(row.MatchIP) != len(document.IPs) {
			t.Fatalf("%q has %d address answers for %d addresses", row.Raw, len(row.MatchIP), len(document.IPs))
		}
	}
}

// The four shapes are the contract; a corpus that stopped reaching one of them
// would only be measuring three.
func TestNetCorpusReachesEveryPatternShape(t *testing.T) {
	document := decodeNet(t)
	var domains, wildcards, addresses, prefixes int
	for _, row := range document.Patterns {
		switch {
		case row.Error != "":
		case strings.HasPrefix(row.Raw, "*."):
			wildcards++
		case row.IsIP && strings.Contains(row.Raw, "/"):
			prefixes++
		case row.IsIP:
			addresses++
		default:
			domains++
		}
	}
	if domains < 10 || wildcards < 4 || addresses < 5 || prefixes < 5 {
		t.Fatalf("shapes are thin: %d domains, %d wildcards, %d addresses, %d prefixes",
			domains, wildcards, addresses, prefixes)
	}
}

// Every answer a Matcher gives is a rule's answer, and the reason travels with
// a deny. If no row carried one, the port could drop the field and the dump
// would not notice.
func TestNetMatcherRowsExerciseTheVerdicts(t *testing.T) {
	document := decodeNet(t)
	var allows, denies, unmatched, withReason int
	for _, row := range document.Matchers {
		if row.Error != "" {
			continue
		}
		if len(row.Hosts) == 0 && len(row.IPs) == 0 {
			t.Fatalf("matcher %q asks nothing", row.Name)
		}
		for _, query := range row.Hosts {
			switch {
			case !query.Matched:
				unmatched++
			case query.Action == net.NetDeny.String():
				denies++
			default:
				allows++
			}
			if query.Reason != "" {
				withReason++
			}
		}
		for _, query := range row.IPs {
			switch {
			case !query.Matched:
				unmatched++
			case query.Action == net.NetDeny.String():
				denies++
			default:
				allows++
			}
			if query.Reason != "" {
				withReason++
			}
		}
	}
	if allows == 0 || denies == 0 || unmatched == 0 || withReason == 0 {
		t.Fatalf("verdicts are thin: %d allow, %d deny, %d unmatched, %d with a reason",
			allows, denies, unmatched, withReason)
	}
}

// The mapped form is the one place where the address's width decides the
// answer, and it only shows up if both spellings are in the query list.
func TestNetAddressQueriesCoverTheMappedForm(t *testing.T) {
	document := decodeNet(t)
	var mapped, unmapped, zoned bool
	for _, raw := range document.IPs {
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			t.Fatalf("%q does not parse: %v", raw, err)
		}
		switch {
		case addr.Is4In6():
			mapped = true
		case addr.Zone() != "":
			zoned = true
		case addr.Is4():
			unmapped = true
		}
	}
	if !mapped || !unmapped || !zoned {
		t.Fatalf("addresses are thin: mapped=%v unmapped=%v zoned=%v", mapped, unmapped, zoned)
	}
}
