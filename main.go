package main

import (
	"bytes"
	cryptorand "crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

const oauthBetaHeader = "oauth-2025-04-20"

func main() {
	configPath := flag.String("config", "config.toml", "config file path")
	allowMissingAccessToken := flag.Bool("allow-missing-access-token", false, "allow proxying without an OAuth access token")
	flag.Parse()

	if err := setupFileLogger(*configPath); err != nil {
		log.Fatal(err)
	}

	config, err := loadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}

	oauth, err := initOAuth(config.OAuth.RefreshToken, *configPath, *allowMissingAccessToken)
	if err != nil {
		if !*allowMissingAccessToken {
			log.Fatal(err)
		}
		log.Printf("oauth initialization degraded: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/v1/messages", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyToUpstream(w, r, config, oauth, *allowMissingAccessToken, rewriteMessagesBody)
	}))
	mux.Handle("/api/event_logging/batch", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyToUpstream(w, r, config, oauth, *allowMissingAccessToken, rewriteEventLoggingBatchBody)
	}))
	mux.Handle("/policy_limits", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyToUpstream(w, r, config, oauth, *allowMissingAccessToken, rewriteGenericIdentityBody)
	}))
	mux.Handle("/settings", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyToUpstream(w, r, config, oauth, *allowMissingAccessToken, rewriteGenericIdentityBody)
	}))

	log.Printf("proxy listening on %s -> %s", config.Listen, config.Upstream.String())
	if err := http.ListenAndServe(config.Listen, mux); err != nil {
		log.Fatal(err)
	}
}

func proxyToUpstream(w http.ResponseWriter, r *http.Request, config Config, oauth *oauthState, allowMissingAccessToken bool, rewrite func([]byte, Config, string) ([]byte, error)) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()
	logRequest("incoming request", r.Method, r.URL.String(), r.Header, body)

	// Generate a random session ID for this request if not already present.
	sessionID := r.Header.Get("X-Claude-Code-Session-Id")
	if r.URL.Path == "/v1/messages" && sessionID == "" {
		sessionID = newSessionID()
	}

	rewrittenBody, err := rewrite(body, config, sessionID)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to rewrite request body: %v", err), http.StatusBadRequest)
		return
	}

	upstreamRequestURL := *config.Upstream
	upstreamRequestURL.Path = r.URL.Path
	upstreamRequestURL.RawPath = ""
	query := r.URL.Query()
	// Always use ?beta=true for /v1/messages because Claude Code uses it.
	if r.URL.Path == "/v1/messages" && !query.Has("beta") {
		query.Set("beta", "true")
	}
	upstreamRequestURL.RawQuery = query.Encode()

	proxyReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamRequestURL.String(), bytes.NewReader(rewrittenBody))
	if err != nil {
		http.Error(w, "failed to build upstream request", http.StatusInternalServerError)
		return
	}

	rewriteHeaders(proxyReq.Header, r.Header, config)
	accessToken := oauth.AccessToken()
	if accessToken == "" {
		if !allowMissingAccessToken {
			http.Error(w, "oauth token not available", http.StatusServiceUnavailable)
			return
		}
		log.Printf("proxying without oauth access token: %s %s", r.Method, r.URL.String())
	} else {
		proxyReq.Header.Set("Authorization", "Bearer "+accessToken)
		// Beside ?beta=true, we also need to ensure oauthHeader to use oauth authentication method.
		ensureHeaderListValue(proxyReq.Header, "Anthropic-Beta", oauthBetaHeader)
		if r.URL.Path == "/v1/messages" {
			// Some more headers for /v1/messages to ensure the request looks like it's coming from the official client
			standardizeMessagesHeaders(proxyReq.Header, sessionID)
		}
	}
	logRequest("rewritten request", r.Method, upstreamRequestURL.String(), proxyReq.Header, rewrittenBody)
	proxyReq.ContentLength = int64(len(rewrittenBody))
	proxyReq.Host = config.Upstream.Host

	resp, err := http.DefaultClient.Do(proxyReq)
	if err != nil {
		log.Printf("proxy error: %s %s: %v", r.Method, r.URL.String(), err)
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)
	if _, err := io.Copy(flushWriter{w, rc}, resp.Body); err != nil {
		log.Printf("response copy error: %s %s: %v", r.Method, r.URL.String(), err)
	}
}

