package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// Config holds local execution, relay delivery, and fallback-gate settings.
type Config struct {
	From                string   `json:"from,omitempty"`
	DefaultTo           string   `json:"default_to,omitempty"`
	ServiceURL          string   `json:"service_url,omitempty"`
	ServiceToken        string   `json:"service_token,omitempty"`
	WebhookURL          string   `json:"webhook_url,omitempty"`
	BotToken            string   `json:"bot_token,omitempty"`
	ChannelID           string   `json:"channel_id,omitempty"`
	CDPURL              string   `json:"cdp_url,omitempty"`
	Browser             string   `json:"browser,omitempty"`
	ChromeSession       string   `json:"chrome_session,omitempty"`
	ChromeSessionHyphen string   `json:"chrome-session,omitempty"`
	ChromeProfile       string   `json:"chrome_profile,omitempty"`
	ChromeProfileHyphen string   `json:"chrome-profile,omitempty"`
	UserDataDir         string   `json:"user_data_dir,omitempty"`
	Driver              string   `json:"driver,omitempty"` // "" | "cdp" | "uia"
	DebugDir            string   `json:"debug_dir,omitempty"`
	RecipesURI          string   `json:"recipes_uri,omitempty"`
	FallbackGate        string   `json:"fallback_gate,omitempty"` // "auto" | "tray"
	SendErrorBundle     *bool    `json:"send_error_bundle,omitempty"`
	ForbiddenTerms      []string `json:"forbidden_terms,omitempty"`
}

