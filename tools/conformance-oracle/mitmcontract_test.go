package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// The dump is committed and compared byte for byte, so the harness has to be a
// function of the scripts and of core -- nothing else. These checks fail on the
// ways that goes wrong quietly:
//
//   - a row that carries the port it listened on, which differs every run;
//   - a masked header that stopped being masked, which is the only reason the
//     dump can hold a clock at all;
//   - a scripted client that only *looks* like a client, which is how the TLS
//     1.1 row silently became "the server waited for a body that never came"
//     for ten seconds before the lengths were fixed;
//   - a table that lost its rows, which would replay perfectly against nothing.

func generateMITM(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if err := writeMITMContract(&buffer); err != nil {
		t.Fatalf("writeMITMContract: %v", err)
	}
	return buffer.Bytes()
}

func decodeMITM(t *testing.T) mitmContract {
	t.Helper()
	var document mitmContract
	if err := json.Unmarshal(generateMITM(t), &document); err != nil {
		t.Fatalf("the document does not decode: %v", err)
	}
	return document
}

func TestMITMContractIsReproducible(t *testing.T) {
	first, second := generateMITM(t), generateMITM(t)
	if !bytes.Equal(first, second) {
		t.Fatal("two dumps of the same inputs differ")
	}
}

// TestMITMContractHasNoEphemeralPort: every listener is on 127.0.0.1:0 and the
// dialer ignores the address it is handed, so any port in the dump would differ
// on the next run.
func TestMITMContractHasNoEphemeralPort(t *testing.T) {
	port := regexp.MustCompile(`127\.0\.0\.1:\d+`)
	if found := port.FindAllString(string(generateMITM(t)), -1); len(found) > 0 {
		t.Fatalf("the dump carries a loopback port: %v", found)
	}
}

// TestMITMContractMasksWhatItSays keeps the mask list honest: a row that quietly
// grew a `date` would be compared against a clock.
func TestMITMContractMasksWhatItSays(t *testing.T) {
	document := decodeMITM(t)
	var walk func(prefix string, headers map[string][]string)
	walk = func(prefix string, headers map[string][]string) {
		for _, masked := range []string{"date", "connection", "x-pad"} {
			if _, present := headers[masked]; present {
				t.Errorf("%s carries a masked header: %s", prefix, masked)
			}
		}
	}
	for _, row := range document.Sessions {
		for _, request := range row.Requests {
			walk(row.Name+" request", request.Headers)
		}
		for _, event := range row.Trace {
			walk(row.Name+" trace", event.Headers)
		}
		for _, answer := range row.Client {
			walk(row.Name+" client", answer.Headers)
		}
		for _, seen := range row.UpstreamSaw {
			walk(row.Name+" upstream", seen.Headers)
		}
	}
}

func TestMITMContractKeepsItsRows(t *testing.T) {
	document := decodeMITM(t)
	if len(document.ALPN) < 7 {
		t.Errorf("alpn has %d rows, expected at least 7", len(document.ALPN))
	}
	if len(document.Sessions) < 21 {
		t.Errorf("sessions has %d rows, expected at least 21", len(document.Sessions))
	}
	for _, row := range document.Sessions {
		if row.Name == "" {
			t.Error("a session row has no name")
		}
		if len(row.Requests) == 0 {
			t.Errorf("%s: no request script", row.Name)
		}
		if len(row.Client) != len(row.Requests) {
			t.Errorf("%s: %d requests and %d answers", row.Name, len(row.Requests), len(row.Client))
		}
		for _, request := range row.Requests {
			if request.Authority == "" {
				t.Errorf("%s: a request has no authority", row.Name)
			}
			if request.Framing == "" {
				t.Errorf("%s: a request has no framing", row.Name)
			}
		}
	}
	// The rows that exist to show a refusal did refuse: a row whose status is
	// not the one it is named for proves nothing and replays fine.
	for _, row := range document.Sessions {
		if len(row.Client) == 0 {
			continue
		}
		switch {
		case strings.HasPrefix(row.Name, "deny-the-"):
			if row.Client[0].Status != 403 {
				t.Errorf("%s: answered %d, not 403", row.Name, row.Client[0].Status)
			}
		case strings.HasPrefix(row.Name, "upstream-"):
			if row.Client[0].Status != 502 {
				t.Errorf("%s: answered %d, not 502", row.Name, row.Client[0].Status)
			}
			if len(row.UpstreamSaw) != 0 {
				t.Errorf("%s: the upstream received %d requests", row.Name, len(row.UpstreamSaw))
			}
		}
	}
}

