package main

import (
	"bytes"
	"encoding/base64"
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
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

var (
	platformPattern   = regexp.MustCompile(`Platform:\s*\S+`)
	shellPattern      = regexp.MustCompile(`Shell:\s*\S+`)
	osVersionPattern  = regexp.MustCompile(`OS Version:\s*[^\n<]+`)
	workingDirPattern = regexp.MustCompile(`((?:Primary )?[Ww]orking directory:\s*)/\S+`)
	workingDirPrefix  = regexp.MustCompile(`^/[^/]+/[^/]+/`)
	homePrefixPattern = regexp.MustCompile(`/(?:Users|home)/[^/\s]+/`)
	systemReminderTag = regexp.MustCompile(`(<system-reminder>)([\s\S]*?)(</system-reminder>)`)
	billingHeaderLine = regexp.MustCompile(`x-anthropic-billing-header:[^\n]+\n?`)
	billingHeaderText = regexp.MustCompile(`^\s*x-anthropic-billing-header:`)
)

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

func proxyToUpstream(w http.ResponseWriter, r *http.Request, config Config, oauth *oauthState, allowMissingAccessToken bool, rewrite func([]byte, Config) ([]byte, error)) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()
	logRequest("incoming request", r.Method, r.URL.String(), r.Header, body)

	rewrittenBody, err := rewrite(body, config)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to rewrite request body: %v", err), http.StatusBadRequest)
		return
	}

	upstreamRequestURL := *config.Upstream
	upstreamRequestURL.Path = r.URL.Path
	upstreamRequestURL.RawPath = ""
	upstreamRequestURL.RawQuery = r.URL.RawQuery

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
	if _, err := io.Copy(w, resp.Body); err != nil {
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

func rewriteMessagesBody(body []byte, config Config) ([]byte, error) {
	jl, err := NewJSONLens(body)
	if err != nil {
		return body, nil
	}

	if userID, ok := jl.At("metadata", "user_id"); ok {
		if rawUserID, ok := userID.AsString(); ok && rawUserID != "" {
			inner, err := NewJSONLens([]byte(rawUserID))
			if err == nil {
				if deviceID, ok := inner.At("device_id"); ok {
					if err := deviceID.Set(config.Identity.DeviceID); err != nil {
						return nil, err
					}
				}
				if err := userID.Set(string(inner.Bytes())); err != nil {
					return nil, err
				}
			}
		}
	}

	if system, ok := jl.At("system"); ok {
		if text, ok := system.AsString(); ok {
			if err := system.Set(billingHeaderLine.ReplaceAllString(rewritePromptText(text, config), "")); err != nil {
				return nil, err
			}
		} else if blocks, ok := system.AsArrayIter(); ok {
			for block := range blocks {
				// remove billing header lines and rewrite prompt text in system blocks,
				// which can be either string or object with a "text" field.
				if text, ok := block.AsString(); ok {
					if billingHeaderText.MatchString(text) {
						if err := block.Set(nil); err != nil {
							return nil, err
						}
						continue
					}
					if err := block.Set(rewritePromptText(text, config)); err != nil {
						return nil, err
					}
					continue
				}
				if text, ok := block.At("text"); ok {
					if value, ok := text.AsString(); ok {
						if billingHeaderText.MatchString(value) {
							if err := block.Set(nil); err != nil {
								return nil, err
							}
							continue
						}
						if err := text.Set(rewritePromptText(value, config)); err != nil {
							return nil, err
						}
					}
				}
			}
			// Now trim out any null blocks that were removed due to billing headers.
			array, ok := system.AsArray()
			if ok {
				compacted := make([]any, 0, len(array))
				for _, item := range array {
					if item.IsNull() {
						continue
					}
					var value any
					if err := json.Unmarshal(item.Bytes(), &value); err != nil {
						return nil, err
					}
					compacted = append(compacted, value)
				}
				if err := system.Set(compacted); err != nil {
					return nil, err
				}
			}
		}
	}

	if messages, ok := jl.At("messages"); ok {
		if items, ok := messages.AsArrayIter(); ok {
			for item := range items {
				content, ok := item.At("content")
				if !ok {
					continue
				}
				if text, ok := content.AsString(); ok {
					if err := content.Set(rewriteSystemReminders(text, config)); err != nil {
						return nil, err
					}
					continue
				}
				if blocks, ok := content.AsArrayIter(); ok {
					for block := range blocks {
						text, ok := block.At("text")
						if !ok {
							continue
						}
						value, ok := text.AsString()
						if !ok {
							continue
						}
						if err := text.Set(rewriteSystemReminders(value, config)); err != nil {
							return nil, err
						}
					}
				}
			}
		}
	}

	return jl.Bytes(), nil
}

func rewriteEventLoggingBatchBody(body []byte, config Config) ([]byte, error) {
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
		if _, ok := data["env"]; ok {
			data["env"] = buildCanonicalEnv(config)
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

func rewriteGenericIdentityBody(body []byte, config Config) ([]byte, error) {
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

func buildCanonicalEnv(config Config) map[string]any {
	return map[string]any{
		"platform":               config.Env.Platform,
		"platform_raw":           config.Env.PlatformRaw,
		"arch":                   config.Env.Arch,
		"node_version":           config.Env.NodeVersion,
		"terminal":               config.Env.Terminal,
		"package_managers":       config.Env.PackageManagers,
		"runtimes":               config.Env.Runtimes,
		"is_running_with_bun":    config.Env.IsRunningWithBun,
		"is_ci":                  false,
		"is_claubbit":            false,
		"is_claude_code_remote":  false,
		"is_local_agent_mode":    false,
		"is_conductor":           false,
		"is_github_action":       false,
		"is_claude_code_action":  false,
		"is_claude_ai_auth":      config.Env.IsClaudeAIAuth,
		"version":                config.Env.Version,
		"version_base":           config.Env.VersionBase,
		"build_time":             config.Env.BuildTime,
		"deployment_environment": config.Env.DeploymentEnvironment,
		"vcs":                    config.Env.VCS,
	}
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

func rewritePromptText(text string, config Config) string {
	result := platformPattern.ReplaceAllString(text, "Platform: "+config.PromptEnv.Platform)
	result = shellPattern.ReplaceAllString(result, "Shell: "+config.PromptEnv.Shell)
	result = osVersionPattern.ReplaceAllString(result, "OS Version: "+config.PromptEnv.OSVersion)
	result = workingDirPattern.ReplaceAllString(result, "$1"+config.PromptEnv.WorkingDir)

	homePrefix := "/Users/user/"
	if prefix := workingDirPrefix.FindString(config.PromptEnv.WorkingDir); prefix != "" {
		homePrefix = prefix
	}

	return homePrefixPattern.ReplaceAllString(result, homePrefix)
}

func rewriteSystemReminders(text string, config Config) string {
	return systemReminderTag.ReplaceAllStringFunc(text, func(match string) string {
		parts := systemReminderTag.FindStringSubmatch(match)
		if len(parts) != 4 {
			return match
		}
		return parts[1] + rewritePromptText(parts[2], config) + parts[3]
	})
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
		case "user-agent":
			dst.Set(key, fmt.Sprintf("claude-code/%s (external, cli)", config.Env.Version))
		default:
			dst.Set(key, value)
		}
	}
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
	Listen    string
	Upstream  *url.URL
	OAuth     OAuthConfig
	Identity  IdentityConfig
	Env       EnvConfig
	PromptEnv PromptEnvConfig
	Process   ProcessConfig
}

type rawConfig struct {
	Listen    *string         `toml:"listen"`
	Upstream  *string         `toml:"upstream"`
	OAuth     OAuthConfig     `toml:"oauth"`
	Identity  IdentityConfig  `toml:"identity"`
	Env       EnvConfig       `toml:"env"`
	PromptEnv PromptEnvConfig `toml:"prompt_env"`
	Process   ProcessConfig   `toml:"process"`
}

type OAuthConfig struct {
	RefreshToken string `toml:"refresh_token"`
}

type IdentityConfig struct {
	DeviceID string `toml:"device_id"`
	Email    string `toml:"email"`
}

type EnvConfig struct {
	Platform              string `toml:"platform"`
	PlatformRaw           string `toml:"platform_raw"`
	Arch                  string `toml:"arch"`
	NodeVersion           string `toml:"node_version"`
	Terminal              string `toml:"terminal"`
	PackageManagers       string `toml:"package_managers"`
	Runtimes              string `toml:"runtimes"`
	IsRunningWithBun      bool   `toml:"is_running_with_bun"`
	IsClaudeAIAuth        bool   `toml:"is_claude_ai_auth"`
	Version               string `toml:"version"`
	VersionBase           string `toml:"version_base"`
	BuildTime             string `toml:"build_time"`
	DeploymentEnvironment string `toml:"deployment_environment"`
	VCS                   string `toml:"vcs"`
}

type PromptEnvConfig struct {
	Platform   string `toml:"platform"`
	Shell      string `toml:"shell"`
	OSVersion  string `toml:"os_version"`
	WorkingDir string `toml:"working_dir"`
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
	if raw.Identity.DeviceID == "" || raw.Identity.Email == "" {
		return Config{}, fmt.Errorf("config identity.device_id/email are required")
	}
	if raw.Env.Platform == "" || raw.Env.PlatformRaw == "" || raw.Env.Arch == "" || raw.Env.NodeVersion == "" || raw.Env.Terminal == "" || raw.Env.PackageManagers == "" || raw.Env.Runtimes == "" || raw.Env.Version == "" || raw.Env.VersionBase == "" || raw.Env.BuildTime == "" || raw.Env.DeploymentEnvironment == "" || raw.Env.VCS == "" {
		return Config{}, fmt.Errorf("config env.platform/platform_raw/arch/node_version/terminal/package_managers/runtimes/version/version_base/build_time/deployment_environment/vcs are required")
	}
	if raw.PromptEnv.Platform == "" || raw.PromptEnv.Shell == "" || raw.PromptEnv.OSVersion == "" || raw.PromptEnv.WorkingDir == "" {
		return Config{}, fmt.Errorf("config prompt_env.platform/shell/os_version/working_dir are required")
	}
	if raw.Process.ConstrainedMemory == 0 || raw.Process.RSSRange[0] == 0 || raw.Process.RSSRange[1] == 0 || raw.Process.HeapTotalRange[0] == 0 || raw.Process.HeapTotalRange[1] == 0 || raw.Process.HeapUsedRange[0] == 0 || raw.Process.HeapUsedRange[1] == 0 {
		return Config{}, fmt.Errorf("config process.constrained_memory/rss_range/heap_total_range/heap_used_range are required")
	}

	return Config{
		Listen:    listen,
		Upstream:  upstreamURL,
		OAuth:     raw.OAuth,
		Identity:  raw.Identity,
		Env:       raw.Env,
		PromptEnv: raw.PromptEnv,
		Process:   raw.Process,
	}, nil
}
