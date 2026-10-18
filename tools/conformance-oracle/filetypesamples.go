package main

// The corpus the filetype contract is computed from: every signature of the
// WHATWG table, every boundary of the algorithm that reads one.
//
// This half is the portable one. `NameType` is not: it resolves through the
// host's MIME database (see filetype.go in the port), so its answers differ
// between a Mac with `/etc/apache2/mime.types` and a Linux box with the
// FreeDesktop `globs2` file -- and a committed snapshot of them would be a
// snapshot of one host. The port reads the same files, and its parsers are
// tested against fixture text instead.

// A signature at the very end of the sample, a byte of it one past the end, the
// short payloads that make the mp4 box walk and the html tag check bail out --
// those are the cases where an off-by-one in the port is silent.

// sniffRow carries the bytes, not a description of them: the port rebuilds the
// sample and runs its own sniffer over it.

func repeat(fill byte, count int) []byte {
	data := make([]byte, count)
	for i := range data {
		data[i] = fill
	}
	return data
}

func appendTo(head []byte, parts ...[]byte) []byte {
	data := append([]byte{}, head...)
	for _, part := range parts {
		data = append(data, part...)
	}
	return data
}

func filetypeCorpus() [][]byte {
	samples := [][]byte{
		nil,
		[]byte("plain text\n"),
		[]byte("  \t\r\n text after whitespace"),
		[]byte("<!DOCTYPE HTML><html></html>"),
		[]byte("<!doctype html><html></html>"),
		[]byte("<HTML>"),
		[]byte("<html>"),
		[]byte("<HTMLX"),    // the tag must be terminated
		[]byte("<HTML X"),   // a space terminates
		[]byte("   <html>"), // leading whitespace is skipped
		[]byte("<?xml version=\"1.0\"?>"),
		[]byte("  <?xml"),   // skipWS
		[]byte("<?xmlnope"), // not xml: the mask has no terminator
		[]byte("<body class=\"x\">"),
		[]byte("<b>bold</b>"),
		[]byte("<h1>title</h1>"),
		[]byte("<!-- comment -->"),
		[]byte("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n"),
		[]byte("%!PS-Adobe-3.0\n"),
		appendTo([]byte{0xfe, 0xff}, repeat(0x00, 8)),
		appendTo([]byte{0xff, 0xfe}, repeat(0x00, 8)),
		appendTo([]byte{0xef, 0xbb, 0xbf}, []byte("text")),
		appendTo([]byte{0x00, 0x00, 0x01, 0x00}, repeat(0x01, 8)),
		appendTo([]byte{0x00, 0x00, 0x02, 0x00}, repeat(0x01, 8)),
		[]byte("BM"),                             // a document that starts with BM
		appendTo([]byte("BM"), repeat(0x00, 56)), // a real BMP header
		appendTo([]byte("GIF87a"), repeat(0x01, 8)),
		appendTo([]byte("GIF89a"), repeat(0x01, 8)),
		appendTo([]byte("GIF88a"), repeat(0x01, 8)), // no such signature
		appendTo([]byte("RIFF"), repeat(0x00, 4), []byte("WEBPVP"), repeat(0x01, 4)),
		appendTo([]byte("RIFF"), repeat(0x00, 4), []byte("WEBPVX"), repeat(0x01, 4)),
		appendTo([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, repeat(0x00, 8)),
		appendTo([]byte{0xff, 0xd8, 0xff, 0xe0}, repeat(0x00, 8)),
		appendTo([]byte("FORM"), repeat(0x00, 4), []byte("AIFF"), repeat(0x00, 4)),
		appendTo([]byte("ID3"), repeat(0x00, 8)),
		appendTo([]byte("OggS\x00"), repeat(0x00, 8)),
		appendTo([]byte("MThd"), repeat(0x00, 3), []byte{0x06}),
		appendTo([]byte("RIFF"), repeat(0x00, 4), []byte("AVI "), repeat(0x00, 4)),
		appendTo([]byte("RIFF"), repeat(0x00, 4), []byte("WAVE"), repeat(0x00, 4)),
		[]byte("ftypmp4"), // too short for the box walk
		appendTo([]byte{0x00, 0x00, 0x00, 0x20}, []byte("ftypisom"), repeat(0x00, 4), []byte("mp41"), repeat(0x42, 32)),
		appendTo([]byte{0x00, 0x00, 0x00, 0x1f}, []byte("ftypisom"), []byte("mp41"), repeat(0x42, 8)), // size not a multiple of 4
		appendTo([]byte{0x00, 0x00, 0x00, 0x40}, []byte("ftypisom"), []byte("mp41"), repeat(0x42, 8)), // box larger than the sample
		appendTo([]byte{0x00, 0x00, 0x00, 0x20}, []byte("ftypjunk"), repeat(0x00, 4), []byte("mp41"), repeat(0x42, 32)),
		appendTo([]byte{0x00, 0x00, 0x00, 0x20}, []byte("ftypisom"), repeat(0x00, 4), []byte("avc1"), repeat(0x42, 32)), // no mp4 brand
		appendTo([]byte{0x1a, 0x45, 0xdf, 0xa3}, repeat(0x01, 8)),
		appendTo(repeat(0x00, 34), []byte("LP")),
		appendTo(repeat(0x00, 34), []byte("LP\x00\x00")), // the mask is two bytes: a third is not matched
		appendTo([]byte{0x00, 0x01, 0x00, 0x00}, repeat(0x01, 8)),
		appendTo([]byte("OTTO"), repeat(0x01, 8)),
		appendTo([]byte("ttcf"), repeat(0x01, 8)),
		appendTo([]byte("wOFF"), repeat(0x01, 8)),
		appendTo([]byte("wOF2"), repeat(0x01, 8)),
		appendTo([]byte{0x1f, 0x8b, 0x08}, repeat(0x01, 8)),
		appendTo([]byte("PK\x03\x04"), repeat(0x00, 24)),
		appendTo([]byte("Rar!\x1a\x07\x00"), repeat(0x01, 8)),
		appendTo([]byte("Rar!\x1a\x07\x01\x00"), repeat(0x01, 8)),
		appendTo([]byte{0x00, 0x61, 0x73, 0x6d}, repeat(0x01, 8)),
		// Text and the control bytes that end it.
		{0x00},
		{0x08}, {0x09}, {0x0b}, {0x0d}, {0x0e}, {0x1a}, {0x1b}, {0x1c}, {0x1f},
		{0x7f}, {0x80}, {0xff},
		[]byte("valid text \xe4\xb8\xad\xe6\x96\x87"),
	}
	// The 512-byte cutoff: a signature that ends exactly at the boundary, and
	// one byte of it that falls past the boundary and must not be seen.
	samples = append(samples,
		appendTo(repeat('a', 504), []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}),
		appendTo(repeat('a', 505), []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}),
		appendTo(repeat('a', 508), []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}),
		appendTo(repeat('a', 511), []byte("B")),
		appendTo(repeat('a', 512), []byte("BM")),
	)
	return samples
}
