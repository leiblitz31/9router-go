// Disposable-mailbox service and pure harvest helpers: a Go port of the
// DuckMailService class, save_to_autoclawpi_db path logic, and the token/OTP
// extraction helpers from petani-proxy core/zai_farm.py.
//
// Porting notes (Python -> Go):
//   - requests.* becomes an injected *http.Client so tests can point the
//     service at httptest servers; per-request timeouts mirror Python's.
//   - wait_for_otp's 3s poll loop honours context cancellation.
//   - save_to_autoclawpi_db needs SQLite (no stdlib driver), so this file only
//     ports the candidate-path resolution (ZaiAutoClawDBPaths) and documents
//     the schema; persistence itself is the ZaiAccountStore seam.
package petaniproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// ZaiMailAPIBases mirrors DuckMailService.api_bases, tried in order.
var ZaiMailAPIBases = []string{
	"https://api.duckmail.sbs",
	"https://api.mail.tm",
}

// ZaiFallbackMailDomain is used when every mail API base fails, mirroring the
// vercelspace.shop fallback in create_mailbox (mailbox without an API token,
// so OTP waiting is skipped).
const ZaiFallbackMailDomain = "vercelspace.shop"

// zaiOTPRe mirrors re.search(r"\b(\d{6})\b", ...) in wait_for_otp.
var zaiOTPRe = regexp.MustCompile(`\b(\d{6})\b`)

const (
	zaiLowerDigits = "abcdefghijklmnopqrstuvwxyz0123456789"
	zaiLetters     = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
)

// ZaiDuckMailService is a Go port of the Python DuckMailService: it creates a
// disposable mailbox through a mail.tm-compatible API and polls it for the
// 6-digit signup OTP.
type ZaiDuckMailService struct {
	// Bases are tried in order; Base is the one that succeeded.
	Bases []string
	Base  string
	// Email, Password and Token describe the created mailbox. Token is empty
	// when the fallback domain was used (no API access).
	Email    string
	Password string
	Token    string
	// HTTP performs the API calls; tests inject an httptest-backed client.
	HTTP *http.Client
}

// NewZaiDuckMailService builds a service with the default API bases and a
// 15s-timeout HTTP client.
func NewZaiDuckMailService() *ZaiDuckMailService {
	return &ZaiDuckMailService{
		Bases: append([]string(nil), ZaiMailAPIBases...),
		Base:  ZaiMailAPIBases[0],
		HTTP:  &http.Client{Timeout: 15 * time.Second},
	}
}

// zaiMailDomain is one entry of GET /domains' hydra:member list.
type zaiMailDomain struct {
	Domain   string `json:"domain"`
	IsActive *bool  `json:"isActive"`
}

// zaiMailMessage is one entry of GET /messages' hydra:member list.
type zaiMailMessage struct {
	ID      string `json:"id"`
	Subject string `json:"subject"`
	Intro   string `json:"intro"`
}

// zaiHydraList decodes {"hydra:member": [...]} envelopes.
type zaiHydraList[T any] struct {
	Member []T `json:"hydra:member"`
}

// CreateMailbox creates a disposable mailbox, mirroring create_mailbox: for
// each API base, fetch /domains, pick a random active domain, register a random
// zai_<9 chars> address, then fetch a token. Falls back to a
// <random>@vercelspace.shop address with no token when all bases fail.
func (s *ZaiDuckMailService) CreateMailbox(ctx context.Context) (email, password string, err error) {
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	for _, base := range s.Bases {
		domains, derr := s.fetchDomains(ctx, base)
		if derr != nil || len(domains) == 0 {
			continue
		}
		domain := domains[rng.Intn(len(domains))]
		email = ZaiRandomUser(rng) + "@" + domain
		password = ZaiRandomPassword(rng)
		if cerr := s.registerAccount(ctx, base, email, password); cerr != nil {
			continue
		}
		token, terr := s.fetchToken(ctx, base, email, password)
		if terr != nil {
			continue
		}
		s.Base, s.Email, s.Password, s.Token = base, email, password, token
		return email, password, nil
	}
	email = ZaiRandomUser(rng) + "@" + ZaiFallbackMailDomain
	password = ZaiRandomPassword(rng)
	s.Email, s.Password, s.Token = email, password, ""
	return email, password, nil
}

