package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestResolveBaseURL(t *testing.T) {
	t.Setenv(envURL, "")
	if got := resolveBaseURL(""); got != defaultBaseURL {
		t.Errorf("default: got %q, want %q", got, defaultBaseURL)
	}
	if got := resolveBaseURL("http://x:1/"); got != "http://x:1" {
		t.Errorf("flag trailing slash: got %q", got)
	}
	t.Setenv(envURL, "http://env:2")
	if got := resolveBaseURL(""); got != "http://env:2" {
		t.Errorf("env: got %q", got)
	}
	if got := resolveBaseURL("http://flag:3"); got != "http://flag:3" {
		t.Errorf("flag beats env: got %q", got)
	}
}

func TestExtractStringFlag(t *testing.T) {
	// space form
	v, rest, err := extractStringFlag([]string{"a", "--reason", "foo", "b"}, "--reason")
	if err != nil || v != "foo" || len(rest) != 2 {
		t.Fatalf("space form: v=%q rest=%v err=%v", v, rest, err)
	}
	// equals form
	v, rest, err = extractStringFlag([]string{"--reason=bar", "x"}, "--reason")
	if err != nil || v != "bar" || len(rest) != 1 || rest[0] != "x" {
		t.Fatalf("equals form: v=%q rest=%v err=%v", v, rest, err)
	}
	// absent
	v, rest, err = extractStringFlag([]string{"x"}, "--reason")
	if err != nil || v != "" || len(rest) != 1 {
		t.Fatalf("absent: v=%q rest=%v err=%v", v, rest, err)
	}
	// missing value
	if _, _, err := extractStringFlag([]string{"--reason"}, "--reason"); err == nil {
		t.Fatal("expected error for missing value")
	}
}

func TestExtractBoolFlag(t *testing.T) {
	found, rest := extractBoolFlag([]string{"a", "--unblock", "b"}, "--unblock")
	if !found || len(rest) != 2 {
		t.Fatalf("found=%v rest=%v", found, rest)
	}
	found, rest = extractBoolFlag([]string{"a"}, "--unblock")
	if found || len(rest) != 1 {
		t.Fatalf("absent: found=%v rest=%v", found, rest)
	}
}

func TestEffectiveStatusAndExitCode(t *testing.T) {
	cases := []struct {
		name   string
		json   string
		status string
		exit   int
	}{
		{"standalone blocked", `{"status":"blocked"}`, "blocked", exitBlocked},
		{"standalone unblocked", `{"status":"unblocked"}`, "unblocked", exitOK},
		{"unknown", `{"cluster_status":"unknown","nodes":[{"name":"a","status":"unknown"}]}`, "unknown", exitNotFound},
		{"cluster blocked", `{"cluster_status":"blocked"}`, "blocked", exitBlocked},
		{"cluster mixed", `{"cluster_status":"mixed","nodes":[{"name":"a","status":"unblocked"},{"name":"b","status":"blocked"}]}`, "blocked", exitBlocked},
		{"node persistence blocked", `{"cluster_status":"unblocked","nodes":[{"name":"a","status":"unblocked","persistence":"blocked"}]}`, "blocked", exitBlocked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var s ipStatus
			if err := json.Unmarshal([]byte(tc.json), &s); err != nil {
				t.Fatal(err)
			}
			if got := s.effectiveStatus(); got != tc.status {
				t.Errorf("effectiveStatus: got %q, want %q", got, tc.status)
			}
			if got := statusExitCode(s.effectiveStatus()); got != tc.exit {
				t.Errorf("exit: got %d, want %d", got, tc.exit)
			}
		})
	}
}

func TestBadActorHistoryReasons(t *testing.T) {
	b := badActor{HistoryJSON: `[{"ts":"t","r":"chain-a"},{"ts":"t","r":"chain-a"},{"ts":"t","r":"chain-b"}]`}
	got := b.historyReasons()
	if len(got) != 2 || got[0] != "chain-a" || got[1] != "chain-b" {
		t.Fatalf("reasons: %v", got)
	}
	if !b.matchesReason("chain-b") || !b.matchesReason("chain-") {
		t.Fatal("expected substring matches")
	}
	if b.matchesReason("nope") {
		t.Fatal("unexpected match")
	}

	// null / empty history yields no reasons and no matches.
	for _, h := range []string{"null", "", "[]"} {
		nb := badActor{HistoryJSON: h}
		if len(nb.historyReasons()) != 0 || nb.matchesReason("x") {
			t.Fatalf("history %q should have no reasons", h)
		}
	}
}

func TestClientDoJSONAndError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"blocked"}`))
		case "/bad":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"Invalid IP address"}`))
		}
	}))
	defer srv.Close()

	c := newClient(srv.URL, 5*time.Second)

	var out ipStatus
	if _, err := c.doJSON("GET", "/ok", nil, &out); err != nil {
		t.Fatalf("doJSON ok: %v", err)
	}
	if out.Status != "blocked" {
		t.Errorf("decoded status: %q", out.Status)
	}

	_, err := c.do("GET", "/bad", nil)
	if err == nil {
		t.Fatal("expected error for 400")
	}
	he, ok := err.(*httpError)
	if !ok || he.status != http.StatusBadRequest {
		t.Fatalf("expected httpError 400, got %v", err)
	}
}

func TestRunUnknownCommand(t *testing.T) {
	if code := run([]string{"bogus", "cmd"}); code != exitError {
		t.Errorf("unknown command exit: got %d", code)
	}
}

func TestLookupCommand(t *testing.T) {
	if _, ok := lookupCommand("ip", "check"); !ok {
		t.Error("ip check should resolve")
	}
	if _, ok := lookupCommand("endpoints", ""); !ok {
		t.Error("endpoints (no subcommand) should resolve")
	}
	if _, ok := lookupCommand("ip", "nope"); ok {
		t.Error("ip nope should not resolve")
	}
}