// TestMITMContractStillDistinguishesBodies: `null` and `""` are the whole reason
// a hook body is spelled as a pointer. A row that lost the distinction would
// compare equal to itself and prove nothing about whether a body was inspected.
func TestMITMContractStillDistinguishesBodies(t *testing.T) {
	document := decodeMITM(t)
	byName := map[string]mitmSessionRow{}
	for _, row := range document.Sessions {
		byName[row.Name] = row
	}
	over, ok := byName["post-over-the-cap"]
	if !ok {
		t.Fatal("the over-the-cap row is gone")
	}
	if len(over.Trace) < 1 || over.Trace[0].Body != nil {
		t.Errorf("post-over-the-cap: the hook was given %v, not no body", over.Trace[0].Body)
	}
	if len(over.UpstreamSaw) != 1 || over.UpstreamSaw[0].Body == nil {
		t.Fatal("post-over-the-cap: the upstream body is not recorded")
	}
	if *over.UpstreamSaw[0].Body != hexOf([]byte("hello")) {
		t.Errorf("post-over-the-cap: the upstream got %s, not the whole body", *over.UpstreamSaw[0].Body)
	}
	at, ok := byName["post-at-the-cap"]
	if !ok {
		t.Fatal("the at-the-cap row is gone")
	}
	if len(at.Trace) < 1 || at.Trace[0].Body == nil || *at.Trace[0].Body != hexOf([]byte("hello")) {
		t.Errorf("post-at-the-cap: the hook was given %v, not the body", at.Trace[0].Body)
	}
	// An inspected request with no body is `""`, not `null`: the hook was
	// handed an empty body rather than nothing. Both spellings exist in the
	// document, which is what makes the pair worth having.
	empty, ok := byName["response-over-the-cap"]
	if !ok {
		t.Fatal("the response-over-the-cap row is gone")
	}
	if len(empty.Trace) < 1 || empty.Trace[0].Body == nil || *empty.Trace[0].Body != "" {
		t.Errorf("response-over-the-cap: the bodyless request was spelled %v, not \"\"", empty.Trace[0].Body)
	}
}

// TestMITMContractTLSElevenHelloIsComplete: the row's client is bytes rather
// than a client, so nothing else checks that the record and handshake lengths
// agree with what was written. A hello that claims more than it carries is
// refused too, but for the wrong reason -- the server waits for the rest.
func TestMITMContractTLSElevenHelloIsComplete(t *testing.T) {
	document := decodeMITM(t)
	var row *mitmAlpnRow
	for index := range document.ALPN {
		if document.ALPN[index].Name == "tls-1-1-refused" {
			row = &document.ALPN[index]
		}
	}
	if row == nil {
		t.Fatal("the TLS 1.1 row is gone")
	}
	if row.OK {
		t.Error("the TLS 1.1 handshake was accepted")
	}
	if row.Offered != nil {
		t.Error("the TLS 1.1 row claims to be a client that could offer ALPN")
	}
	hello, err := hex.DecodeString(row.Hello)
	if err != nil {
		t.Fatalf("the hello is not hex: %v", err)
	}
	if len(hello) < 9 || hello[0] != 0x16 {
		t.Fatalf("the hello is not a handshake record: %x", hello)
	}
	if record := int(hello[3])<<8 | int(hello[4]); record != len(hello)-5 {
		t.Errorf("the record claims %d bytes and carries %d", record, len(hello)-5)
	}
	if body := int(hello[6])<<16 | int(hello[7])<<8 | int(hello[8]); body != len(hello)-9 {
		t.Errorf("the handshake claims %d bytes and carries %d", body, len(hello)-9)
	}
}

func TestMITMContractAmbientNamesItsRow(t *testing.T) {
	document := decodeMITM(t)
	for _, row := range document.Sessions {
		if row.Name != document.Ambient.Row {
			continue
		}
		if len(row.UpstreamSaw) != 1 {
			t.Fatalf("%s: %d requests reached the upstream", row.Name, len(row.UpstreamSaw))
		}
		for name, value := range document.Ambient.AddsWhenAbsent {
			values := row.UpstreamSaw[0].Headers[name]
			if len(values) == 0 || values[0] != value {
				t.Errorf("ambient says %s is %q and the row saw %v", name, value, values)
			}
		}
		if document.Ambient.UpstreamProto != row.UpstreamSaw[0].Proto {
			t.Errorf("ambient says the upstream spoke %q and the row saw %q",
				document.Ambient.UpstreamProto, row.UpstreamSaw[0].Proto)
		}
		return
	}
	t.Fatalf("ambient names %q and no session row has it", document.Ambient.Row)
}
