package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	oauthTokenURL                          = "https://platform.claude.com/v1/oauth/token"
	oauthClientID                          = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	oauthRefreshTokenFromClaudeCredentials = "__FROM_CLAUDE_CREDENTIALS__"
)

var oauthScopes = []string{
	"user:inference",
	"user:profile",
	"user:sessions:claude_code",
	"user:mcp_servers",
	"user:file_upload",
}

var refreshTokenLine = regexp.MustCompile(`^(\s*refresh_token\s*=\s*).*$`)

type oauthTokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

type oauthState struct {
	mu                      sync.RWMutex
	persistMu               sync.Mutex
	tokens                  *oauthTokens
	configPath              string
	allowMissingAccessToken bool
	useClaudeCredentials    bool
	credentialsPath         string
	credentialsMtimeUnixNs  int64
}

func initOAuth(refreshToken, configPath string, allowMissingAccessToken bool) (*oauthState, error) {
	state := &oauthState{configPath: configPath, allowMissingAccessToken: allowMissingAccessToken}
	if refreshToken == oauthRefreshTokenFromClaudeCredentials {
		credentialsPath, err := resolveClaudeCredentialsPath()
		if err != nil {
			return nil, fmt.Errorf("resolve claude credentials path: %w", err)
		}
		state.useClaudeCredentials = true
		state.credentialsPath = credentialsPath

		if err := state.reloadTokensFromClaudeCredentials(); err != nil {
			if !allowMissingAccessToken {
				return nil, fmt.Errorf("initialize oauth from claude credentials: %w", err)
			}
			log.Printf("oauth initialize from claude credentials degraded: %v", err)
		}

		go state.credentialsRefreshLoop()
		return state, nil
	}

	tokens, err := refreshOAuthToken(refreshToken)
	if err != nil {
		if allowMissingAccessToken {
			go state.refreshLoop(refreshToken)
			return state, nil
		}
		return nil, fmt.Errorf("initialize oauth: %w", err)
	}

	state.tokens = &tokens
	if tokens.RefreshToken != refreshToken {
		if err := state.persistRefreshToken(tokens.RefreshToken); err != nil {
			log.Printf("oauth refresh token persist failed after init: %v", err)
		}
	}
	go state.refreshLoop(tokens.RefreshToken)
	return state, nil
}

