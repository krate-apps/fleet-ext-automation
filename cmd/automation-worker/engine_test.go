package main

import "testing"

func TestParseRulesTextAndPriority(t *testing.T) {
	text := `{"id":"b","priority":20,"when":{"private":false},"then":[{"action":"stop"}]}
{"id":"a","priority":10,"stopOnMatch":true,"when":{"private":false},"then":[{"action":"throttle","name":"slow","upRate":1}]}
`
	rules, err := parseRulesText(text)
	if err != nil || len(rules) != 2 {
		t.Fatalf("%v %v", rules, err)
	}
	if rules[0].ID != "a" || rules[1].ID != "b" {
		t.Fatalf("priority order %+v", rules)
	}
	privFalse := false
	tv := torrentView{Hash: "H1", Name: "x", Private: false, ClientKind: "rtorrent"}
	got := matchTorrent(rules, tv)
	if len(got) != 1 || got[0].RuleID != "a" || !got[0].Stopped {
		t.Fatalf("stopOnMatch %+v", got)
	}
	_ = privFalse
}

func TestWhenMatchesMatrix(t *testing.T) {
	priv := false
	w := whenClause{Private: &priv, NameRegex: "show", ClientKind: "rtorrent"}
	ok, err := whenMatches(w, torrentView{Name: "Show.S01", Private: false, ClientKind: "rtorrent"})
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	ok, _ = whenMatches(w, torrentView{Name: "Show.S01", Private: true, ClientKind: "rtorrent"})
	if ok {
		t.Fatal("private should block")
	}
	ok, _ = whenMatches(w, torrentView{Name: "Show.S01", Private: false, ClientKind: "qbittorrent"})
	if ok {
		t.Fatal("clientKind should block")
	}
}

func TestWhenMatchesClientID_successAndFailure(t *testing.T) {
	w := whenClause{ClientID: "gw-qbit", ClientKind: "qbittorrent"}
	ok, err := whenMatches(w, torrentView{
		Name: "x", ClientID: "gw-qbit", ClientKind: "qbittorrent",
	})
	if err != nil || !ok {
		t.Fatalf("want match: %v %v", ok, err)
	}
	ok, _ = whenMatches(w, torrentView{
		Name: "x", ClientID: "local-rtorrent", ClientKind: "qbittorrent",
	})
	if ok {
		t.Fatal("clientId should block")
	}
	// Dry-run multi-gateway: kind+id must both match when set.
	ok, _ = whenMatches(w, torrentView{
		Name: "x", ClientID: "gw-qbit", ClientKind: "deluge",
	})
	if ok {
		t.Fatal("clientKind should block even when clientId matches")
	}
}

func TestTrackerHostRegex(t *testing.T) {
	w := whenClause{TrackerHostRegex: `open\.cd`}
	ok, err := whenMatches(w, torrentView{Trackers: []string{"https://open.cd/announce"}})
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	ok, _ = whenMatches(w, torrentView{Trackers: []string{"https://other.example/a"}})
	if ok {
		t.Fatal("expected miss")
	}
}

func TestValidateUnknownAction(t *testing.T) {
	_, err := parseRulesText(`{"id":"x","when":{},"then":[{"action":"explode"}]}`)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestValidateMoveTo(t *testing.T) {
	_, err := parseRulesText(`{"id":"m","when":{"complete":true},"then":[{"action":"moveTo"}]}`)
	if err == nil {
		t.Fatal("expected dest required")
	}
	rules, err := parseRulesText(`{"id":"m","when":{"complete":true,"label":"tv"},"then":[{"action":"moveTo","dest":"{label}/{name}"}]}`)
	if err != nil || len(rules) != 1 {
		t.Fatalf("%v %v", rules, err)
	}
	ok, err := whenMatches(rules[0].When, torrentView{Label: "tv", Complete: true})
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	ok, _ = whenMatches(rules[0].When, torrentView{Label: "tv", Complete: false})
	if ok {
		t.Fatal("incomplete should miss")
	}
	got := expandPathTemplate("{label}/{name}", torrentView{Label: "tv", Name: "Show/S01"})
	if got != "tv/Show_S01" {
		t.Fatalf("got %q", got)
	}
	if !pathUnderRoots("/data/tv/x", []string{"/data"}) {
		t.Fatal("expected under root")
	}
	if pathUnderRoots("/other/tv", []string{"/data"}) {
		t.Fatal("expected outside")
	}
}

func TestDryRunConcept(t *testing.T) {
	// Pure match path does not require host; dry-run is a settings flag applied in scanOnce.
	rules, err := parseRulesText(`{"id":"pub","priority":1,"when":{"private":false},"then":[{"action":"throttle","name":"slow","upRate":10}]}`)
	if err != nil {
		t.Fatal(err)
	}
	got := matchTorrent(rules, torrentView{Hash: "ABC", Name: "t", Private: false})
	if len(got) != 1 || got[0].Actions[0].Action != "throttle" {
		t.Fatalf("%+v", got)
	}
}