// PreferredChromeSession returns the configured Chrome session/profile name.
func (c Config) PreferredChromeSession() string {
	for _, v := range []string{c.ChromeSession, c.ChromeSessionHyphen, c.ChromeProfile, c.ChromeProfileHyphen} {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// NormalizedFallbackGate returns "tray" when explicitly configured, defaulting to "auto".
func (c Config) NormalizedFallbackGate() string {
	if strings.EqualFold(strings.TrimSpace(c.FallbackGate), "tray") {
		return "tray"
	}
	return "auto"
}

// NormalizedDriver returns "uia" when explicitly configured, defaulting to "cdp".
func (c Config) NormalizedDriver() string {
	if strings.EqualFold(strings.TrimSpace(c.Driver), "uia") {
		return "uia"
	}
	return "cdp"
}

// ParseConfigJSON decodes a deskdrop.json configuration file permissively (allowing // comments
// and normalizing legacy chrome-session / chrome_profile aliases into ChromeSession).
func ParseConfigJSON(raw []byte, dst *Config) (hadDuplicateSessionKeys bool, err error) {
	cleaned := stripJSONComments(raw)
	if len(bytes.TrimSpace(cleaned)) == 0 {
		return false, nil
	}
	if err := json.Unmarshal(cleaned, dst); err != nil {
		return false, err
	}
	var rawMap map[string]any
	if json.Unmarshal(cleaned, &rawMap) == nil {
		sessionKeyCount := 0
		for _, k := range []string{"chrome_session", "chrome-session", "chrome_profile", "chrome-profile"} {
			if _, exists := rawMap[k]; exists {
				sessionKeyCount++
			}
		}
		if sessionKeyCount > 1 {
			hadDuplicateSessionKeys = true
		}
	}
	dst.ChromeSession = dst.PreferredChromeSession()
	dst.ChromeSessionHyphen = ""
	dst.ChromeProfile = ""
	dst.ChromeProfileHyphen = ""
	return hadDuplicateSessionKeys, nil
}

func (c Config) ShouldSendErrorBundle() bool {
	if c.SendErrorBundle == nil {
		return true
	}
	return *c.SendErrorBundle
}

// Recipe defines a strictly declarative desktop/browser extraction contract (zero arbitrary JS).
type Recipe struct {
	Name                     string            `json:"name"`
	Topic                    string            `json:"topic"`
	Description              string            `json:"description,omitempty"`
	Driver                   string            `json:"driver,omitempty"` // "" / "cdp" | "uia"
	AllowComputerUseFallback *bool             `json:"allow_computer_use_fallback,omitempty"`
	AllowedDomains           []string          `json:"allowed_domains,omitempty"`
	Defaults                 map[string]string `json:"defaults,omitempty"`
	Steps                    []Step            `json:"steps"`
}

// NormalizedDriver returns "uia" when explicitly set on the recipe, defaulting to "cdp".
func (r Recipe) NormalizedDriver() string {
	if strings.EqualFold(strings.TrimSpace(r.Driver), "uia") {
		return "uia"
	}
	return "cdp"
}

// AllowsComputerUseFallback defaults to true unless explicitly disabled by the recipe.
func (r Recipe) AllowsComputerUseFallback() bool {
	if r.AllowComputerUseFallback == nil {
		return true
	}
	return *r.AllowComputerUseFallback
}

// FieldRule extracts a single typed scalar value declaratively in both browser JS and pure Go.
type FieldRule struct {
	Selector      string `json:"selector,omitempty"`
	Closest       string `json:"closest,omitempty"`
	Attr          string `json:"attr,omitempty"` // "" / "text", attribute name, or fallback chain "aria-label|text"
	Regex         string `json:"regex,omitempty"`
	Group         int    `json:"group,omitempty"`
	FallbackRegex string `json:"fallback_regex,omitempty"`
	FallbackGroup int    `json:"fallback_group,omitempty"`
	Parse         string `json:"parse,omitempty"` // "" | "string" | "number" | "float"
	Prefix        string `json:"prefix,omitempty"`
	Suffix        string `json:"suffix,omitempty"`
	MaxChars      int    `json:"max_chars,omitempty"`
	Optional      bool   `json:"optional,omitempty"`
}

// ItemRule extracts a list of repeating structured records from matching DOM containers.
type ItemRule struct {
	Container     string               `json:"container"`
	AnchorClosest string               `json:"anchor_closest,omitempty"`
	DedupeBy      string               `json:"dedupe_by,omitempty"`
	Limit         int                  `json:"limit,omitempty"`
	Fields        map[string]FieldRule `json:"fields"`
}

// Step attaches to a target tab or opens a scratch tab and runs declarative extraction rules.
type Step struct {
	ID             string               `json:"id"`
	URL            string               `json:"url,omitempty"`
	MatchTabURL    string               `json:"match_tab_url,omitempty"`
	WindowTitle    string               `json:"window_title,omitempty"`
	ReuseOpenTab   bool                 `json:"reuse_open_tab,omitempty"`
	WaitSelector   string               `json:"wait_selector,omitempty"`
	WaitText       string               `json:"wait_text,omitempty"`
	WaitMs         int                  `json:"wait_ms,omitempty"`
	TimeoutMs      int                  `json:"timeout_ms,omitempty"`
	ClickSelector  string               `json:"click_selector,omitempty"`
	ClickIfPresent string               `json:"click_if_present,omitempty"`
	Selectors      map[string]string    `json:"selectors,omitempty"`
	Fields         map[string]FieldRule `json:"fields,omitempty"`
	Items          *ItemRule            `json:"items,omitempty"`
	Pagination     *PaginationRule      `json:"pagination,omitempty"`
	IncludeTables  *bool                `json:"include_tables,omitempty"`
	IncludeCards   *bool                `json:"include_cards,omitempty"`
	MaxTextChars   int                  `json:"max_text_chars,omitempty"`
	Expect         Expectation          `json:"expect,omitempty"`
}

func (s Step) WantTables() bool {
	return s.IncludeTables == nil || *s.IncludeTables
}

func (s Step) WantCards() bool {
	return s.IncludeCards == nil || *s.IncludeCards
}

// Expectation defines assertions that trigger error-escalation dumps when violated.
type Expectation struct {
	NotURLContains  []string `json:"not_url_contains,omitempty"`
	MustContainText []string `json:"must_contain_text,omitempty"`
	MinTables       int      `json:"min_tables,omitempty"`
	MinMetrics      int      `json:"min_metrics,omitempty"`
	MinSelectorHits int      `json:"min_selector_hits,omitempty"`
	MinItems        int      `json:"min_items,omitempty"`
	RequiredFields  []string `json:"required_fields,omitempty"`
}

// MetricPair represents a label-value stat card extracted from the DOM.
type MetricPair struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// TableData represents an HTML <table> or ARIA grid/table extracted across Shadow DOMs.
type TableData struct {
	Title   string     `json:"title,omitempty"`
	Headers []string   `json:"headers,omitempty"`
	Rows    [][]string `json:"rows"`
}

// StepResult holds structured declarative DOM output from a single recipe step.
type StepResult struct {
	StepID       string            `json:"step_id"`
	Title        string            `json:"title"`
	URL          string            `json:"url"`
	ReusedTab    bool              `json:"reused_tab,omitempty"`
	PagesVisited int               `json:"pages_visited,omitempty"`
	Headings     []string          `json:"headings,omitempty"`
	Metrics      []MetricPair      `json:"metrics,omitempty"`
	Selectors    map[string]string `json:"selectors,omitempty"`
	Fields       map[string]any    `json:"fields,omitempty"`
	Items        []map[string]any  `json:"items,omitempty"`
	Tables       []TableData       `json:"tables,omitempty"`
	TextExcerpt  string            `json:"text_excerpt,omitempty"`
}

// DropReport is the top-level structured extraction payload delivered to the relay.
type DropReport struct {
	SchemaVersion string       `json:"schema_version"`
	Mode          string       `json:"mode,omitempty"` // "deterministic" | "computer_use_fallback"
	Recipe        string       `json:"recipe"`
	Topic         string       `json:"topic"`
	Host          string       `json:"host"`
	CapturedAtUTC string       `json:"captured_at_utc"`
	Steps         []StepResult `json:"steps"`
}

const (
	DropModeDeterministic       = "deterministic"
	DropModeComputerUseFallback = "computer_use_fallback"
)

var varPlaceholderRe = regexp.MustCompile(`\{\{\s*([a-zA-Z0-9_]+)\s*\}\}`)

// ParseJSONStrict decodes a single JSON value (allowing // line comments outside strings) with
// DisallowUnknownFields and rejects any trailing tokens after the root value.
func ParseJSONStrict(raw []byte, dst any) error {
	cleaned := stripJSONComments(raw)
	dec := json.NewDecoder(bytes.NewReader(cleaned))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("unexpected trailing JSON token")
		}
		return fmt.Errorf("unexpected trailing JSON content: %w", err)
	}
	return nil
}