func setupFileLogger(configPath string) error {
	logPath := filepath.Join(filepath.Dir(configPath), "gateway.log")
	file, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open log file %q: %w", logPath, err)
	}
	log.SetOutput(file)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	return nil
}

func logRequest(prefix, method, path string, headers http.Header, body []byte) {
	headersJSON, err := json.MarshalIndent(headers, "", "  ")
	if err != nil {
		log.Printf("%s\nmethod=%s\npath=%s\nheaders_marshal_error=%v\nbody=\n%s", prefix, method, path, err, string(body))
		return
	}
	log.Printf("%s\nmethod=%s\npath=%s\nheaders=\n%s\nbody=\n%s", prefix, method, path, string(headersJSON), string(body))
}

func rewriteEventLoggingBatchBody(body []byte, config Config, _ string) ([]byte, error) {
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body, nil
	}

	root, ok := parsed.(map[string]any)
	if !ok {
		return body, nil
	}

	events, ok := root["events"].([]any)
	if !ok {
		return json.Marshal(parsed)
	}

	for _, rawEvent := range events {
		event, ok := rawEvent.(map[string]any)
		if !ok {
			continue
		}
		data, ok := event["event_data"].(map[string]any)
		if !ok {
			continue
		}

		if _, ok := data["device_id"]; ok {
			data["device_id"] = config.Identity.DeviceID
		}
		if _, ok := data["email"]; ok {
			data["email"] = config.Identity.Email
		}
		if process, ok := data["process"]; ok {
			data["process"] = rewriteProcess(process, config)
		}

		delete(data, "baseUrl")
		delete(data, "base_url")
		delete(data, "gateway")

		if additional, ok := data["additional_metadata"].(string); ok {
			data["additional_metadata"] = rewriteAdditionalMetadata(additional)
		}
	}

	return json.Marshal(parsed)
}

func rewriteGenericIdentityBody(body []byte, config Config, _ string) ([]byte, error) {
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body, nil
	}

	root, ok := parsed.(map[string]any)
	if !ok {
		return body, nil
	}

	if _, ok := root["device_id"]; ok {
		root["device_id"] = config.Identity.DeviceID
	}
	if _, ok := root["email"]; ok {
		root["email"] = config.Identity.Email
	}

	return json.Marshal(root)
}

func rewriteProcess(original any, config Config) any {
	switch value := original.(type) {
	case string:
		decoded, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return original
		}

		var process map[string]any
		if err := json.Unmarshal(decoded, &process); err != nil {
			return original
		}

		rewriteProcessFields(process, config)
		rewritten, err := json.Marshal(process)
		if err != nil {
			return original
		}
		return base64.StdEncoding.EncodeToString(rewritten)
	case map[string]any:
		rewriteProcessFields(value, config)
		return value
	default:
		return original
	}
}

func rewriteProcessFields(process map[string]any, config Config) {
	process["constrainedMemory"] = config.Process.ConstrainedMemory
	process["rss"] = randomInRange(config.Process.RSSRange[0], config.Process.RSSRange[1])
	process["heapTotal"] = randomInRange(config.Process.HeapTotalRange[0], config.Process.HeapTotalRange[1])
	process["heapUsed"] = randomInRange(config.Process.HeapUsedRange[0], config.Process.HeapUsedRange[1])
}

func rewriteAdditionalMetadata(original string) string {
	decoded, err := base64.StdEncoding.DecodeString(original)
	if err != nil {
		return original
	}

	var metadata map[string]any
	if err := json.Unmarshal(decoded, &metadata); err != nil {
		return original
	}

	delete(metadata, "baseUrl")
	delete(metadata, "base_url")
	delete(metadata, "gateway")

	rewritten, err := json.Marshal(metadata)
	if err != nil {
		return original
	}
	return base64.StdEncoding.EncodeToString(rewritten)
}

func randomInRange(min, max int64) int64 {
	if max < min {
		min, max = max, min
	}
	if min == max {
		return min
	}
	return min + rand.Int64N(max-min+1)
}

func rewriteHeaders(dst, src http.Header, config Config) {
	for key, values := range src {
		value := strings.Join(values, ", ")
		if value == "" {
			continue
		}

		switch strings.ToLower(key) {
		case "host", "connection", "proxy-authorization", "proxy-connection", "transfer-encoding", "authorization", "content-length", "x-api-key", "x-anthropic-billing-header":
			continue
		default:
			dst.Set(key, value)
		}
	}
}