func (s *oauthState) AccessToken() string {
	if s.useClaudeCredentials {
		if err := s.reloadTokensFromClaudeCredentialsIfNeeded(); err != nil {
			s.mu.RLock()
			hasTokens := s.tokens != nil
			s.mu.RUnlock()
			if !s.allowMissingAccessToken || hasTokens {
				log.Printf("oauth reload from claude credentials failed: %v", err)
			}
		}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.tokens == nil || time.Now().After(s.tokens.ExpiresAt) {
		return ""
	}
	return s.tokens.AccessToken
}

func (s *oauthState) credentialsRefreshLoop() {
	for {
		time.Sleep(30 * time.Second)

		if err := s.reloadTokensFromClaudeCredentialsIfNeeded(); err != nil {
			s.mu.RLock()
			hasTokens := s.tokens != nil
			s.mu.RUnlock()
			if !s.allowMissingAccessToken || hasTokens {
				log.Printf("oauth refresh from claude credentials failed: %v; retrying in 30s", err)
			}
		}
	}
}

func (s *oauthState) reloadTokensFromClaudeCredentialsIfNeeded() error {
	if s.credentialsPath == "" {
		return fmt.Errorf("claude credentials path is empty")
	}

	info, err := os.Stat(s.credentialsPath)
	if err != nil {
		return fmt.Errorf("stat claude credentials: %w", err)
	}

	mtimeUnixNs := info.ModTime().UnixNano()
	now := time.Now()

	s.mu.RLock()
	currentTokens := s.tokens
	currentMtime := s.credentialsMtimeUnixNs
	s.mu.RUnlock()

	if currentTokens != nil && currentMtime == mtimeUnixNs && now.Before(currentTokens.ExpiresAt) {
		return nil
	}

	return s.reloadTokensFromClaudeCredentials()
}

func (s *oauthState) reloadTokensFromClaudeCredentials() error {
	if s.credentialsPath == "" {
		return fmt.Errorf("claude credentials path is empty")
	}

	content, err := os.ReadFile(s.credentialsPath)
	if err != nil {
		return fmt.Errorf("read claude credentials: %w", err)
	}

	var payload struct {
		ClaudeAIOAuth struct {
			AccessToken  string          `json:"accessToken"`
			RefreshToken string          `json:"refreshToken"`
			ExpiresAt    json.RawMessage `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(content, &payload); err != nil {
		return fmt.Errorf("decode claude credentials: %w", err)
	}

	if payload.ClaudeAIOAuth.AccessToken == "" {
		return fmt.Errorf("claude credentials missing claudeAiOauth.accessToken")
	}
	if len(payload.ClaudeAIOAuth.ExpiresAt) == 0 {
		return fmt.Errorf("claude credentials missing claudeAiOauth.expiresAt")
	}

	expiresAt, err := parseClaudeCredentialsExpiresAt(payload.ClaudeAIOAuth.ExpiresAt)
	if err != nil {
		return fmt.Errorf("parse claude credentials expiresAt: %w", err)
	}

	info, err := os.Stat(s.credentialsPath)
	if err != nil {
		return fmt.Errorf("stat claude credentials: %w", err)
	}

	s.mu.Lock()
	s.tokens = &oauthTokens{
		AccessToken:  payload.ClaudeAIOAuth.AccessToken,
		RefreshToken: payload.ClaudeAIOAuth.RefreshToken,
		ExpiresAt:    expiresAt,
	}
	s.credentialsMtimeUnixNs = info.ModTime().UnixNano()
	s.mu.Unlock()

	return nil
}

func resolveClaudeCredentialsPath() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" {
		return filepath.Join(dir, ".credentials.json"), nil
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home dir: %w", err)
	}

	return filepath.Join(homeDir, ".claude", ".credentials.json"), nil
}

func parseClaudeCredentialsExpiresAt(raw json.RawMessage) (time.Time, error) {
	if len(raw) == 0 {
		return time.Time{}, fmt.Errorf("expiresAt is empty")
	}

	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		if str == "" {
			return time.Time{}, fmt.Errorf("expiresAt is empty string")
		}
		expiresAt, err := time.Parse(time.RFC3339Nano, str)
		if err == nil {
			return expiresAt, nil
		}
		expiresAt, err = time.Parse(time.RFC3339, str)
		if err == nil {
			return expiresAt, nil
		}
		return time.Time{}, err
	}

	var num json.Number
	if err := json.Unmarshal(raw, &num); err != nil {
		return time.Time{}, fmt.Errorf("unsupported expiresAt format")
	}

	value, err := num.Int64()
	if err != nil {
		return time.Time{}, fmt.Errorf("expiresAt is not an integer: %w", err)
	}
	if value <= 0 {
		return time.Time{}, fmt.Errorf("expiresAt must be positive")
	}

	// Heuristic for Unix timestamps:
	// seconds: ~1e9, milliseconds: ~1e12, microseconds: ~1e15, nanoseconds: ~1e18.
	switch {
	case value >= 1_000_000_000_000_000_000:
		return time.Unix(0, value), nil
	case value >= 1_000_000_000_000_000:
		return time.UnixMicro(value), nil
	case value >= 1_000_000_000_000:
		return time.UnixMilli(value), nil
	default:
		return time.Unix(value, 0), nil
	}
}

func (s *oauthState) refreshLoop(refreshToken string) {
	currentRefreshToken := refreshToken
	for {
		wait := 30 * time.Second

		s.mu.RLock()
		if s.tokens != nil {
			wait = time.Until(s.tokens.ExpiresAt.Add(-5 * time.Minute))
		}
		s.mu.RUnlock()

		if wait < 10*time.Second {
			wait = 10 * time.Second
		}

		time.Sleep(wait)

		tokens, err := refreshOAuthToken(currentRefreshToken)
		if err != nil {
			if !s.allowMissingAccessToken || s.tokens != nil {
				log.Printf("oauth refresh failed: %v; retrying in 30s", err)
			}
			time.Sleep(30 * time.Second)
			continue
		}

		currentRefreshToken = tokens.RefreshToken
		if tokens.RefreshToken != refreshToken {
			if err := s.persistRefreshToken(tokens.RefreshToken); err != nil {
				log.Printf("oauth refresh token persist failed: %v", err)
			}
		}
		s.mu.Lock()
		s.tokens = &tokens
		s.mu.Unlock()
		refreshToken = tokens.RefreshToken
	}
}

func (s *oauthState) persistRefreshToken(refreshToken string) error {
	s.persistMu.Lock()
	defer s.persistMu.Unlock()

	info, err := os.Stat(s.configPath)
	if err != nil {
		return fmt.Errorf("stat config: %w", err)
	}

	content, err := os.ReadFile(s.configPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}

	newline := "\n"
	if bytes.Contains(content, []byte("\r\n")) {
		newline = "\r\n"
	}

	lines := strings.Split(string(content), newline)
	inOAuth := false
	replaced := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			inOAuth = trimmed == "[oauth]"
			continue
		}
		if !inOAuth {
			continue
		}
		if matches := refreshTokenLine.FindStringSubmatch(line); matches != nil {
			lines[i] = matches[1] + strconv.Quote(refreshToken)
			replaced = true
			break
		}
	}
	if !replaced {
		return fmt.Errorf("config oauth.refresh_token not found")
	}

	updated := strings.Join(lines, newline)
	if !bytes.Equal([]byte(updated), content) && len(content) > 0 && content[len(content)-1] == '\n' && !strings.HasSuffix(updated, newline) {
		updated += newline
	}

	dir := filepath.Dir(s.configPath)
	tmp, err := os.CreateTemp(dir, filepath.Base(s.configPath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp config: %w", err)
	}
	if _, err := tmp.WriteString(updated); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Rename(tmpName, s.configPath); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}

	dirHandle, err := os.Open(dir)
	if err != nil {
		return nil
	}
	defer dirHandle.Close()
	_ = dirHandle.Sync()
	return nil
}

func refreshOAuthToken(refreshToken string) (oauthTokens, error) {
	body, err := json.Marshal(map[string]any{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     oauthClientID,
		"scope":         strings.Join(oauthScopes, " "),
	})
	if err != nil {
		return oauthTokens{}, err
	}

	req, err := http.NewRequest(http.MethodPost, oauthTokenURL, bytes.NewReader(body))
	if err != nil {
		return oauthTokens{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return oauthTokens{}, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return oauthTokens{}, err
	}

	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(respBody, &payload); err != nil {
		return oauthTokens{}, fmt.Errorf("decode oauth response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return oauthTokens{}, fmt.Errorf("oauth refresh failed (%d): %s", resp.StatusCode, string(respBody))
	}
	if payload.AccessToken == "" {
		return oauthTokens{}, fmt.Errorf("oauth response missing access_token")
	}
	if payload.RefreshToken == "" {
		payload.RefreshToken = refreshToken
	}
	if payload.ExpiresIn == 0 {
		payload.ExpiresIn = 3600
	}

	return oauthTokens{
		AccessToken:  payload.AccessToken,
		RefreshToken: payload.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(payload.ExpiresIn) * time.Second),
	}, nil
}