// ResolveRecipe substitutes {{var}} placeholders and validates declarative FieldRule / ItemRule contracts.
func ResolveRecipe(r Recipe, cliVars map[string]string) (Recipe, error) {
	if strings.TrimSpace(r.Name) == "" {
		return Recipe{}, errors.New("recipe.name is required")
	}
	if strings.TrimSpace(r.Topic) == "" {
		r.Topic = r.Name
	}
	if len(r.Steps) == 0 {
		return Recipe{}, fmt.Errorf("recipe %q must define at least 1 step", r.Name)
	}
	drv := strings.ToLower(strings.TrimSpace(r.Driver))
	switch drv {
	case "", "cdp", "uia":
		r.Driver = drv
	default:
		return Recipe{}, fmt.Errorf("recipe %q has invalid driver %q (expected cdp or uia)", r.Name, r.Driver)
	}

	merged := make(map[string]string, len(r.Defaults)+len(cliVars))
	for k, v := range r.Defaults {
		merged[k] = v
	}
	for k, v := range cliVars {
		merged[k] = v
	}

	var missing []string
	sub := func(in string) string {
		return varPlaceholderRe.ReplaceAllStringFunc(in, func(m string) string {
			submatch := varPlaceholderRe.FindStringSubmatch(m)
			key := submatch[1]
			val, ok := merged[key]
			if !ok || val == "" {
				missing = append(missing, key)
				return m
			}
			return val
		})
	}

	r.Topic = sub(r.Topic)
	steps := make([]Step, len(r.Steps))
	for i, s := range r.Steps {
		if strings.TrimSpace(s.ID) == "" {
			s.ID = fmt.Sprintf("step_%d", i+1)
		}
		s.URL = sub(s.URL)
		s.MatchTabURL = sub(s.MatchTabURL)
		s.WindowTitle = strings.TrimSpace(sub(s.WindowTitle))
		s.WaitSelector = sub(s.WaitSelector)
		s.WaitText = sub(s.WaitText)
		s.ClickSelector = sub(s.ClickSelector)
		s.ClickIfPresent = sub(s.ClickIfPresent)
		if s.URL == "" && s.MatchTabURL == "" && (r.NormalizedDriver() != "uia" || s.WindowTitle == "") {
			return Recipe{}, fmt.Errorf("step %q requires url or match_tab_url", s.ID)
		}
		if len(s.Selectors) > 0 {
			sel := make(map[string]string, len(s.Selectors))
			for k, v := range s.Selectors {
				sel[k] = sub(v)
			}
			s.Selectors = sel
		}
		if len(s.Fields) > 0 {
			resolvedFields, err := resolveFieldRules(s.ID, s.Fields, sub)
			if err != nil {
				return Recipe{}, err
			}
			s.Fields = resolvedFields
		}
		if s.Items != nil {
			ir := *s.Items
			ir.Container = strings.TrimSpace(sub(ir.Container))
			ir.AnchorClosest = strings.TrimSpace(sub(ir.AnchorClosest))
			ir.DedupeBy = strings.TrimSpace(sub(ir.DedupeBy))
			if ir.Container == "" {
				return Recipe{}, fmt.Errorf("step %q items.container is required", s.ID)
			}
			if len(ir.Fields) == 0 {
				return Recipe{}, fmt.Errorf("step %q items.fields must define at least 1 field", s.ID)
			}
			resolvedFields, err := resolveFieldRules(s.ID+".items", ir.Fields, sub)
			if err != nil {
				return Recipe{}, err
			}
			ir.Fields = resolvedFields
			s.Items = &ir
		}
		if s.Pagination != nil {
			resolvedPag, err := resolvePaginationRule(s.ID, s.Pagination, sub)
			if err != nil {
				return Recipe{}, err
			}
			s.Pagination = resolvedPag
		}
		steps[i] = s
	}
	if len(missing) > 0 {
		return Recipe{}, fmt.Errorf("missing required recipe variable(s): %s (pass --var key=value)", strings.Join(uniqueStrings(missing), ", "))
	}
	r.Steps = steps
	if err := validateStepSelectors(r); err != nil {
		return Recipe{}, err
	}
	if err := validateResolvedTargets(r); err != nil {
		return Recipe{}, err
	}
	return r, nil
}

