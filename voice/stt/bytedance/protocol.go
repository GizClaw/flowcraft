package bytedance

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"io"
)

// Wire protocol constants for the ByteDance SAUC ASR binary framing.
const (
	protoVersion = 0b_0001_0000_0000_0000_0000_0000_0000_0000
	protoHdrSize = 0b_0000_0001_0000_0000_0000_0000_0000_0000

	msgTypeMask     = 0b_0000_0000_1111_0000_0000_0000_0000_0000
	msgTypeFlagMask = 0b_0000_0000_0000_1111_0000_0000_0000_0000
	compressionMask = 0b_0000_0000_0000_0000_0000_1111_0000_0000

	msgTypeClientFull      = 0b_0000_0000_0001_0000_0000_0000_0000_0000
	msgTypeClientAudioOnly = 0b_0000_0000_0010_0000_0000_0000_0000_0000
	msgTypeServerFull      = 0b_0000_0000_1001_0000_0000_0000_0000_0000
	msgTypeError           = 0b_0000_0000_1111_0000_0000_0000_0000_0000

	flagNoSeq       = 0b_0000_0000_0000_0000_0000_0000_0000_0000
	flagPositiveSeq = 0b_0000_0000_0000_0001_0000_0000_0000_0000
	flagNoSeqEOF    = 0b_0000_0000_0000_0010_0000_0000_0000_0000
	flagNegativeSeq = 0b_0000_0000_0000_0011_0000_0000_0000_0000
	flagEventID     = 0b_0000_0000_0000_0100_0000_0000_0000_0000

	serializationJSON = 0b_0000_0000_0000_0000_0001_0000_0000_0000
	compressionGZip   = 0b_0000_0000_0000_0000_0000_0001_0000_0000
)

type frameHeader struct {
	toc         uint32
	seq         uint32
	code        uint32
	payloadSize uint32
}

func (h frameHeader) msgType() uint32     { return h.toc & msgTypeMask }
func (h frameHeader) msgTypeFlag() uint32 { return h.toc & msgTypeFlagMask }
func (h frameHeader) isGZip() bool        { return h.toc&compressionMask == compressionGZip }

func unpackFrame(b []byte) (frameHeader, []byte, error) {
	if len(b) < 4 {
		return frameHeader{}, nil, io.ErrUnexpectedEOF
	}

	hdr := frameHeader{toc: binary.BigEndian.Uint32(b)}
	b = b[4:]

	switch hdr.msgTypeFlag() {
	case flagPositiveSeq, flagNegativeSeq:
		if len(b) < 4 {
			return frameHeader{}, nil, io.ErrUnexpectedEOF
		}
		hdr.seq = binary.BigEndian.Uint32(b)
		b = b[4:]
	case flagEventID:
		if len(b) < 4 {
			return frameHeader{}, nil, io.ErrUnexpectedEOF
		}
		hdr.seq = binary.BigEndian.Uint32(b)
		b = b[4:]
	}

	switch hdr.msgType() {
	case msgTypeServerFull:
		if len(b) < 4 {
			return frameHeader{}, nil, io.ErrUnexpectedEOF
		}
		hdr.payloadSize = binary.BigEndian.Uint32(b)
		b = b[4:]
	case msgTypeError:
		if len(b) < 8 {
			return frameHeader{}, nil, io.ErrUnexpectedEOF
		}
		hdr.code = binary.BigEndian.Uint32(b)
		hdr.payloadSize = binary.BigEndian.Uint32(b[4:])
		b = b[8:]
	}

	if hdr.payloadSize > 0 {
		if uint32(len(b)) < hdr.payloadSize {
			return frameHeader{}, nil, io.ErrUnexpectedEOF
		}
		b = b[:hdr.payloadSize]
	}

	return hdr, b, nil
}

func packMessage(msgType uint32, seq int32, data []byte, compress bool) ([]byte, error) {
	var err error
	if compress {
		data, err = gzipCompress(data)
		if err != nil {
			return nil, err
		}
	}

	header := protoVersion | protoHdrSize | msgType | serializationJSON
	if compress {
		header |= compressionGZip
	}
	if seq < 0 {
		header |= flagNegativeSeq
	} else {
		header |= flagPositiveSeq
	}

	buf := make([]byte, 4+4+4+len(data))
	binary.BigEndian.PutUint32(buf[0:], header)
	binary.BigEndian.PutUint32(buf[4:], uint32(seq))
	binary.BigEndian.PutUint32(buf[8:], uint32(len(data)))
	copy(buf[12:], data)
	return buf, nil
}

// --- ASR request/response types ---

type asrRequestPayload struct {
	User    asrUserMeta    `json:"user"`
	Audio   asrAudioMeta   `json:"audio"`
	Request asrRequestMeta `json:"request"`
}

type asrUserMeta struct {
	UID string `json:"uid,omitempty"`
}

type asrAudioMeta struct {
	Format   string `json:"format,omitempty"`
	Codec    string `json:"codec,omitempty"`
	Rate     int    `json:"rate,omitempty"`
	Bits     int    `json:"bits,omitempty"`
	Channel  int    `json:"channel,omitempty"`
	Language string `json:"language,omitempty"`
}

type asrRequestMeta struct {
	ModelName       string `json:"model_name,omitempty"`
	EnableITN       bool   `json:"enable_itn,omitempty"`
	EnablePUNC      bool   `json:"enable_punc,omitempty"`
	ShowUtterances  bool   `json:"show_utterances"`
	EnableNonstream bool   `json:"enable_nonstream"`
	EndWindowSize   int    `json:"end_window_size,omitempty"`
	ResultType      string `json:"result_type"`
}

type asrResponsePayload struct {
	Result struct {
		Text       string         `json:"text"`
		Utterances []asrUtterance `json:"utterances,omitempty"`
	} `json:"result"`
	Error string `json:"error,omitempty"`
}

type asrUtterance struct {
	Definite bool   `json:"definite"`
	Text     string `json:"text"`
}

type asrResponse struct {
	isLast  bool
	code    uint32
	payload *asrResponsePayload
}

func decodeASRResponse(hdr frameHeader, data []byte) (*asrResponse, error) {
	resp := &asrResponse{}

	switch hdr.msgTypeFlag() {
	case flagNegativeSeq, flagNoSeqEOF:
		resp.isLast = true
	}

	if hdr.msgType() == msgTypeError {
		resp.code = hdr.code
		if len(data) > 0 {
			resp.payload = &asrResponsePayload{}
			if err := json.Unmarshal(data, resp.payload); err != nil {
				resp.payload.Error = string(data)
			}
		}
		return resp, nil
	}

	if len(data) > 0 {
		resp.payload = &asrResponsePayload{}
		if err := json.Unmarshal(data, resp.payload); err != nil {
			return nil, err
		}
	}

	return resp, nil
}

// --- gzip helpers ---

func gzipCompress(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func gzipDecompress(b []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(r)
}
