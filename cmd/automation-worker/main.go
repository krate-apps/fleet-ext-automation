package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	ID      any             `json:"id"`
}

type settings struct {
	Enabled     bool   `json:"enabled"`
	IntervalSec int    `json:"intervalSec"`
	DryRun      bool   `json:"dryRun"`
	RulesText   string `json:"rulesText"`
	EverEnabled bool   `json:"everEnabled"`
	Rules       []rule `json:"-"`
}

type ruleExample struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	RulesText   string `json:"rulesText"`
}

type logEntry struct {
	Time    string   `json:"time"`
	Hash    string   `json:"hash"`
	Name    string   `json:"name"`
	RuleID  string   `json:"ruleId"`
	Actions []string `json:"actions"`
	DryRun  bool     `json:"dryRun"`
	Error   string   `json:"error,omitempty"`
}

type hintEntry struct {
	Time string `json:"time"`
	Hash string `json:"hash"`
	Name string `json:"name"`
}

type hostReply struct {
	result json.RawMessage
	err    string
}

var (
	mu       sync.Mutex
	nextHost = int64(1)
	pending  = map[int64]chan hostReply{}
	outMu    sync.Mutex
)

const logLimit = 200
const hintLimit = 100

func main() {
	go loopScan()
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for in.Scan() {
		line := append([]byte(nil), in.Bytes()...)
		if len(line) == 0 {
			continue
		}
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(line, &probe); err != nil {
			continue
		}
		if _, hasMethod := probe["method"]; !hasMethod {
			var resp struct {
				ID     any             `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(line, &resp); err != nil {
				continue
			}
			idNum, ok := asInt64(resp.ID)
			if !ok {
				continue
			}
			mu.Lock()
			ch := pending[idNum]
			delete(pending, idNum)
			mu.Unlock()
			if ch == nil {
				continue
			}
			msg := ""
			if resp.Error != nil {
				msg = resp.Error.Message
			}
			ch <- hostReply{result: resp.Result, err: msg}
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}
		go handleRequest(req)
	}
}

func handleRequest(req request) {
	switch req.Method {
	case "fleet.handshake", "fleet.ping", "ext.activate", "ext.deactivate", "ext.health", "ext.upgrade":
		reply(req.ID, map[string]any{"ok": true})
	case "fleet.shutdown":
		reply(req.ID, map[string]any{"ok": true})
		os.Exit(0)
	case "ext.automation.getSettings":
		st, err := loadSettings()
		if err != nil {
			replyErr(req.ID, err.Error())
			return
		}
		reply(req.ID, publicSettings(st))
	case "ext.automation.saveSettings":
		handleSave(req.ID, req.Params)
	case "ext.automation.scan":
		res, err := scanOnce()
		if err != nil {
			replyErr(req.ID, err.Error())
			return
		}
		reply(req.ID, res)
	case "ext.automation.listLog":
		list, err := loadLog()
		if err != nil {
			replyErr(req.ID, err.Error())
			return
		}
		reply(req.ID, map[string]any{"entries": list})
	case "ext.automation.clearLog":
		if err := saveLog(nil); err != nil {
			replyErr(req.ID, err.Error())
			return
		}
		reply(req.ID, map[string]any{"ok": true})
	case "ext.automation.listHints":
		list, err := loadHints()
		if err != nil {
			replyErr(req.ID, err.Error())
			return
		}
		reply(req.ID, map[string]any{"hints": list})
	case "ext.automation.listExamples":
		reply(req.ID, map[string]any{"examples": ruleExamples()})
	case "ext.automation.appendExamples":
		st, err := appendExampleRules()
		if err != nil {
			replyErr(req.ID, err.Error())
			return
		}
		reply(req.ID, publicSettings(st))
	default:
		replyErr(req.ID, fmt.Sprintf("method not found: %s", req.Method))
	}
}

func handleSave(id any, params json.RawMessage) {
	var p struct {
		Enabled     bool   `json:"enabled"`
		IntervalSec int    `json:"intervalSec"`
		DryRun      bool   `json:"dryRun"`
		RulesText   string `json:"rulesText"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		replyErr(id, "invalid settings")
		return
	}
	rules, err := parseRulesText(p.RulesText)
	if err != nil {
		replyErr(id, err.Error())
		return
	}
	prev, _ := loadSettings()
	st := settings{
		Enabled:     p.Enabled,
		IntervalSec: p.IntervalSec,
		DryRun:      p.DryRun,
		RulesText:   rulesText(rules),
		Rules:       rules,
		EverEnabled: prev.EverEnabled,
	}
	if st.IntervalSec < 5 || st.IntervalSec > 3600 {
		st.IntervalSec = 15
	}
	// PD-RUL-01: first activation forces dry-run so operators can review the journal.
	if st.Enabled && !prev.EverEnabled {
		st.DryRun = true
		st.EverEnabled = true
	} else if st.Enabled {
		st.EverEnabled = true
	}
	if err := saveSettings(&st); err != nil {
		replyErr(id, err.Error())
		return
	}
	out := publicSettings(st)
	if st.Enabled && st.DryRun && !prev.EverEnabled {
		out["firstEnableDryRun"] = true
		out["notice"] = "First enable forces dry-run; review the application log, then disable dry-run explicitly."
	}
	reply(id, out)
}

func loopScan() {
	time.Sleep(3 * time.Second)
	for {
		st, err := loadSettings()
		interval := 15
		if err == nil && st.IntervalSec > 0 {
			interval = st.IntervalSec
		}
		if err == nil && st.Enabled {
			_, _ = scanOnce()
		}
		time.Sleep(time.Duration(interval) * time.Second)
	}
}

func scanOnce() (map[string]any, error) {
	st, err := loadSettings()
	if err != nil {
		return nil, err
	}
	raw, err := hostCall("fleet.torrents.list", map[string]any{"allClients": true})
	if err != nil {
		return nil, err
	}
	var payload struct {
		Torrents []struct {
			Hash       string  `json:"hash"`
			Name       string  `json:"name"`
			Label      string  `json:"label"`
			State      string  `json:"state"`
			Ratio      float64 `json:"ratio"`
			Private    bool    `json:"private"`
			Complete   bool    `json:"complete"`
			Directory  string  `json:"directory"`
			FinishedAt int64   `json:"finishedAt"`
			ClientID   string  `json:"clientId"`
		} `json:"torrents"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}

	kindOf := loadGatewayKinds()
	fetchTrackers := needsTrackers(st.Rules)
	matched := 0
	applied := 0
	hashes := make([]string, 0)
	byKind := map[string]int{}

	for _, row := range payload.Torrents {
		hash := strings.ToUpper(strings.TrimSpace(row.Hash))
		if hash == "" {
			continue
		}
		cid := strings.TrimSpace(row.ClientID)
		if cid == "" {
			cid = "local-rtorrent"
		}
		kind := kindOf(cid)
		if kind == "" {
			kind = "rtorrent"
		}
		tv := torrentView{
			Hash:       hash,
			Name:       row.Name,
			Label:      row.Label,
			State:      row.State,
			Ratio:      row.Ratio,
			Private:    row.Private,
			Complete:   row.Complete,
			Directory:  row.Directory,
			FinishedAt: row.FinishedAt,
			ClientKind: kind,
			ClientID:   cid,
		}
		if fetchTrackers && (cid == "local-rtorrent" || kind == "rtorrent") {
			tv.Trackers = listTrackerURLs(hash)
		}
		results := matchTorrent(st.Rules, tv)
		if len(results) == 0 {
			continue
		}
		matched += len(results)
		hashes = append(hashes, hash)
		byKind[kind]++
		for _, mr := range results {
			names := make([]string, 0, len(mr.Actions))
			var actErr error
			for _, a := range mr.Actions {
				names = append(names, strings.ToLower(a.Action))
				if st.DryRun || !st.Enabled {
					continue
				}
				if err := applyAction(a, tv); err != nil && actErr == nil {
					actErr = err
				} else if err == nil {
					applied++
				}
			}
			errMsg := ""
			if actErr != nil {
				errMsg = actErr.Error()
			}
			_ = appendLog(logEntry{
				Time:    time.Now().UTC().Format(time.RFC3339),
				Hash:    hash,
				Name:    tv.Name,
				RuleID:  mr.RuleID,
				Actions: names,
				DryRun:  st.DryRun || !st.Enabled,
				Error:   errMsg,
			})
		}
	}
	return map[string]any{
		"matched":   matched,
		"applied":   applied,
		"hashes":    unique(hashes),
		"dryRun":    st.DryRun || !st.Enabled,
		"count":     len(payload.Torrents),
		"byKind":    byKind,
		"allClients": true,
	}, nil
}

func loadGatewayKinds() func(clientID string) string {
	raw, err := hostCall("fleet.gateways.list", map[string]any{})
	kinds := map[string]string{"local-rtorrent": "rtorrent"}
	if err == nil {
		var wrap struct {
			Clients []struct {
				ID   string `json:"id"`
				Kind string `json:"kind"`
			} `json:"clients"`
		}
		_ = json.Unmarshal(raw, &wrap)
		for _, c := range wrap.Clients {
			id := strings.TrimSpace(c.ID)
			if id == "" {
				continue
			}
			kinds[id] = strings.ToLower(strings.TrimSpace(c.Kind))
		}
	}
	return func(clientID string) string {
		clientID = strings.TrimSpace(clientID)
		if clientID == "" {
			return "rtorrent"
		}
		if k, ok := kinds[clientID]; ok && k != "" {
			return k
		}
		return ""
	}
}

func actionHash(t torrentView) string {
	cid := strings.TrimSpace(t.ClientID)
	if cid == "" || cid == "local-rtorrent" {
		return t.Hash
	}
	return t.Hash + "@" + cid
}

func applyAction(a thenAction, t torrentView) error {
	switch strings.ToLower(strings.TrimSpace(a.Action)) {
	case "setlabel":
		params := map[string]any{"hash": t.Hash, "label": a.Label}
		if cid := strings.TrimSpace(t.ClientID); cid != "" {
			params["clientId"] = cid
		}
		_, err := hostCall("fleet.torrents.setLabel", params)
		return err
	case "throttle":
		if _, err := hostCall("fleet.throttle.define", map[string]any{
			"name": a.Name, "upRate": a.UpRate, "downRate": a.DownRate,
		}); err != nil {
			return err
		}
		_, err := hostCall("fleet.throttle.assign", map[string]any{
			"hashes": []string{actionHash(t)}, "name": a.Name,
		})
		return err
	case "pause":
		_, err := hostCall("fleet.torrents.action", map[string]any{"action": "pause", "hashes": []string{actionHash(t)}})
		return err
	case "stop":
		_, err := hostCall("fleet.torrents.action", map[string]any{"action": "stop", "hashes": []string{actionHash(t)}})
		return err
	case "notify":
		body := a.Body
		if body == "" {
			body = fmt.Sprintf("Fleet rules: %s (%s)", t.Name, t.Hash)
		}
		_, err := hostCall("fleet.http.fetch", map[string]any{
			"url":         a.URL,
			"method":      "POST",
			"body":        body,
			"contentType": "text/plain; charset=utf-8",
		})
		return err
	case "crossseedhint":
		return appendHint(hintEntry{
			Time: time.Now().UTC().Format(time.RFC3339),
			Hash: t.Hash,
			Name: t.Name,
		})
	case "moveto":
		return applyMoveTo(a.Dest, t)
	default:
		return fmt.Errorf("unknown action %q", a.Action)
	}
}

func applyMoveTo(destTmpl string, t torrentView) error {
	src := strings.TrimSpace(t.Directory)
	if src == "" {
		return fmt.Errorf("torrent has no directory")
	}
	rootsRaw, err := hostCall("fleet.files.roots", map[string]any{})
	if err != nil {
		return err
	}
	var rootsWrap struct {
		Roots []string `json:"roots"`
	}
	if err := json.Unmarshal(rootsRaw, &rootsWrap); err != nil {
		return err
	}
	if len(rootsWrap.Roots) == 0 {
		return fmt.Errorf("download_roots not configured")
	}
	dest := expandPathTemplate(strings.TrimSpace(destTmpl), t)
	if !filepath.IsAbs(dest) {
		dest = filepath.Join(rootsWrap.Roots[0], dest)
	}
	dest = filepath.Clean(dest)
	if !pathUnderRoots(dest, rootsWrap.Roots) {
		return fmt.Errorf("dest %q outside download_roots", dest)
	}
	if src == dest {
		return nil
	}
	if _, err := hostCall("fleet.files.move", map[string]any{"from": src, "to": dest}); err != nil {
		return err
	}
	// Best-effort: keep rTorrent pointing at the new path.
	if _, err := hostCall("fleet.torrents.setDirectory", map[string]any{"hash": t.Hash, "directory": dest}); err != nil {
		_ = appendLog(logEntry{
			Time:    time.Now().UTC().Format(time.RFC3339),
			Hash:    t.Hash,
			Name:    t.Name,
			RuleID:  "moveTo",
			Actions: []string{"setDirectory-skipped"},
			Error:   err.Error(),
		})
	}
	return nil
}

func listTrackerURLs(hash string) []string {
	raw, err := hostCall("fleet.trackers.list", map[string]any{"hash": hash})
	if err != nil {
		return nil
	}
	var payload struct {
		Trackers []struct {
			URL string `json:"url"`
		} `json:"trackers"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}
	out := make([]string, 0, len(payload.Trackers))
	for _, tr := range payload.Trackers {
		if u := strings.TrimSpace(tr.URL); u != "" {
			out = append(out, u)
		}
	}
	return out
}

func unique(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func defaultSettings() settings {
	return settings{Enabled: false, IntervalSec: 15, DryRun: true, EverEnabled: false}
}

func ruleExamples() []ruleExample {
	return []ruleExample{
		{
			ID:          "label-moveto",
			Title:       "Complete TV → moveTo",
			Description: "When complete with label tv, move under {label}/{name} (download_roots).",
			RulesText:   `{"id":"tv-moveto","when":{"complete":true,"label":"tv"},"then":[{"action":"moveTo","dest":"{label}/{name}"}]}`,
		},
		{
			ID:          "public-throttle",
			Title:       "Public → throttle",
			Description: "Non-private torrents get a named throttle profile.",
			RulesText:   `{"id":"public-throttle","when":{"private":false},"then":[{"action":"throttle","name":"public","upRate":100,"downRate":0}]}`,
		},
		{
			ID:          "finish-notify",
			Title:       "Finish → notify",
			Description: "Completed torrents fire a notify action (requires Notify channel configured).",
			RulesText:   `{"id":"finish-notify","when":{"complete":true},"then":[{"action":"notify","title":"Done","body":"{name} ({hash})"}]}`,
		},
	}
}

func appendExampleRules() (settings, error) {
	st, err := loadSettings()
	if err != nil {
		return settings{}, err
	}
	var b strings.Builder
	if strings.TrimSpace(st.RulesText) != "" {
		b.WriteString(strings.TrimRight(st.RulesText, "\n"))
		b.WriteString("\n\n")
	}
	b.WriteString("# Fleet Rules examples (PD-RUL-01) — review, then enable with dry-run first\n")
	for _, ex := range ruleExamples() {
		b.WriteString("# ")
		b.WriteString(ex.Title)
		b.WriteString(": ")
		b.WriteString(ex.Description)
		b.WriteByte('\n')
		b.WriteString(ex.RulesText)
		b.WriteByte('\n')
	}
	text := b.String()
	rules, err := parseRulesText(text)
	if err != nil {
		return settings{}, err
	}
	st.RulesText = text
	st.Rules = rules
	if err := saveSettings(&st); err != nil {
		return settings{}, err
	}
	return st, nil
}

func publicSettings(st settings) map[string]any {
	return map[string]any{
		"enabled":     st.Enabled,
		"intervalSec": st.IntervalSec,
		"dryRun":      st.DryRun,
		"rulesText":   st.RulesText,
		"ruleCount":   len(st.Rules),
		"everEnabled": st.EverEnabled,
	}
}

func loadSettings() (settings, error) {
	raw, err := hostCall("fleet.storage.get", map[string]any{"key": "settings"})
	if err != nil {
		return settings{}, err
	}
	if string(raw) == "null" || len(raw) == 0 {
		return defaultSettings(), nil
	}
	var st settings
	if err := json.Unmarshal(raw, &st); err != nil {
		return defaultSettings(), nil
	}
	rules, err := parseRulesText(st.RulesText)
	if err != nil {
		st.Rules = nil
	} else {
		st.Rules = rules
		st.RulesText = rulesText(rules)
	}
	if st.IntervalSec == 0 {
		st.IntervalSec = 15
	}
	return st, nil
}

func saveSettings(st *settings) error {
	wire := map[string]any{
		"enabled":     st.Enabled,
		"intervalSec": st.IntervalSec,
		"dryRun":      st.DryRun,
		"rulesText":   st.RulesText,
		"everEnabled": st.EverEnabled,
	}
	_, err := hostCall("fleet.storage.put", map[string]any{"key": "settings", "value": wire})
	return err
}

func loadLog() ([]logEntry, error) {
	raw, err := hostCall("fleet.storage.get", map[string]any{"key": "log"})
	if err != nil {
		return nil, err
	}
	if string(raw) == "null" || len(raw) == 0 {
		return []logEntry{}, nil
	}
	var list []logEntry
	_ = json.Unmarshal(raw, &list)
	return list, nil
}

func saveLog(list []logEntry) error {
	if list == nil {
		list = []logEntry{}
	}
	_, err := hostCall("fleet.storage.put", map[string]any{"key": "log", "value": list})
	return err
}

func appendLog(e logEntry) error {
	list, err := loadLog()
	if err != nil {
		return err
	}
	list = append([]logEntry{e}, list...)
	if len(list) > logLimit {
		list = list[:logLimit]
	}
	return saveLog(list)
}

func loadHints() ([]hintEntry, error) {
	raw, err := hostCall("fleet.storage.get", map[string]any{"key": "crossseedHints"})
	if err != nil {
		return nil, err
	}
	if string(raw) == "null" || len(raw) == 0 {
		return []hintEntry{}, nil
	}
	var list []hintEntry
	_ = json.Unmarshal(raw, &list)
	return list, nil
}

func saveHints(list []hintEntry) error {
	if list == nil {
		list = []hintEntry{}
	}
	_, err := hostCall("fleet.storage.put", map[string]any{"key": "crossseedHints", "value": list})
	return err
}

func appendHint(e hintEntry) error {
	list, err := loadHints()
	if err != nil {
		return err
	}
	list = append([]hintEntry{e}, list...)
	if len(list) > hintLimit {
		list = list[:hintLimit]
	}
	return saveHints(list)
}

func hostCall(method string, params any) (json.RawMessage, error) {
	ch := make(chan hostReply, 1)
	mu.Lock()
	id := nextHost
	nextHost++
	pending[id] = ch
	mu.Unlock()
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params, "id": id})
	if err != nil {
		mu.Lock()
		delete(pending, id)
		mu.Unlock()
		return nil, err
	}
	outMu.Lock()
	fmt.Fprintf(os.Stdout, "%s\n", b)
	outMu.Unlock()
	select {
	case reply := <-ch:
		if reply.err != "" {
			return nil, fmt.Errorf("%s", reply.err)
		}
		if len(reply.result) == 0 {
			return json.RawMessage("null"), nil
		}
		return reply.result, nil
	case <-time.After(60 * time.Second):
		mu.Lock()
		delete(pending, id)
		mu.Unlock()
		return nil, fmt.Errorf("host call timeout")
	}
}

func reply(id any, result any) {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "result": result, "id": id})
	outMu.Lock()
	fmt.Fprintf(os.Stdout, "%s\n", b)
	outMu.Unlock()
}

func replyErr(id any, msg string) {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"error":   map[string]any{"code": -32000, "message": msg},
		"id":      id,
	})
	outMu.Lock()
	fmt.Fprintf(os.Stdout, "%s\n", b)
	outMu.Unlock()
}

func asInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	default:
		return 0, false
	}
}
