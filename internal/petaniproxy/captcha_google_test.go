package petaniproxy

import (
	"context"
	"encoding/binary"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseGoogleSpeechTranscript(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    string
		wantErr bool
	}{
		{
			name: "multi-line with blanks picks first transcript",
			body: "\n" +
				`{"result":[]}` + "\n" +
				`{"result":[{"alternative":[{"transcript":"one two three","confidence":0.9}]}]}` + "\n" +
				`{"result":[{"alternative":[{"transcript":"ignored"}]}]}` + "\n",
			want: "one two three",
		},
		{
			name:    "blank lines in the middle are skipped",
			body:    "\n\n" + `{"result":[{"alternative":[{"transcript":"hello"}]}]}` + "\n\n",
			want:    "hello",
			wantErr: false,
		},
		{
			name:    "malformed json line is skipped",
			body:    "not json\n" + `{"result":[{"alternative":[{"transcript":"ok"}]}]}` + "\n",
			want:    "ok",
			wantErr: false,
		},
		{
			name:    "no alternative yields clear error",
			body:    `{"result":[{"alternative":[]}]}` + "\n",
			wantErr: true,
		},
		{
			name:    "empty result everywhere yields clear error",
			body:    `{"result":[]}` + "\n" + `{"result":[]}` + "\n",
			wantErr: true,
		},
		{
			name:    "empty body yields clear error",
			body:    "",
			wantErr: true,
		},
		{
			name:    "whitespace-only transcript is not a transcript",
			body:    `{"result":[{"alternative":[{"transcript":"   "}]}]}` + "\n",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseGoogleSpeechTranscript([]byte(tt.body))
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseGoogleSpeechTranscript() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if err == nil || !strings.Contains(err.Error(), "no transcript") {
					t.Fatalf("error = %v, want message containing %q", err, "no transcript")
				}
				return
			}
			if got != tt.want {
				t.Errorf("parseGoogleSpeechTranscript() = %q, want %q", got, tt.want)
			}
		})
	}
}

// spyTransport records the outgoing request and redirects it to the test server.
type spyTransport struct {
	lastReq *http.Request
	base    http.RoundTripper
}

func (s *spyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.lastReq = req
	return s.base.RoundTrip(req)
}

func TestGoogleCaptchaSolverRecognizeHTTP(t *testing.T) {
	const mockTranscript = "seven four nine"

	var gotContentType, gotQuery string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		gotQuery = r.URL.RawQuery
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "\n"+`{"result":[]}`+"\n"+`{"result":[{"alternative":[{"transcript":"`+mockTranscript+`"}]}]}`+"\n")
	}))
	defer srv.Close()

	solver := NewGoogleCaptchaSolver()
	solver.endpoint = srv.URL // same package: point at the mock server

	// Feed synthetic PCM through the real FLAC encoder so the POST body is
	// exactly what production sends.
	samples := make([]int16, captchaTargetRate) // 1 second of sine
	for i := range samples {
		samples[i] = int16(math.Sin(float64(i)*440*2*math.Pi/captchaTargetRate) * 20000)
	}
	flacBytes, err := encodeFLACMono16(samples, captchaTargetRate)
	if err != nil {
		t.Fatalf("encodeFLACMono16: %v", err)
	}

	got, err := solver.recognize(context.Background(), flacBytes, captchaTargetRate)
	if err != nil {
		t.Fatalf("recognize: %v", err)
	}
	if got != mockTranscript {
		t.Errorf("recognize() = %q, want %q", got, mockTranscript)
	}
	if !strings.Contains(gotContentType, "audio/x-flac") {
		t.Errorf("Content-Type = %q, want it to contain audio/x-flac", gotContentType)
	}
	if !strings.Contains(gotContentType, "rate=16000") {
		t.Errorf("Content-Type = %q, want rate=16000", gotContentType)
	}
	if len(gotBody) == 0 || string(gotBody[:4]) != "fLaC" {
		t.Errorf("body len=%d, magic=%q; want non-empty FLAC stream", len(gotBody), gotBody[:min(4, len(gotBody))])
	}
	for _, want := range []string{"client=chromium", "lang=en-US", "key=" + googleSpeechAPIKey, "pFilter=0"} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("query %q missing %q", gotQuery, want)
		}
	}
}

func TestGoogleCaptchaSolverRecognizeHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "nope")
	}))
	defer srv.Close()

	solver := NewGoogleCaptchaSolver()
	solver.endpoint = srv.URL
	if _, err := solver.recognize(context.Background(), []byte("fLaC"), captchaTargetRate); err == nil {
		t.Fatal("recognize() on 400: want error, got nil")
	}
}

func TestNewGoogleCaptchaSolverOptions(t *testing.T) {
	def := NewGoogleCaptchaSolver()
	if def.language != "en-US" {
		t.Errorf("default language = %q, want en-US", def.language)
	}
	if def.client == nil {
		t.Error("default client is nil")
	}

	// WithLanguage must change the lang query parameter actually sent.
	var gotLang string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLang = r.URL.Query().Get("lang")
		_, _ = io.WriteString(w, `{"result":[{"alternative":[{"transcript":"x"}]}]}`+"\n")
	}))
	defer srv.Close()

	spy := &spyTransport{base: http.DefaultTransport}
	custom := &http.Client{Transport: spy}
	solver := NewGoogleCaptchaSolver(WithLanguage("id-ID"), WithHTTPClient(custom))
	if solver.language != "id-ID" {
		t.Errorf("language = %q, want id-ID", solver.language)
	}
	solver.endpoint = srv.URL
	if _, err := solver.recognize(context.Background(), []byte("fLaC"), captchaTargetRate); err != nil {
		t.Fatalf("recognize: %v", err)
	}
	if gotLang != "id-ID" {
		t.Errorf("sent lang = %q, want id-ID", gotLang)
	}
	if spy.lastReq == nil {
		t.Fatal("custom http client was not used")
	}

	// Nil / empty options must not clobber defaults.
	s2 := NewGoogleCaptchaSolver(WithLanguage(""), WithHTTPClient(nil))
	if s2.language != "en-US" || s2.client == nil {
		t.Errorf("empty options clobbered defaults: %+v", s2)
	}
}

func TestSolveAudioInvalidMP3(t *testing.T) {
	solver := NewGoogleCaptchaSolver()
	if _, err := solver.SolveAudio(context.Background(), []byte("not an mp3")); err == nil {
		t.Fatal("SolveAudio(garbage): want error, got nil")
	}
	if _, err := solver.SolveAudio(context.Background(), nil); err == nil {
		t.Fatal("SolveAudio(nil): want error, got nil")
	}
}

func TestStereoToMonoAndResample(t *testing.T) {
	stereo := []int16{1000, 3000, -1000, -3000}
	mono := stereoToMono(stereo)
	if len(mono) != 2 || mono[0] != 2000 || mono[1] != -2000 {
		t.Fatalf("stereoToMono = %v, want [2000 -2000]", mono)
	}

	same := resampleLinear(mono, 16000, 16000)
	if len(same) != len(mono) {
		t.Fatalf("resample same rate changed length: %d", len(same))
	}

	down := resampleLinear([]int16{0, 1000, 2000, 3000}, 16000, 8000)
	if len(down) != 2 {
		t.Fatalf("resample 16k->8k len = %d, want 2", len(down))
	}
	up := resampleLinear([]int16{0, 1000}, 8000, 16000)
	if len(up) != 4 {
		t.Fatalf("resample 8k->16k len = %d, want 4", len(up))
	}
}

