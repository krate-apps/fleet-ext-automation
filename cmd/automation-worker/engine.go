package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type whenClause struct {
	Label            string   `json:"label,omitempty"`
	LabelRegex       string   `json:"labelRegex,omitempty"`
	NameRegex        string   `json:"nameRegex,omitempty"`
	Private          *bool    `json:"private,omitempty"`
	Complete         *bool    `json:"complete,omitempty"`
	TrackerHostRegex string   `json:"trackerHostRegex,omitempty"`
	RatioMin         *float64 `json:"ratioMin,omitempty"`
	RatioMax         *float64 `json:"ratioMax,omitempty"`
	AgeSecMin        *int64   `json:"ageSecMin,omitempty"`
	StatusIn         []string `json:"statusIn,omitempty"`
	ClientKind       string   `json:"clientKind,omitempty"`
	ClientID         string   `json:"clientId,omitempty"`
}

type thenAction struct {
	Action   string `json:"action"`
	Label    string `json:"label,omitempty"`
	Name     string `json:"name,omitempty"`
	Dest     string `json:"dest,omitempty"`
	UpRate   int64  `json:"upRate,omitempty"`
	DownRate int64  `json:"downRate,omitempty"`
	URL      string `json:"url,omitempty"`
	Body     string `json:"body,omitempty"`
}

type rule struct {
	ID          string       `json:"id"`
	Priority    int          `json:"priority"`
	Enabled     *bool        `json:"enabled,omitempty"`
	StopOnMatch bool         `json:"stopOnMatch"`
	When        whenClause   `json:"when"`
	Then        []thenAction `json:"then"`
}

type torrentView struct {
	Hash       string
	Name       string
	Label      string
	State      string
	Ratio      float64
	Private    bool
	Complete   bool
	Directory  string
	FinishedAt int64
	ClientKind string // rtorrent|qbittorrent|deluge|transmission
	ClientID   string // local-rtorrent or gateway id
	Trackers   []string
}

type matchResult struct {
	RuleID  string
	Hash    string
	Name    string
	Actions []thenAction
	Stopped bool
}

func parseRulesText(text string) ([]rule, error) {
	out := make([]rule, 0)
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var r rule
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		if strings.TrimSpace(r.ID) == "" {
			r.ID = fmt.Sprintf("rule-%d", i+1)
		}
		if len(r.Then) == 0 {
			return nil, fmt.Errorf("line %d: then actions required", i+1)
		}
		for _, a := range r.Then {
			if err := validateAction(a); err != nil {
				return nil, fmt.Errorf("line %d: %w", i+1, err)
			}
		}
		if err := validateWhen(r.When); err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Priority < out[j].Priority
	})
	return out, nil
}

func validateWhen(w whenClause) error {
	for _, re := range []string{w.LabelRegex, w.NameRegex, w.TrackerHostRegex} {
		if re == "" {
			continue
		}
		if _, err := regexp.Compile("(?i)" + re); err != nil {
			return fmt.Errorf("invalid regex: %w", err)
		}
	}
	return nil
}

func validateAction(a thenAction) error {
	switch strings.ToLower(strings.TrimSpace(a.Action)) {
	case "setlabel":
		if strings.TrimSpace(a.Label) == "" {
			return fmt.Errorf("setLabel requires label")
		}
	case "moveto":
		if strings.TrimSpace(a.Dest) == "" {
			return fmt.Errorf("moveTo requires dest")
		}
	case "throttle":
		if strings.TrimSpace(a.Name) == "" {
			return fmt.Errorf("throttle requires name")
		}
	case "pause", "stop", "crossseedhint":
	case "notify":
		if strings.TrimSpace(a.URL) == "" {
			return fmt.Errorf("notify requires url")
		}
	default:
		return fmt.Errorf("unknown action %q", a.Action)
	}
	return nil
}

func ruleEnabled(r rule) bool {
	if r.Enabled == nil {
		return true
	}
	return *r.Enabled
}

func needsTrackers(rules []rule) bool {
	for _, r := range rules {
		if !ruleEnabled(r) {
			continue
		}
		if strings.TrimSpace(r.When.TrackerHostRegex) != "" {
			return true
		}
	}
	return false
}