func ensureHeaderListValue(headers http.Header, key, want string) {
	for _, value := range headers.Values(key) {
		for _, part := range strings.Split(value, ",") {
			if strings.TrimSpace(part) == want {
				return
			}
		}
	}

	existing := strings.TrimSpace(headers.Get(key))
	if existing == "" {
		headers.Set(key, want)
		return
	}

	headers.Set(key, existing+", "+want)
}

func newSessionID() string {
	buf := make([]byte, 16)
	if _, err := cryptorand.Read(buf); err != nil {
		// It couldn't be.
		panic(err)
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	hexValue := hex.EncodeToString(buf)
	return fmt.Sprintf("%s-%s-%s-%s-%s", hexValue[:8], hexValue[8:12], hexValue[12:16], hexValue[16:20], hexValue[20:32])
}

type flushWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

func (fw flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	fw.rc.Flush()
	return n, err
}

func copyResponseHeaders(dst, src http.Header) {
	for key, values := range src {
		if strings.EqualFold(key, "Transfer-Encoding") {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

type Config struct {
	Listen   string
	Upstream *url.URL
	OAuth    OAuthConfig
	Identity IdentityConfig
	Process  ProcessConfig
}

type rawConfig struct {
	Listen   *string        `toml:"listen"`
	Upstream *string        `toml:"upstream"`
	OAuth    OAuthConfig    `toml:"oauth"`
	Identity IdentityConfig `toml:"identity"`
	Process  ProcessConfig  `toml:"process"`
}

type OAuthConfig struct {
	RefreshToken string `toml:"refresh_token"`
}

type IdentityConfig struct {
	DeviceID    string `toml:"device_id"`
	Email       string `toml:"email"`
	AccountUUID string `toml:"account_uuid"`
}

type ProcessConfig struct {
	ConstrainedMemory int64    `toml:"constrained_memory"`
	RSSRange          [2]int64 `toml:"rss_range"`
	HeapTotalRange    [2]int64 `toml:"heap_total_range"`
	HeapUsedRange     [2]int64 `toml:"heap_used_range"`
}

func loadConfig(path string) (Config, error) {
	var raw rawConfig
	if _, err := toml.DecodeFile(path, &raw); err != nil {
		return Config{}, fmt.Errorf("load config %q: %w", path, err)
	}

	listen := ":8443"
	if raw.Listen != nil {
		if *raw.Listen == "" {
			return Config{}, fmt.Errorf("config listen must not be empty")
		}
		listen = *raw.Listen
	}

	upstream := "https://api.anthropic.com"
	if raw.Upstream != nil {
		if *raw.Upstream == "" {
			return Config{}, fmt.Errorf("config upstream must not be empty")
		}
		upstream = *raw.Upstream
	}

	upstreamURL, err := url.Parse(upstream)
	if err != nil {
		return Config{}, fmt.Errorf("config upstream is invalid: %w", err)
	}
	if upstreamURL.Scheme == "" || upstreamURL.Host == "" {
		return Config{}, fmt.Errorf("config upstream must be an absolute URL")
	}

	if raw.OAuth.RefreshToken == "" {
		return Config{}, fmt.Errorf("config oauth.refresh_token is required")
	}
	if raw.Identity.DeviceID == "" || raw.Identity.Email == "" || raw.Identity.AccountUUID == "" {
		return Config{}, fmt.Errorf("config identity.device_id/email/account_uuid are required")
	}
	if raw.Process.ConstrainedMemory == 0 || raw.Process.RSSRange[0] == 0 || raw.Process.RSSRange[1] == 0 || raw.Process.HeapTotalRange[0] == 0 || raw.Process.HeapTotalRange[1] == 0 || raw.Process.HeapUsedRange[0] == 0 || raw.Process.HeapUsedRange[1] == 0 {
		return Config{}, fmt.Errorf("config process.constrained_memory/rss_range/heap_total_range/heap_used_range are required")
	}

	return Config{
		Listen:   listen,
		Upstream: upstreamURL,
		OAuth:    raw.OAuth,
		Identity: raw.Identity,
		Process:  raw.Process,
	}, nil
}