func TestEncodeFLACMono16Structure(t *testing.T) {
	const n = captchaTargetRate * 2 // 2 seconds, forces multiple frames
	samples := make([]int16, n)
	for i := range samples {
		samples[i] = int16(math.Sin(float64(i)*880*2*math.Pi/captchaTargetRate) * 15000)
	}

	enc, err := encodeFLACMono16(samples, captchaTargetRate)
	if err != nil {
		t.Fatalf("encodeFLACMono16: %v", err)
	}
	if string(enc[:4]) != "fLaC" {
		t.Fatalf("magic = %q, want fLaC", enc[:4])
	}
	// STREAMINFO block header: last-block flag (bit 7) + type 0, length 34.
	if enc[4] != 0x80 || enc[5] != 0 || enc[6] != 0 || enc[7] != 0x22 {
		t.Fatalf("streaminfo header = % x, want 80 00 00 22", enc[4:8])
	}
	si := enc[8:42]
	sr := uint32(si[10])<<12 | uint32(si[11])<<4 | uint32(si[12])>>4
	if sr != captchaTargetRate {
		t.Errorf("streaminfo sample rate = %d, want %d", sr, captchaTargetRate)
	}
	if ch := (si[12] >> 1) & 0x7; ch != 0 {
		t.Errorf("streaminfo channels-1 = %d, want 0 (mono)", ch)
	}
	if total := binary.BigEndian.Uint64(si[10:18]) & 0xFFFFFFFFF; total != uint64(n) {
		t.Errorf("streaminfo total samples = %d, want %d", total, n)
	}
	// First frame must start with the sync word FF F8.
	rest := enc[42:]
	if len(rest) < 2 || rest[0] != 0xFF || rest[1] != 0xF8 {
		t.Fatalf("first frame header = % x, want ff f8 ...", rest[:min(2, len(rest))])
	}

	// Walk every frame: verify CRC-8 of each header and CRC-16 of each frame.
	off := 42
	frames := 0
	for off < len(enc) {
		start := off
		if enc[off] != 0xFF || enc[off+1] != 0xF8 {
			t.Fatalf("frame %d: bad sync at offset %d", frames, off)
		}
		off += 4 // 32-bit fixed header
		// UTF-8 coded frame number: continuation bytes have high bit set.
		for enc[off]&0x80 != 0 {
			off++
		}
		off++               // last UTF-8 byte
		bsOff := off        // 16-bit blocksize-1 lives here
		hdrEnd := bsOff + 2 // + optional 16-bit sample rate
		rateCode := enc[start+2] & 0x0F
		var rate uint16
		if rateCode == 0xB {
			// 16-bit explicit sample rate in Hz.
			rate = binary.BigEndian.Uint16(enc[hdrEnd : hdrEnd+2])
			hdrEnd += 2
		} else {
			rate = uint16(captchaTargetRate) // literal code path in this test
		}
		if flacCRC8(enc[start:hdrEnd]) != enc[hdrEnd] {
			t.Fatalf("frame %d: CRC-8 mismatch", frames)
		}
		off = hdrEnd + 1
		blockSize := int(binary.BigEndian.Uint16(enc[bsOff:bsOff+2])) + 1
		if int(rate) != captchaTargetRate {
			t.Fatalf("frame %d: sample rate = %d, want %d", frames, rate, captchaTargetRate)
		}
		off += 1 + blockSize*2 // subframe header byte + 16-bit samples
		crc := binary.BigEndian.Uint16(enc[off : off+2])
		if flacCRC16(enc[start:off]) != crc {
			t.Fatalf("frame %d: CRC-16 mismatch", frames)
		}
		off += 2
		frames++
	}
	if off != len(enc) {
		t.Fatalf("frame walk ended at %d, stream len %d", off, len(enc))
	}
	wantFrames := (n + flacMaxBlock - 1) / flacMaxBlock
	if frames != wantFrames {
		t.Errorf("frames = %d, want %d", frames, wantFrames)
	}
}

func TestEncodeFLACMono16Errors(t *testing.T) {
	if _, err := encodeFLACMono16(nil, 16000); err == nil {
		t.Error("empty samples: want error, got nil")
	}
	if _, err := encodeFLACMono16(make([]int16, 100), 0); err == nil {
		t.Error("zero sample rate: want error, got nil")
	}
	if _, err := encodeFLACMono16(make([]int16, 10), 16000); err == nil {
		t.Error("10 samples: want error, got nil")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