func matchTorrent(rules []rule, t torrentView) []matchResult {
	out := make([]matchResult, 0)
	for _, r := range rules {
		if !ruleEnabled(r) {
			continue
		}
		ok, err := whenMatches(r.When, t)
		if err != nil || !ok {
			continue
		}
		out = append(out, matchResult{
			RuleID:  r.ID,
			Hash:    t.Hash,
			Name:    t.Name,
			Actions: append([]thenAction(nil), r.Then...),
			Stopped: r.StopOnMatch,
		})
		if r.StopOnMatch {
			break
		}
	}
	return out
}

func whenMatches(w whenClause, t torrentView) (bool, error) {
	if lab := strings.TrimSpace(w.Label); lab != "" {
		if !strings.EqualFold(strings.TrimSpace(t.Label), lab) {
			return false, nil
		}
	}
	if re := strings.TrimSpace(w.LabelRegex); re != "" {
		compiled, err := regexp.Compile("(?i)" + re)
		if err != nil || !compiled.MatchString(t.Label) {
			return false, err
		}
	}
	if re := strings.TrimSpace(w.NameRegex); re != "" {
		compiled, err := regexp.Compile("(?i)" + re)
		if err != nil || !compiled.MatchString(t.Name) {
			return false, err
		}
	}
	if w.Private != nil && t.Private != *w.Private {
		return false, nil
	}
	if w.Complete != nil && t.Complete != *w.Complete {
		return false, nil
	}
	if re := strings.TrimSpace(w.TrackerHostRegex); re != "" {
		compiled, err := regexp.Compile("(?i)" + re)
		if err != nil {
			return false, err
		}
		hit := false
		for _, tr := range t.Trackers {
			host := trackerHost(tr)
			if compiled.MatchString(tr) || (host != "" && compiled.MatchString(host)) {
				hit = true
				break
			}
		}
		if !hit {
			return false, nil
		}
	}
	if w.RatioMin != nil && t.Ratio < *w.RatioMin {
		return false, nil
	}
	if w.RatioMax != nil && t.Ratio > *w.RatioMax {
		return false, nil
	}
	if w.AgeSecMin != nil {
		if t.FinishedAt <= 0 {
			return false, nil
		}
		age := time.Now().Unix() - t.FinishedAt
		if age < *w.AgeSecMin {
			return false, nil
		}
	}
	if len(w.StatusIn) > 0 {
		st := strings.ToLower(strings.TrimSpace(t.State))
		ok := false
		for _, s := range w.StatusIn {
			if strings.ToLower(strings.TrimSpace(s)) == st {
				ok = true
				break
			}
		}
		if !ok {
			return false, nil
		}
	}
	if kind := strings.TrimSpace(w.ClientKind); kind != "" {
		have := strings.TrimSpace(t.ClientKind)
		if have == "" {
			have = "rtorrent"
		}
		if !strings.EqualFold(have, kind) {
			return false, nil
		}
	}
	if wantID := strings.TrimSpace(w.ClientID); wantID != "" {
		have := strings.TrimSpace(t.ClientID)
		if have == "" {
			have = "local-rtorrent"
		}
		if !strings.EqualFold(have, wantID) {
			return false, nil
		}
	}
	return true, nil
}

func trackerHost(raw string) string {
	u := strings.TrimSpace(raw)
	if u == "" {
		return ""
	}
	if !strings.Contains(u, "://") {
		u = "http://" + u
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return ""
	}
	return strings.ToLower(parsed.Hostname())
}

func rulesText(list []rule) string {
	if len(list) == 0 {
		return ""
	}
	var b strings.Builder
	for i, r := range list {
		raw, _ := json.Marshal(r)
		if i > 0 {
			b.WriteByte('\n')
		}
		b.Write(raw)
	}
	return b.String()
}

func expandPathTemplate(tmpl string, t torrentView) string {
	label := strings.TrimSpace(t.Label)
	if label == "" {
		label = "_nolabel"
	}
	replacer := strings.NewReplacer(
		"{hash}", t.Hash,
		"{name}", sanitizePathSegment(t.Name),
		"{label}", sanitizePathSegment(label),
	)
	return replacer.Replace(tmpl)
}

func sanitizePathSegment(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	s = strings.ReplaceAll(s, "..", "_")
	if s == "" {
		return "_"
	}
	return s
}

func pathUnderRoots(abs string, roots []string) bool {
	abs = filepath.Clean(abs)
	for _, root := range roots {
		root = filepath.Clean(strings.TrimSpace(root))
		if root == "" {
			continue
		}
		if abs == root || strings.HasPrefix(abs, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
