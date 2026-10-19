package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"testing"
)

// The wire dump is committed and compared byte for byte, so it has to be a
// function of core and of the scripts -- nothing else. These checks keep the
// *sample set* honest rather than the writer:
//
//   - a row that lost its bytes would still compare equal to itself;
//   - a row that leaked the loopback port it listened on would make the dump
//     differ between runs, which is the one thing the gate cannot tolerate;
//   - a masked row that stopped masking would pass just as quietly, and a
//     bind address is the only thing in the dump that is not reproducible.

func generateWire(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if err := writeWireContract(&buffer); err != nil {
		t.Fatalf("writeWireContract: %v", err)
	}
	return buffer.Bytes()
}

func decodeWire(t *testing.T) wireContract {
	t.Helper()
	var document wireContract
	if err := json.Unmarshal(generateWire(t), &document); err != nil {
		t.Fatalf("the document does not decode: %v", err)
	}
	return document
}

func TestWireContractIsReproducible(t *testing.T) {
	first, second := generateWire(t), generateWire(t)
	if !bytes.Equal(first, second) {
		t.Fatal("two dumps of the same inputs differ")
	}
}

// TestWireContractHasNoEphemeralPort is the invariant the whole file rests on:
// every listener is on 127.0.0.1:0, so any port that reached the dump would
// differ on the next run. The upstream URLs are built from the stub's address
// at run time and never written; the SOCKS5 bind address is masked.
func TestWireContractHasNoEphemeralPort(t *testing.T) {
	port := regexp.MustCompile(`127\.0\.0\.1:\d+`)
	if found := port.FindAllString(string(generateWire(t)), -1); len(found) > 0 {
		t.Fatalf("the dump carries a loopback port: %v", found)
	}
}

// TestWireContractKeepsItsRows checks the sample set, not the writer: a table
// that lost its rows would replay perfectly against an empty document.
func TestWireContractKeepsItsRows(t *testing.T) {
	document := decodeWire(t)
	for table, least := range map[string]int{"socks5": 16, "http": 6, "upstream": 5} {
		var count int
		switch table {
		case "socks5":
			count = len(document.Socks5)
		case "http":
			count = len(document.HTTP)
		case "upstream":
			count = len(document.Upstream)
		}
		if count < least {
			t.Errorf("%s has %d rows, expected at least %d", table, count, least)
		}
	}
	for _, row := range document.Socks5 {
		if row.Name == "" {
			t.Error("a socks5 row has no name")
		}
		if row.Client == "" {
			t.Errorf("%s: the client script is empty", row.Name)
		}
		if row.Reply == "" {
			t.Errorf("%s: the reply is empty, which no session produces", row.Name)
		}
	}
	for _, row := range document.HTTP {
		if row.Name == "" || row.Request == "" {
			t.Errorf("%s: an http row lost its name or its request", row.Name)
		}
		if row.Status == 0 {
			t.Errorf("%s: no status was recorded", row.Name)
		}
		if row.Status == 200 && row.Head == "" {
			t.Errorf("%s: a hijacked answer has a head, and it is missing", row.Name)
		}
		if row.Status != 200 && row.Head != "" {
			t.Errorf("%s: a refused answer's head carries net/http's Date header", row.Name)
		}
	}
	for _, row := range document.Upstream {
		if row.Name == "" || row.Target == "" || row.Request == "" {
			t.Errorf("%s: an upstream row lost its name, target or request", row.Name)
		}
	}
}

// TestWireContractMasksEveryBindAddress checks the mask itself. A masked reply
// is a success reply whose BND.ADDR and BND.PORT are zero; ATYP survives so the
// address family, which is core's choice, is still pinned.
func TestWireContractMasksEveryBindAddress(t *testing.T) {
	document := decodeWire(t)
	var masked int
	for _, row := range document.Socks5 {
		if !row.MaskBind {
			continue
		}
		masked++
		reply, err := hex.DecodeString(row.Reply)
		if err != nil {
			t.Fatalf("%s: the reply is not hex: %v", row.Name, err)
		}
		if len(reply) < 2+4 {
			t.Fatalf("%s: the reply is too short to hold a request answer", row.Name)
		}
		request := reply[2:]
		if request[0] != 0x05 || request[1] != 0x00 {
			t.Errorf("%s: a masked row is not a success reply", row.Name)
			continue
		}
		var address int
		switch request[3] {
		case 0x01:
			address = 4
		case 0x04:
			address = 16
		default:
			t.Errorf("%s: a masked row has no bind address family", row.Name)
			continue
		}
		for _, byte := range reply[6 : 6+address+2] {
			if byte != 0 {
				t.Errorf("%s: the bind address was not masked", row.Name)
				break
			}
		}
	}
	if masked == 0 {
		t.Fatal("no row records a masked bind address")
	}
}