func validateOptionalSelector(scope string, field string, sel string) error {
	if strings.TrimSpace(sel) == "" {
		return nil
	}
	if err := ValidateCSSSelector(sel); err != nil {
		return fmt.Errorf("%s %s %q: %w", scope, field, sel, err)
	}
	return nil
}

func validateStepSelectors(r Recipe) error {
	for _, s := range r.Steps {
		scope := fmt.Sprintf("step %q", s.ID)
		for _, pair := range [][2]string{
			{"wait_selector", s.WaitSelector},
			{"click_selector", s.ClickSelector},
			{"click_if_present", s.ClickIfPresent},
		} {
			if err := validateOptionalSelector(scope, pair[0], pair[1]); err != nil {
				return err
			}
		}
		for k, sel := range s.Selectors {
			if err := validateOptionalSelector(scope, fmt.Sprintf("selectors[%q]", k), sel); err != nil {
				return err
			}
		}
		for k, fr := range s.Fields {
			if err := validateOptionalSelector(scope, fmt.Sprintf("fields[%q].selector", k), fr.Selector); err != nil {
				return err
			}
			if err := validateOptionalSelector(scope, fmt.Sprintf("fields[%q].closest", k), fr.Closest); err != nil {
				return err
			}
		}
		if s.Items != nil {
			if err := ValidateCSSSelector(s.Items.Container); err != nil {
				return fmt.Errorf("%s items.container %q: %w", scope, s.Items.Container, err)
			}
			if err := validateOptionalSelector(scope, "items.anchor_closest", s.Items.AnchorClosest); err != nil {
				return err
			}
			for k, fr := range s.Items.Fields {
				if err := validateOptionalSelector(scope, fmt.Sprintf("items.fields[%q].selector", k), fr.Selector); err != nil {
					return err
				}
				if err := validateOptionalSelector(scope, fmt.Sprintf("items.fields[%q].closest", k), fr.Closest); err != nil {
					return err
				}
			}
		}
		if s.Pagination != nil {
			if err := validateOptionalSelector(scope, "pagination.next_selector", s.Pagination.NextSelector); err != nil {
				return err
			}
			if err := validateOptionalSelector(scope, "pagination.wait_selector", s.Pagination.WaitSelector); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateResolvedTargets(r Recipe) error {
	if r.NormalizedDriver() == "uia" {
		for _, s := range r.Steps {
			u := strings.TrimSpace(s.URL)
			if u != "" && !strings.HasPrefix(strings.ToLower(u), "uia://") {
				return fmt.Errorf("uia step %q url %q must use uia:// scheme", s.ID, s.URL)
			}
		}
		return nil
	}

	var allowed []string
	for _, raw := range r.AllowedDomains {
		d := ExtractDomain(raw)
		if d == "" {
			return fmt.Errorf("recipe %q has invalid allowed_domain %q", r.Name, raw)
		}
		allowed = append(allowed, d)
	}

	for _, s := range r.Steps {
		if u := strings.TrimSpace(s.URL); u != "" {
			if err := validateHTTPURL(u); err != nil {
				return fmt.Errorf("step %q url %q: %w", s.ID, s.URL, err)
			}
			if len(allowed) > 0 {
				dom := ExtractDomain(u)
				if !domainInAllowlist(dom, allowed) {
					return fmt.Errorf("step %q url domain %q violates recipe allowed_domains", s.ID, dom)
				}
			}
		}
		if m := strings.TrimSpace(s.MatchTabURL); m != "" {
			dom := ExtractDomain(m)
			if dom == "" {
				return fmt.Errorf("step %q match_tab_url %q must specify an explicit HTTP/HTTPS host", s.ID, s.MatchTabURL)
			}
			if len(allowed) > 0 && !domainInAllowlist(dom, allowed) {
				return fmt.Errorf("step %q match_tab_url domain %q violates recipe allowed_domains", s.ID, dom)
			}
		}
	}
	return nil
}

func resolveFieldRules(scope string, in map[string]FieldRule, sub func(string) string) (map[string]FieldRule, error) {
	out := make(map[string]FieldRule, len(in))
	for k, rule := range in {
		rule.Selector = strings.TrimSpace(sub(rule.Selector))
		rule.Closest = strings.TrimSpace(sub(rule.Closest))
		rule.Attr = strings.TrimSpace(sub(rule.Attr))
		rule.Regex = sub(rule.Regex)
		rule.FallbackRegex = sub(rule.FallbackRegex)
		rule.Prefix = sub(rule.Prefix)
		rule.Suffix = sub(rule.Suffix)
		rule.Parse = strings.ToLower(strings.TrimSpace(rule.Parse))
		switch rule.Parse {
		case "", "string", "number", "float":
		default:
			return nil, fmt.Errorf("%s field %q has invalid parse mode %q (expected string, number, or float)", scope, k, rule.Parse)
		}
		if rule.Regex != "" {
			if _, err := regexp.Compile(rule.Regex); err != nil {
				return nil, fmt.Errorf("%s field %q has invalid regex %q: %w", scope, k, rule.Regex, err)
			}
		}
		if rule.FallbackRegex != "" {
			if _, err := regexp.Compile(rule.FallbackRegex); err != nil {
				return nil, fmt.Errorf("%s field %q has invalid fallback_regex %q: %w", scope, k, rule.FallbackRegex, err)
			}
		}
		out[k] = rule
	}
	return out, nil
}

// ValidateStepResult checks Expectations against an extracted StepResult.
func ValidateStepResult(s Step, res StepResult) error {
	lowerURL := strings.ToLower(res.URL)
	for _, bad := range s.Expect.NotURLContains {
		if bad != "" && strings.Contains(lowerURL, strings.ToLower(bad)) {
			return fmt.Errorf("step %q landed on blocked/login URL %q (matched %q)", s.ID, res.URL, bad)
		}
	}
	if len(s.Expect.MustContainText) > 0 {
		blob, _ := json.Marshal(res)
		lowerBlob := strings.ToLower(string(blob))
		for _, needle := range s.Expect.MustContainText {
			if needle != "" && !strings.Contains(lowerBlob, strings.ToLower(needle)) {
				return fmt.Errorf("step %q output missing expected text %q", s.ID, needle)
			}
		}
	}
	if s.Expect.MinTables > 0 && len(res.Tables) < s.Expect.MinTables {
		return fmt.Errorf("step %q extracted %d table(s), expected >= %d", s.ID, len(res.Tables), s.Expect.MinTables)
	}
	if s.Expect.MinMetrics > 0 && len(res.Metrics) < s.Expect.MinMetrics {
		return fmt.Errorf("step %q extracted %d metric(s), expected >= %d", s.ID, len(res.Metrics), s.Expect.MinMetrics)
	}
	if s.Expect.MinItems > 0 && len(res.Items) < s.Expect.MinItems {
		return fmt.Errorf("step %q extracted %d item(s), expected >= %d", s.ID, len(res.Items), s.Expect.MinItems)
	}
	if s.Expect.MinSelectorHits > 0 {
		nonEmpty := 0
		for _, v := range res.Selectors {
			if strings.TrimSpace(v) != "" {
				nonEmpty++
			}
		}
		if nonEmpty < s.Expect.MinSelectorHits {
			return fmt.Errorf("step %q matched %d non-empty selector(s), expected >= %d", s.ID, nonEmpty, s.Expect.MinSelectorHits)
		}
	}
	for _, reqField := range s.Expect.RequiredFields {
		val, ok := res.Fields[reqField]
		if !ok || isEmptyFieldValue(val) {
			return fmt.Errorf("step %q missing required field %q", s.ID, reqField)
		}
	}
	return nil
}

func isEmptyFieldValue(v any) bool {
	if v == nil {
		return true
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t) == ""
	default:
		return false
	}
}

func uniqueStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}

// StripJSONComments removes // line comments outside JSON string literals.
func StripJSONComments(src []byte) []byte {
	return stripJSONComments(src)
}

func stripJSONComments(src []byte) []byte {
	out := make([]byte, 0, len(src))
	inStr := false
	escaped := false
	for i := 0; i < len(src); i++ {
		c := src[i]
		if inStr {
			out = append(out, c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
			out = append(out, c)
			continue
		}
		if c == '/' && i+1 < len(src) && src[i+1] == '/' {
			for i < len(src) && src[i] != '\n' {
				i++
			}
			if i < len(src) {
				out = append(out, '\n')
			}
			continue
		}
		out = append(out, c)
	}
	return out
}