// WaitForOTP polls GET /messages for a 6-digit code, mirroring wait_for_otp:
// it scans subject+intro of each message, then fetches the full message text.
// Returns "" when the timeout expires or no API token exists.
func (s *ZaiDuckMailService) WaitForOTP(ctx context.Context, timeout time.Duration) (string, error) {
	if s.Token == "" {
		return "", nil
	}
	deadline := time.Now().Add(timeout)
	for {
		msgs, err := s.fetchMessages(ctx)
		if err == nil {
			for _, m := range msgs {
				if code, ok := ZaiExtractOTPCode(m.Subject + " " + m.Intro); ok {
					return code, nil
				}
				if m.ID == "" {
					continue
				}
				if body, berr := s.fetchMessageText(ctx, m.ID); berr == nil {
					if code, ok := ZaiExtractOTPCode(body); ok {
						return code, nil
					}
				}
			}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return "", nil
		}
		wait := 3 * time.Second
		if remaining < wait {
			wait = remaining
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(wait):
		}
	}
}

func (s *ZaiDuckMailService) fetchDomains(ctx context.Context, base string) ([]string, error) {
	_, body, err := zaiDoJSON(ctx, s.HTTP, http.MethodGet, base+"/domains", nil, nil)
	if err != nil {
		return nil, err
	}
	var list zaiHydraList[zaiMailDomain]
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("petaniproxy: decode domains: %w", err)
	}
	var active []string
	for _, d := range list.Member {
		// Python: d.get("isActive", True) — missing means active.
		if d.IsActive != nil && !*d.IsActive {
			continue
		}
		if d.Domain != "" {
			active = append(active, d.Domain)
		}
	}
	return active, nil
}

func (s *ZaiDuckMailService) registerAccount(ctx context.Context, base, email, password string) error {
	payload := map[string]string{"address": email, "password": password}
	resp, _, err := zaiDoJSON(ctx, s.HTTP, http.MethodPost, base+"/accounts", payload,
		map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("petaniproxy: POST /accounts -> %d", resp.StatusCode)
	}
	return nil
}

func (s *ZaiDuckMailService) fetchToken(ctx context.Context, base, email, password string) (string, error) {
	payload := map[string]string{"address": email, "password": password}
	resp, body, err := zaiDoJSON(ctx, s.HTTP, http.MethodPost, base+"/token", payload,
		map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("petaniproxy: POST /token -> %d", resp.StatusCode)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("petaniproxy: decode token: %w", err)
	}
	return out.Token, nil
}

func (s *ZaiDuckMailService) fetchMessages(ctx context.Context) ([]zaiMailMessage, error) {
	_, body, err := zaiDoJSON(ctx, s.HTTP, http.MethodGet, s.Base+"/messages", nil,
		map[string]string{"Authorization": "Bearer " + s.Token})
	if err != nil {
		return nil, err
	}
	var list zaiHydraList[zaiMailMessage]
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("petaniproxy: decode messages: %w", err)
	}
	return list.Member, nil
}

func (s *ZaiDuckMailService) fetchMessageText(ctx context.Context, id string) (string, error) {
	_, body, err := zaiDoJSON(ctx, s.HTTP, http.MethodGet, s.Base+"/messages/"+id, nil,
		map[string]string{"Authorization": "Bearer " + s.Token})
	if err != nil {
		return "", err
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("petaniproxy: decode message: %w", err)
	}
	return out.Text, nil
}

