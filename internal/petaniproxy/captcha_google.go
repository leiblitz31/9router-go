package petaniproxy

// Free audio-captcha solver: Go port of Python speech_recognition's
// recognize_google (used by petani-proxy's webshare_hunter.py).
//
// It POSTs raw FLAC audio to Google's public speech-api/v2 endpoint with the
// library's embedded public key (gratis, no user API key involved) and parses
// the multi-line JSON response. This replaces the paid Whisper-API approach.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hajimehoshi/go-mp3"
)

const (
	// googleSpeechAPIKey is the public embedded key from the
	// speech_recognition Python library — free, safe to hardcode.
	googleSpeechAPIKey = "AIzaSyBOti4mM-6x9WDnZIjIeyEU21OpBXqWBgw"
	googleSpeechBase   = "http://www.google.com/speech-api/v2/recognize"

	// googleCaptchaTimeout bounds one recognition round-trip.
	googleCaptchaTimeout = 60 * time.Second

	// captchaTargetRate is the sample rate we always send; >= 8000 Hz is
	// required by the endpoint.
	captchaTargetRate = 16000
)

// GoogleCaptchaSolver implements WebshareCaptchaSolver with Google's free
// speech-api/v2 endpoint (same as speech_recognition.recognize_google).
type GoogleCaptchaSolver struct {
	language string
	client   *http.Client
	endpoint string
}

// GoogleCaptchaOption configures a GoogleCaptchaSolver.
type GoogleCaptchaOption func(*GoogleCaptchaSolver)

// WithLanguage sets the recognition language (default "en-US").
func WithLanguage(lang string) GoogleCaptchaOption {
	return func(s *GoogleCaptchaSolver) {
		if lang != "" {
			s.language = lang
		}
	}
}

// WithHTTPClient overrides the HTTP client (default: 60s timeout).
func WithHTTPClient(c *http.Client) GoogleCaptchaOption {
	return func(s *GoogleCaptchaSolver) {
		if c != nil {
			s.client = c
		}
	}
}

// NewGoogleCaptchaSolver builds a solver; no API key needed.
func NewGoogleCaptchaSolver(opts ...GoogleCaptchaOption) *GoogleCaptchaSolver {
	s := &GoogleCaptchaSolver{
		language: "en-US",
		client:   &http.Client{Timeout: googleCaptchaTimeout},
		endpoint: googleSpeechBase,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// SolveAudio implements WebshareCaptchaSolver: MP3 -> mono 16-bit PCM at
// 16 kHz -> FLAC -> Google speech-api/v2 -> transcript.
func (s *GoogleCaptchaSolver) SolveAudio(ctx context.Context, audioMP3 []byte) (WebshareAudioTranscript, error) {
	if len(audioMP3) == 0 {
		return WebshareAudioTranscript{}, errors.New("petaniproxy: SolveAudio: empty mp3 audio")
	}

	stereo, srcRate, err := decodeMP3Stereo16(audioMP3)
	if err != nil {
		return WebshareAudioTranscript{}, fmt.Errorf("petaniproxy: SolveAudio: %w", err)
	}
	mono := stereoToMono(stereo)
	resampled := resampleLinear(mono, srcRate, captchaTargetRate)

	flacBytes, err := encodeFLACMono16(resampled, captchaTargetRate)
	if err != nil {
		return WebshareAudioTranscript{}, fmt.Errorf("petaniproxy: SolveAudio: %w", err)
	}

	text, err := s.recognize(ctx, flacBytes, captchaTargetRate)
	if err != nil {
		return WebshareAudioTranscript{}, err
	}
	return WebshareAudioTranscript{
		Text:        text,
		DurationSec: float64(len(resampled)) / float64(captchaTargetRate),
	}, nil
}

// recognize POSTs raw FLAC bytes and returns the transcript.
func (s *GoogleCaptchaSolver) recognize(ctx context.Context, flacBytes []byte, rate int) (string, error) {
	u := s.endpoint + "?client=chromium&lang=" + url.QueryEscape(s.language) +
		"&key=" + googleSpeechAPIKey + "&pFilter=0"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(flacBytes))
	if err != nil {
		return "", fmt.Errorf("petaniproxy: google speech: %w", err)
	}
	req.Header.Set("Content-Type", fmt.Sprintf("audio/x-flac; rate=%d", rate))

	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("petaniproxy: google speech: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("petaniproxy: google speech: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("petaniproxy: google speech: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return parseGoogleSpeechTranscript(body)
}

// googleSpeechLine mirrors one JSON line of the multi-line response.
type googleSpeechLine struct {
	Result []struct {
		Alternative []struct {
			Transcript string `json:"transcript"`
		} `json:"alternative"`
	} `json:"result"`
}

// parseGoogleSpeechTranscript takes the first non-empty result's first
// alternative transcript, skipping blank lines. Lines that fail to parse are
// skipped; an error is returned only when no line yields a transcript.
func parseGoogleSpeechTranscript(body []byte) (string, error) {
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var parsed googleSpeechLine
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			continue
		}
		if len(parsed.Result) == 0 {
			continue
		}
		if len(parsed.Result[0].Alternative) == 0 {
			continue
		}
		if t := strings.TrimSpace(parsed.Result[0].Alternative[0].Transcript); t != "" {
			return t, nil
		}
	}
	return "", errors.New("petaniproxy: google speech returned no transcript")
}

// decodeMP3Stereo16 decodes MP3 bytes into interleaved stereo 16-bit samples
// plus the source sample rate (go-mp3 always outputs 16-bit LE stereo).
func decodeMP3Stereo16(data []byte) (stereo []int16, sampleRate int, err error) {
	dec, err := mp3.NewDecoder(bytes.NewReader(data))
	if err != nil {
		return nil, 0, fmt.Errorf("mp3 decode: %w", err)
	}
	raw, err := io.ReadAll(dec)
	if err != nil {
		return nil, 0, fmt.Errorf("mp3 decode: %w", err)
	}
	if len(raw)%4 != 0 {
		return nil, 0, fmt.Errorf("mp3 decode: odd pcm length %d", len(raw))
	}
	stereo = make([]int16, len(raw)/2)
	for i := range stereo {
		stereo[i] = int16(binary.LittleEndian.Uint16(raw[2*i:]))
	}
	return stereo, dec.SampleRate(), nil
}

// stereoToMono averages channel pairs into mono.
func stereoToMono(stereo []int16) []int16 {
	if len(stereo)%2 != 0 {
		stereo = stereo[:len(stereo)-1]
	}
	mono := make([]int16, len(stereo)/2)
	for i := range mono {
		mono[i] = int16((int32(stereo[2*i]) + int32(stereo[2*i+1])) / 2)
	}
	return mono
}

// resampleLinear resamples mono PCM from srcRate to dstRate with simple
// linear interpolation; returns the input unchanged when rates match.
func resampleLinear(mono []int16, srcRate, dstRate int) []int16 {
	if srcRate == dstRate || len(mono) == 0 {
		return mono
	}
	outLen := int(int64(len(mono)) * int64(dstRate) / int64(srcRate))
	if outLen <= 1 {
		return []int16{mono[0]}
	}
	out := make([]int16, outLen)
	for i := range out {
		pos := float64(i) * float64(len(mono)-1) / float64(outLen-1)
		lo := int(pos)
		hi := lo + 1
		if hi >= len(mono) {
			hi = len(mono) - 1
		}
		frac := pos - float64(lo)
		out[i] = int16(float64(mono[lo])*(1-frac) + float64(mono[hi])*frac)
	}
	return out
}