// zaiDoJSON performs one JSON API call and returns the response plus body.
func zaiDoJSON(ctx context.Context, client *http.Client, method, url string, payload any, headers map[string]string) (*http.Response, []byte, error) {
	var rdr io.Reader
	if payload != nil {
		buf, err := json.Marshal(payload)
		if err != nil {
			return nil, nil, fmt.Errorf("petaniproxy: marshal payload: %w", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, nil, fmt.Errorf("petaniproxy: build request: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("petaniproxy: %s %s: %w", method, url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return nil, nil, fmt.Errorf("petaniproxy: read body: %w", err)
	}
	return resp, body, nil
}

// ZaiExtractOTPCode returns the first 6-digit code in text, mirroring
// re.search(r"\b(\d{6})\b", text) in wait_for_otp.
func ZaiExtractOTPCode(text string) (string, bool) {
	m := zaiOTPRe.FindStringSubmatch(text)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// ZaiSelectTokens picks access/refresh tokens from the signup tab's cookies,
// mirroring run_zai_farm: access = token|jwt|access_token cookie, else a
// generated "Bearer zai_<6 digits>" placeholder; refresh = refresh_token
// cookie, else the access token.
func ZaiSelectTokens(cookies map[string]string, rng *rand.Rand) (access, refresh string) {
	for _, k := range []string{"token", "jwt", "access_token"} {
		if v := cookies[k]; v != "" {
			access = v
			break
		}
	}
	if access == "" {
		if rng == nil {
			rng = rand.New(rand.NewSource(time.Now().UnixNano()))
		}
		access = fmt.Sprintf("Bearer zai_%d", 100000+rng.Intn(900000))
	}
	if v := cookies["refresh_token"]; v != "" {
		refresh = v
	} else {
		refresh = access
	}
	return access, refresh
}

// ZaiRandomUser builds the "zai_<9 lowercase/digits>" local part used for both
// the mailbox address and (with a domain) the email.
func ZaiRandomUser(rng *rand.Rand) string {
	return "zai_" + zaiRandomString(rng, zaiLowerDigits, 9)
}

// ZaiRandomPassword builds the "ZaiFarm<8 letters>!@" mailbox password.
func ZaiRandomPassword(rng *rand.Rand) string {
	return "ZaiFarm" + zaiRandomString(rng, zaiLetters, 8) + "!@"
}

// ZaiDisplayName builds the "Zai User <100-999>" full name for the signup form.
func ZaiDisplayName(rng *rand.Rand) string {
	if rng == nil {
		rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	return fmt.Sprintf("Zai User %d", 100+rng.Intn(900))
}

func zaiRandomString(rng *rand.Rand, charset string, n int) string {
	if rng == nil {
		rng = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = charset[rng.Intn(len(charset))]
	}
	return string(b)
}

// ZaiAutoClawDBPaths returns the candidate AutoClawPi database locations in the
// order save_to_autoclawpi_db tries them: $AUTOCLAWPI_DB first when set, then
// ~/.autoclawpi/autoclawpi.db, <moduleRoot>/output/autoclawpi.db, and
// <parent-of-moduleRoot>/autoclawpi/data/autoclawpi.db. A production
// ZaiAccountStore iterates these, creates the accounts table
// (name, access_token, refresh_token, provider='zai', email,
// device_id='autoclawpi-<epoch-ms>', points=10540, active=1, created_at), and
// inserts the harvested account.
func ZaiAutoClawDBPaths(moduleRoot string) []string {
	var paths []string
	if env := os.Getenv("AUTOCLAWPI_DB"); env != "" {
		paths = append(paths, env)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		paths = append(paths, filepath.Join(home, ".autoclawpi", "autoclawpi.db"))
	}
	paths = append(paths,
		filepath.Join(moduleRoot, "output", "autoclawpi.db"),
		filepath.Join(filepath.Dir(moduleRoot), "autoclawpi", "data", "autoclawpi.db"),
	)
	return paths
}
