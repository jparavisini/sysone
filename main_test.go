package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// env replaces getenv with a map lookup for one test.
func env(t *testing.T, m map[string]string) {
	t.Helper()
	old := getenv
	getenv = func(k string) string { return m[k] }
	t.Cleanup(func() { getenv = old })
}

// capture swaps stdio for one test and returns the stdout/stderr buffers.
func capture(t *testing.T, in string, tty bool) (*bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	oldOut, oldErr, oldIn := stdout, stderr, stdin
	oldOutTTY, oldInTTY, oldErrTTY := stdoutTTY, stdinTTY, stderrTTY
	out, errb := &bytes.Buffer{}, &bytes.Buffer{}
	stdout, stderr, stdin = out, errb, strings.NewReader(in)
	stdoutTTY, stdinTTY, stderrTTY = tty, in == "" && tty, false
	t.Cleanup(func() {
		stdout, stderr, stdin = oldOut, oldErr, oldIn
		stdoutTTY, stdinTTY, stderrTTY = oldOutTTY, oldInTTY, oldErrTTY
	})
	return out, errb
}

func noSleep(t *testing.T) {
	t.Helper()
	old := sleepFn
	sleepFn = func(time.Duration) {}
	t.Cleanup(func() { sleepFn = old })
}

func testResolved(url string) *Resolved {
	return &Resolved{Name: "t", URL: url, Timeout: 5 * time.Second, Threshold: 0.9, Concurrency: 4,
		keySource: literal("test-key")}
}

// ---------------------------------------------------------------- normalise

func TestNormalise(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	tests := []struct {
		name   string
		qtype  string
		raw    string
		answer any
		p      float64
		probs  map[string]float64
		expect *float64
		err    bool
	}{
		{"choice plain", typeChoice,
			`{"type":"choice","choice":"billing","probabilities":{"billing":0.91,"tech":0.06,"other":0.03},"confidence":0.91}`,
			"billing", 0.91, map[string]float64{"billing": 0.91, "tech": 0.06, "other": 0.03}, nil, false},
		{"choice answer_confidence wins over probabilities", typeChoice,
			`{"choice":"tech","probabilities":{"billing":0.4,"tech":0.6},"confidence":0.6,"answer_confidence":0.55,"routing":{"path":"fast"}}`,
			"tech", 0.55, map[string]float64{"billing": 0.4, "tech": 0.6}, nil, false},
		{"choice without choice field uses argmax", typeChoice,
			`{"probabilities":{"a":0.2,"b":0.8}}`, "b", 0.8, map[string]float64{"a": 0.2, "b": 0.8}, nil, false},
		{"choice confidence is not used when probabilities miss the label", typeChoice,
			`{"choice":"zzz","probabilities":{"a":0.2,"b":0.8},"confidence":0.9}`, "zzz", 0, map[string]float64{"a": 0.2, "b": 0.8}, nil, false},
		{"choice empty", typeChoice, `{}`, nil, 0, nil, nil, true},

		{"score plain", typeScore,
			`{"type":"score","probabilities":{"0":0.04,"1":0.10,"2":0.86},"score":2,"expected":1.82,"confidence":0.86}`,
			2, 0.86, map[string]float64{"0": 0.04, "1": 0.10, "2": 0.86}, f(1.82), false},
		{"score answer_confidence", typeScore,
			`{"score":1,"probabilities":{"0":0.3,"1":0.7},"answer_confidence":0.65}`, 1, 0.65, map[string]float64{"0": 0.3, "1": 0.7}, nil, false},
		{"score float rounds", typeScore, `{"score":1.0,"probabilities":{"0":0.3,"1":0.7}}`, 1, 0.7, map[string]float64{"0": 0.3, "1": 0.7}, nil, false},
		{"score argmax fallback", typeScore, `{"probabilities":{"0":0.3,"1":0.7}}`, 1, 0.7, map[string]float64{"0": 0.3, "1": 0.7}, nil, false},

		{"yesno plain", typeYesNo,
			`{"type":"noul","noul":0.94,"probability":0.94,"value":true,"confidence":0.94}`,
			true, 0.94, map[string]float64{"yes": 0.94, "no": 0.06}, nil, false},
		{"yesno no, p is P(no)", typeYesNo,
			`{"probability":0.2,"value":false,"confidence":0.8}`, false, 0.8, map[string]float64{"yes": 0.2, "no": 0.8}, nil, false},
		{"yesno value derived from probability", typeYesNo, `{"noul":0.3}`, false, 0.7, map[string]float64{"yes": 0.3, "no": 0.7}, nil, false},
		{"yesno only value and confidence", typeYesNo, `{"value":true,"confidence":0.77}`, true, 0.77, nil, nil, false},
		{"yesno answer_confidence wins", typeYesNo, `{"probability":0.9,"value":true,"answer_confidence":0.5}`, true, 0.5, map[string]float64{"yes": 0.9, "no": 0.1}, nil, false},
		{"yesno empty", typeYesNo, `{}`, nil, 0, nil, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, err := normalise("q", Question{Type: tc.qtype}, json.RawMessage(tc.raw))
			if tc.err {
				if err == nil {
					t.Fatalf("expected error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(a.Answer) != fmt.Sprint(tc.answer) {
				t.Errorf("answer = %v, want %v", a.Answer, tc.answer)
			}
			if fmt.Sprintf("%.4f", a.P) != fmt.Sprintf("%.4f", tc.p) {
				t.Errorf("p = %v, want %v", a.P, tc.p)
			}
			if fmt.Sprintf("%.2f", probsOrZero(a.Probabilities)) != fmt.Sprintf("%.2f", probsOrZero(tc.probs)) || len(a.Probabilities) != len(tc.probs) {
				t.Errorf("probabilities = %v, want %v", a.Probabilities, tc.probs)
			}
			if (a.Expected == nil) != (tc.expect == nil) || (tc.expect != nil && *a.Expected != *tc.expect) {
				t.Errorf("expected = %v, want %v", a.Expected, tc.expect)
			}
		})
	}
}

func probsOrZero(m map[string]float64) float64 {
	s := 0.0
	for _, v := range m {
		s += v
	}
	return s
}

// ------------------------------------------------------------------ retry

func TestRetry(t *testing.T) {
	tests := []struct {
		name     string
		statuses []int // per attempt; a 0 means close the connection
		headers  map[string]string
		wantCode int
		wantHits int
		wantWait time.Duration
	}{
		{"503 then 200 honours Retry-After", []int{503, 200}, map[string]string{"Retry-After": "2"}, 0, 2, 2 * time.Second},
		{"500 500 200", []int{500, 500, 200}, nil, 0, 3, 0},
		{"500 x3 gives up", []int{500, 500, 500}, nil, exitNetwork, 3, 0},
		{"401 no retry", []int{401}, nil, exitAuth, 1, 0},
		{"403 no retry", []int{403}, nil, exitAuth, 1, 0},
		{"422 no retry", []int{422}, nil, exitBadInput, 1, 0},
		{"413 no retry", []int{413}, nil, exitBadInput, 1, 0},
		{"404", []int{404}, nil, exitNetwork, 1, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/health" {
					fmt.Fprint(w, `{}`)
					return
				}
				n := int(hits.Add(1))
				if r.Header.Get("Authorization") != "Bearer test-key" {
					t.Errorf("missing bearer: %q", r.Header.Get("Authorization"))
				}
				st := tc.statuses[n-1]
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(st)
				if st == 200 {
					fmt.Fprint(w, `{"answers":{"q":{"value":true,"probability":0.95}}}`)
				} else {
					fmt.Fprint(w, `{"error":"nope"}`)
				}
			}))
			defer srv.Close()
			var waited time.Duration
			old := sleepFn
			sleepFn = func(d time.Duration) { waited += d }
			defer func() { sleepFn = old }()

			c, _ := newClient(testResolved(srv.URL))
			_, err := c.Ask(context.Background(), "s", []string{"q"}, map[string]Question{"q": {Type: typeYesNo, Instructions: "?"}})
			if got := int(hits.Load()); got != tc.wantHits {
				t.Errorf("hits = %d, want %d", got, tc.wantHits)
			}
			if tc.wantCode == 0 && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantCode != 0 && codeOf(err) != tc.wantCode {
				t.Errorf("exit = %d (%v), want %d", codeOf(err), err, tc.wantCode)
			}
			if tc.wantWait > 0 && waited != tc.wantWait {
				t.Errorf("waited %v, want %v", waited, tc.wantWait)
			}
			if err != nil && strings.Contains(err.Error(), "test-key") {
				t.Errorf("error text leaks key: %v", err)
			}
		})
	}
}

func TestRetryConnectionRefused(t *testing.T) {
	noSleep(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	c, _ := newClient(testResolved(url))
	_, err := c.Ask(context.Background(), "s", []string{"q"}, map[string]Question{"q": {Type: typeYesNo, Instructions: "?"}})
	if codeOf(err) != exitNetwork {
		t.Fatalf("exit = %d (%v), want %d", codeOf(err), err, exitNetwork)
	}
}

func TestBackoffGrows(t *testing.T) {
	a, b := backoff(2, nil), backoff(3, nil)
	if a < 300*time.Millisecond || a > 450*time.Millisecond {
		t.Errorf("attempt 2 backoff %v out of range", a)
	}
	if b < 600*time.Millisecond || b > 900*time.Millisecond {
		t.Errorf("attempt 3 backoff %v out of range", b)
	}
	if d := parseRetryAfter("3"); d != 3*time.Second {
		t.Errorf("Retry-After seconds = %v", d)
	}
	if d := parseRetryAfter(time.Now().Add(10 * time.Second).UTC().Format(http.TimeFormat)); d < 8*time.Second || d > 10*time.Second {
		t.Errorf("Retry-After date = %v", d)
	}
}

// ----------------------------------------------------------- config & auth

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResolvePrecedence(t *testing.T) {
	cfgPath := writeConfig(t, `
default_endpoint = "prod"
[endpoints.prod]
url = "http://cfg"
timeout = "7s"
threshold = 0.5
concurrency = 3
[endpoints.staging]
url = "http://staging"
`)
	tests := []struct {
		name      string
		env       map[string]string
		flags     globalFlags
		wantName  string
		wantURL   string
		wantTO    time.Duration
		wantThr   float64
		wantConc  int
		wantError int
	}{
		{"config only", nil, globalFlags{threshold: -1}, "prod", "http://cfg", 7 * time.Second, 0.5, 3, 0},
		{"env beats config", map[string]string{"SYSONE_URL": "http://env", "SYSONE_TIMEOUT": "9", "SYSONE_THRESHOLD": "0.6"},
			globalFlags{threshold: -1}, "prod", "http://env", 9 * time.Second, 0.6, 3, 0},
		{"flag beats env", map[string]string{"SYSONE_URL": "http://env", "SYSONE_TIMEOUT": "9", "SYSONE_THRESHOLD": "0.6"},
			globalFlags{url: "http://flag/", timeout: "1m", threshold: 0.7}, "prod", "http://flag", time.Minute, 0.7, 3, 0},
		{"endpoint from env", map[string]string{"SYSONE_ENDPOINT": "staging"}, globalFlags{threshold: -1}, "staging", "http://staging", 30 * time.Second, 0.9, 1, 0},
		{"endpoint from flag", map[string]string{"SYSONE_ENDPOINT": "staging"}, globalFlags{endpoint: "prod", threshold: -1}, "prod", "http://cfg", 7 * time.Second, 0.5, 3, 0},
		{"unknown endpoint", nil, globalFlags{endpoint: "nope", threshold: -1}, "", "", 0, 0, 0, exitUsage},
		{"unknown endpoint with --url is fine", nil, globalFlags{endpoint: "adhoc", url: "http://x", threshold: -1}, "adhoc", "http://x", 30 * time.Second, 0.9, 1, 0},
		{"bad threshold", nil, globalFlags{threshold: 1.5}, "", "", 0, 0, 0, exitUsage},
		{"bad timeout", nil, globalFlags{timeout: "soon", threshold: -1}, "", "", 0, 0, 0, exitUsage},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := map[string]string{"SYSONE_CONFIG": cfgPath}
			for k, v := range tc.env {
				m[k] = v
			}
			env(t, m)
			r, err := resolve(&tc.flags)
			if tc.wantError != 0 {
				if codeOf(err) != tc.wantError {
					t.Fatalf("exit = %d (%v), want %d", codeOf(err), err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if r.Name != tc.wantName || r.URL != tc.wantURL || r.Timeout != tc.wantTO || r.Threshold != tc.wantThr || r.Concurrency != tc.wantConc {
				t.Errorf("got %s %s %v %v %d, want %s %s %v %v %d", r.Name, r.URL, r.Timeout, r.Threshold, r.Concurrency,
					tc.wantName, tc.wantURL, tc.wantTO, tc.wantThr, tc.wantConc)
			}
		})
	}
}

func TestNoConfigNeedsURL(t *testing.T) {
	env(t, map[string]string{"SYSONE_CONFIG": filepath.Join(t.TempDir(), "missing.toml")})
	if _, err := resolve(&globalFlags{threshold: -1}); codeOf(err) != exitUsage {
		t.Fatalf("want usage error without url, got %v", err)
	}
	env(t, map[string]string{"SYSONE_CONFIG": filepath.Join(t.TempDir(), "missing.toml"), "SYSONE_URL": "http://x", "SYSONE_KEY": "k"})
	r, err := resolve(&globalFlags{threshold: -1})
	if err != nil {
		t.Fatal(err)
	}
	if k, _ := r.Key(); k != "k" || r.Name != "default" {
		t.Errorf("got %q %q", r.Name, k)
	}
}

func TestKeySources(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	os.WriteFile(keyFile, []byte("from-file\n"), 0o600)
	looseFile := filepath.Join(dir, "loose")
	os.WriteFile(looseFile, []byte("loose-key"), 0o644)

	tests := []struct {
		name    string
		ep      Endpoint
		env     map[string]string
		flags   globalFlags
		want    string
		wantErr int
		warn    bool
	}{
		{"key", Endpoint{Key: "plain"}, nil, globalFlags{}, "plain", 0, false},
		{"key_env", Endpoint{KeyEnv: "MY_KEY"}, map[string]string{"MY_KEY": "from-env"}, globalFlags{}, "from-env", 0, false},
		{"key_env unset", Endpoint{KeyEnv: "MY_KEY"}, nil, globalFlags{}, "", exitAuth, false},
		{"key_file", Endpoint{KeyFile: keyFile}, nil, globalFlags{}, "from-file", 0, false},
		{"key_file loose mode warns", Endpoint{KeyFile: looseFile}, nil, globalFlags{}, "loose-key", 0, true},
		{"key_file missing", Endpoint{KeyFile: filepath.Join(dir, "nope")}, nil, globalFlags{}, "", exitAuth, false},
		{"key_cmd first line only", Endpoint{KeyCmd: "printf 'from-cmd\\nurl: x\\n'"}, nil, globalFlags{}, "from-cmd", 0, false},
		{"key_cmd fails", Endpoint{KeyCmd: "exit 3"}, nil, globalFlags{}, "", exitAuth, false},
		{"key_cmd empty", Endpoint{KeyCmd: "true"}, nil, globalFlags{}, "", exitAuth, false},
		{"first set wins: key over key_cmd", Endpoint{Key: "plain", KeyCmd: "echo cmd"}, nil, globalFlags{}, "plain", 0, false},
		{"SYSONE_KEY beats config", Endpoint{Key: "plain"}, map[string]string{"SYSONE_KEY": "env"}, globalFlags{}, "env", 0, false},
		{"--key beats env", Endpoint{Key: "plain"}, map[string]string{"SYSONE_KEY": "env"}, globalFlags{key: "flag"}, "flag", 0, false},
		{"nothing set is empty", Endpoint{}, nil, globalFlags{}, "", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env(t, tc.env)
			_, errb := capture(t, "", false)
			r := &Resolved{keySource: keySource(&tc.flags, tc.ep)}
			got, err := r.Key()
			if tc.wantErr != 0 {
				if codeOf(err) != tc.wantErr {
					t.Fatalf("exit = %d (%v), want %d", codeOf(err), err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("key = %q, want %q", got, tc.want)
			}
			if tc.warn != strings.Contains(errb.String(), "chmod 600") {
				t.Errorf("warning = %q, want warn=%v", errb.String(), tc.warn)
			}
			if strings.Contains(errb.String(), got) && got != "" {
				t.Errorf("stderr leaks key: %q", errb.String())
			}
		})
	}
}

func TestKeyCachedPerProcess(t *testing.T) {
	var calls int
	r := &Resolved{keySource: func() (string, error) { calls++; return "k", nil }}
	r.Key()
	r.Key()
	if calls != 1 {
		t.Errorf("key source called %d times", calls)
	}
}

func TestConfigShowMasksKey(t *testing.T) {
	cfgPath := writeConfig(t, "[endpoints.default]\nurl = \"http://x\"\nkey = \"supersecret\"\nkey_cmd = \"pass show x\"\n")
	env(t, map[string]string{"SYSONE_CONFIG": cfgPath})
	out, _ := capture(t, "", false)
	if code := run([]string{"config", "show"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(out.String(), "supersecret") {
		t.Fatalf("config show leaked key:\n%s", out.String())
	}
	if !strings.Contains(out.String(), `key = "****"`) || !strings.Contains(out.String(), "pass show x") {
		t.Errorf("unexpected output:\n%s", out.String())
	}
}

func TestConfigSetRoundTrip(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "c.toml")
	env(t, map[string]string{"SYSONE_CONFIG": cfgPath})
	capture(t, "", false)
	run([]string{"config", "set", "endpoints.default.url", "http://one"})
	run([]string{"config", "set", "endpoints.default.threshold", "0.8"})
	run([]string{"config", "set", "endpoints.default.concurrency", "6"})
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	ep := cfg.Endpoints["default"]
	if ep.URL != "http://one" || ep.Threshold == nil || *ep.Threshold != 0.8 || ep.Concurrency != 6 {
		t.Errorf("got %+v", ep)
	}
}

// --------------------------------------------------------------- end to end

// answerServer answers every question with a fixed shape, delaying by the
// duration named in the state ("sleep:20ms") when present.
func answerServer(t *testing.T, yesP float64, choice string, choiceP float64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			fmt.Fprint(w, `{"ok":true,"model":"m1"}`)
			return
		}
		var req struct {
			State     any                       `json:"state"`
			Questions map[string]map[string]any `json:"questions"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if s, ok := req.State.(string); ok && strings.HasPrefix(s, "sleep:") {
			d, _ := time.ParseDuration(strings.TrimPrefix(s, "sleep:"))
			time.Sleep(d)
		}
		answers := map[string]any{}
		for id, q := range req.Questions {
			switch q["type"] {
			case wireYesNo:
				answers[id] = map[string]any{"probability": yesP, "value": yesP >= 0.5, "confidence": max(yesP, 1-yesP)}
			case typeChoice:
				answers[id] = map[string]any{"choice": choice, "probabilities": map[string]float64{choice: choiceP, "zz": 1 - choiceP}}
			case typeScore:
				answers[id] = map[string]any{"score": 1, "probabilities": map[string]float64{"0": 0.05, "1": 0.95}}
			default:
				w.WriteHeader(422)
				fmt.Fprintf(w, `{"error":"bad type %v"}`, q["type"])
				return
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"answers": answers, "model": "m1"})
	}))
}

func TestExitCodes(t *testing.T) {
	noSleep(t)
	yes := answerServer(t, 0.95, "billing", 0.95)
	defer yes.Close()
	no := answerServer(t, 0.05, "billing", 0.95)
	defer no.Close()
	unsure := answerServer(t, 0.6, "billing", 0.6)
	defer unsure.Close()
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer auth.Close()

	tests := []struct {
		name  string
		url   string
		args  []string
		stdin string
		want  int
		out   string
	}{
		{"gate yes", yes.URL, []string{"gate", "Is it?"}, "state", exitOK, "yes\n"},
		{"gate no", no.URL, []string{"gate", "Is it?"}, "state", exitNo, "no\n"},
		{"gate unsure", unsure.URL, []string{"gate", "Is it?"}, "state", exitAbstain, ""},
		{"gate lower threshold passes", unsure.URL, []string{"gate", "Is it?", "--threshold", "0.5"}, "state", exitOK, "yes\n"},
		{"pick", yes.URL, []string{"pick", "Which?", "billing=x", "zz=y"}, "state", exitOK, "billing\n"},
		{"pick with literal state", yes.URL, []string{"pick", "the state", "Which?", "billing", "zz"}, "", exitOK, "billing\n"},
		{"pick abstain", unsure.URL, []string{"pick", "Which?", "billing", "zz"}, "state", exitAbstain, ""},
		{"pick usage", yes.URL, []string{"pick", "Which?", "only-one"}, "state", exitUsage, ""},
		{"score", yes.URL, []string{"score", "How?", "low", "high"}, "state", exitOK, "1\n"},
		{"ask never exits 3", unsure.URL, []string{"ask", "--yesno", "q"}, "state", exitOK, ""},
		{"ask no questions", yes.URL, []string{"ask"}, "state", exitUsage, ""},
		{"no state", yes.URL, []string{"gate", "q"}, "", exitUsage, ""},
		{"auth", auth.URL, []string{"gate", "q"}, "state", exitAuth, ""},
		{"bad spec", yes.URL, []string{"ask", "-s", "does-not-exist"}, "state", exitBadInput, ""},
		{"unknown command", yes.URL, []string{"frob"}, "", exitUsage, ""},
		{"bad output format", yes.URL, []string{"gate", "q", "-o", "xml"}, "state", exitUsage, ""},
		{"help exits 0", yes.URL, []string{"gate", "--help"}, "", exitOK, ""},
		{"version", yes.URL, []string{"version"}, "", exitOK, "sysone dev\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env(t, map[string]string{"SYSONE_CONFIG": filepath.Join(t.TempDir(), "none.toml"), "SYSONE_URL": tc.url, "SYSONE_KEY": "k"})
			out, errb := capture(t, tc.stdin, false)
			args := append([]string{}, tc.args...)
			if tc.out != "" && args[0] != "version" {
				args = append(args, "-o", "value")
			}
			if got := run(args); got != tc.want {
				t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", got, tc.want, out, errb)
			}
			if tc.out != "" && out.String() != tc.out {
				t.Errorf("stdout = %q, want %q", out.String(), tc.out)
			}
			if tc.want == exitAbstain {
				if out.Len() != 0 || !strings.Contains(errb.String(), "best guess") {
					t.Errorf("abstain should print guess on stderr only; stdout=%q stderr=%q", out, errb)
				}
			}
			if strings.Contains(errb.String(), "Bearer k") || strings.Contains(out.String(), "Bearer k") {
				t.Errorf("key leaked")
			}
		})
	}
}

func TestAskJSONShape(t *testing.T) {
	srv := answerServer(t, 0.95, "billing", 0.95)
	defer srv.Close()
	env(t, map[string]string{"SYSONE_CONFIG": filepath.Join(t.TempDir(), "none.toml"), "SYSONE_URL": srv.URL, "SYSONE_KEY": "k"})
	out, errb := capture(t, "the state", false)
	code := run([]string{"ask", "--yesno", "late?", "--id", "late", "--choice", "team?", "--opt", "billing=b", "--opt", "zz=z",
		"--score", "sev", "--level", "l0", "--level", "l1", "-v"})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	var res Result
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Answers) != 3 || res.Answers[0].ID != "late" || res.Answers[1].ID != "q2" || res.Answers[2].ID != "q3" {
		t.Errorf("ids: %+v", res.Answers)
	}
	if res.Answers[1].Answer != "billing" || res.Answers[2].Answer != float64(1) || res.Model != "m1" || res.Abstain {
		t.Errorf("answers: %+v", res)
	}
	if !strings.Contains(errb.String(), "POST /v1/systemone -> 200") || strings.Contains(errb.String(), "k\n") {
		t.Errorf("verbose log: %q", errb.String())
	}
}

func TestBatchOrderingUnderConcurrency(t *testing.T) {
	srv := answerServer(t, 0.95, "billing", 0.95)
	defer srv.Close()
	specPath := filepath.Join(t.TempDir(), "s.json")
	os.WriteFile(specPath, []byte(`{"questions":{"ok":{"type":"yesno","instructions":"?"}}}`), 0o644)
	env(t, map[string]string{"SYSONE_CONFIG": filepath.Join(t.TempDir(), "none.toml"), "SYSONE_URL": srv.URL, "SYSONE_KEY": "k"})

	// Earlier lines sleep longer, so a naive pool would emit them last.
	var in strings.Builder
	const n = 12
	for i := 0; i < n; i++ {
		fmt.Fprintf(&in, `{"state":"sleep:%dms","i":%d}`+"\n", (n-i)*5, i)
	}
	in.WriteString("\n\"a raw string line\"\n{\"nostate\":1}\n")
	out, errb := capture(t, in.String(), false)
	if code := run([]string{"batch", "-q", specPath, "-j", "4", "--keep"}); code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != n+2 {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), n+2, out)
	}
	for i := 0; i < n; i++ {
		var rec struct {
			Input   map[string]any `json:"input"`
			Answers []Answer       `json:"answers"`
			Abstain bool           `json:"abstain"`
		}
		if err := json.Unmarshal([]byte(lines[i]), &rec); err != nil {
			t.Fatal(err)
		}
		if int(rec.Input["i"].(float64)) != i {
			t.Errorf("line %d has input i=%v (order broken)", i, rec.Input["i"])
		}
		if len(rec.Answers) != 1 || rec.Answers[0].Answer != true {
			t.Errorf("line %d answers %+v", i, rec.Answers)
		}
	}
	if !strings.Contains(lines[n], `"input":"a raw string line"`) {
		t.Errorf("raw string line: %s", lines[n])
	}
	if !strings.Contains(lines[n+1], `"error"`) || !strings.Contains(lines[n+1], `"line":14`) {
		t.Errorf("bad line should be an inline error: %s", lines[n+1])
	}
}

func TestBatchFailFastAndAuthStop(t *testing.T) {
	noSleep(t)
	var mu sync.Mutex
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(401)
	}))
	defer srv.Close()
	specPath := filepath.Join(t.TempDir(), "s.json")
	os.WriteFile(specPath, []byte(`{"questions":{"ok":{"type":"yesno","instructions":"?"}}}`), 0o644)
	env(t, map[string]string{"SYSONE_CONFIG": filepath.Join(t.TempDir(), "none.toml"), "SYSONE_URL": srv.URL, "SYSONE_KEY": "k"})
	out, _ := capture(t, strings.Repeat("\"x\"\n", 50), false)
	if code := run([]string{"batch", "-q", specPath, "-j", "1"}); code != exitAuth {
		t.Fatalf("exit = %d, want %d", code, exitAuth)
	}
	if !strings.Contains(out.String(), `"error"`) {
		t.Errorf("expected inline error line, got %q", out.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if hits >= 50 {
		t.Errorf("auth failure should stop the batch early; server hit %d times", hits)
	}
}

func TestBatchPerLineQuestionsOverride(t *testing.T) {
	var seen atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			fmt.Fprint(w, `{}`)
			return
		}
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		seen.Store(req["questions"])
		fmt.Fprint(w, `{"answers":{"custom":{"choice":"a","probabilities":{"a":0.99,"b":0.01}}}}`)
	}))
	defer srv.Close()
	specPath := filepath.Join(t.TempDir(), "s.json")
	os.WriteFile(specPath, []byte(`{"questions":{"ok":{"type":"yesno","instructions":"?"}}}`), 0o644)
	env(t, map[string]string{"SYSONE_CONFIG": filepath.Join(t.TempDir(), "none.toml"), "SYSONE_URL": srv.URL, "SYSONE_KEY": "k"})
	out, errb := capture(t, `{"state":"s","questions":{"custom":{"type":"choice","instructions":"?","criteria":{"a":"a","b":"b"}}}}`+"\n", false)
	if code := run([]string{"batch", "-q", specPath}); code != 0 {
		t.Fatalf("exit %d: %s", code, errb)
	}
	q := seen.Load().(map[string]any)
	if _, ok := q["custom"]; !ok || len(q) != 1 {
		t.Errorf("server saw questions %v", q)
	}
	if !strings.Contains(out.String(), `"answer":"a"`) {
		t.Errorf("output %s", out)
	}
}

func TestSpecParsing(t *testing.T) {
	tests := []struct {
		name string
		body string
		ok   bool
	}{
		{"full spec", `{"rules":["r"],"questions":{"a":{"type":"yesno","instructions":"?"}}}`, true},
		{"bare map", `{"a":{"type":"choice","instructions":"?","criteria":{"x":"1","y":"2"}}}`, true},
		{"noul alias", `{"a":{"type":"noul","instructions":"?"}}`, true},
		{"choice one option", `{"a":{"type":"choice","instructions":"?","criteria":{"x":"1"}}}`, false},
		{"score not a list", `{"a":{"type":"score","instructions":"?","criteria":{"x":"1"}}}`, false},
		{"empty instructions", `{"a":{"type":"yesno","instructions":" "}}`, false},
		{"unknown type", `{"a":{"type":"regress","instructions":"?"}}`, false},
		{"not json", `nope`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := parseSpec([]byte(tc.body), tc.name)
			if (err == nil) != tc.ok {
				t.Fatalf("ok=%v err=%v", tc.ok, err)
			}
			if tc.ok && s.Questions["a"].Type == wireYesNo {
				t.Errorf("noul not normalised to yesno")
			}
		})
	}
}

func TestStateValueMergesRules(t *testing.T) {
	if v, ok := stateValue(`{"a":1}`, []string{"r"}).(map[string]any); !ok || v["rules"] == nil || v["a"] != float64(1) {
		t.Errorf("object merge: %v", v)
	}
	if v, ok := stateValue("plain text", []string{"r"}).(map[string]any); !ok || v["text"] != "plain text" {
		t.Errorf("text wrap: %v", v)
	}
	if v := stateValue("plain text", nil); v != "plain text" {
		t.Errorf("no rules: %v", v)
	}
}

func TestExampleSpecsAndSkillsEmbedded(t *testing.T) {
	for _, n := range exampleSpecNames() {
		if _, err := parseSpec(exampleSpec(n), n); err != nil {
			t.Errorf("example spec %s: %v", n, err)
		}
	}
	if names := skillNames(); len(names) < 2 {
		t.Errorf("skills: %v", names)
	}
	for _, n := range skillNames() {
		b, _ := skillBody(n)
		if frontMatter(b, "name") != n || frontMatter(b, "description") == "" {
			t.Errorf("skill %s front matter", n)
		}
		if !strings.Contains(string(b), "threshold") {
			t.Errorf("skill %s must teach escalation below threshold", n)
		}
	}
	dir := t.TempDir()
	capture(t, "", false)
	if code := run([]string{"skills", "install", "--dir", dir}); code != 0 {
		t.Fatalf("install exit %d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "sysone", "SKILL.md")); err != nil {
		t.Error(err)
	}
	if code := run([]string{"skills", "install", "--agent", "codex", "--dir", dir}); code != 0 {
		t.Fatalf("codex install exit %d", code)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if !strings.Contains(string(b), agentsMarker) || strings.Contains(string(b), "---\nname:") {
		t.Errorf("AGENTS.md: %s", b)
	}
	if code := run([]string{"skills", "uninstall", "--dir", dir}); code != 0 {
		t.Fatalf("uninstall exit %d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "sysone")); err == nil {
		t.Error("skill dir still present after uninstall")
	}
}

func TestConfigInitInstallsExamples(t *testing.T) {
	dir := t.TempDir()
	env(t, map[string]string{"XDG_CONFIG_HOME": dir})
	capture(t, "", false)
	if code := run([]string{"config", "init"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, f := range []string{"config.toml", "specs/sentiment.json", "specs/ticket-triage.json"} {
		if _, err := os.Stat(filepath.Join(dir, "sysone", f)); err != nil {
			t.Error(err)
		}
	}
	fi, _ := os.Stat(filepath.Join(dir, "sysone", "config.toml"))
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("config mode %o", fi.Mode().Perm())
	}
	out, _ := capture(t, "", true)
	run([]string{"spec", "list"})
	if !strings.Contains(out.String(), "ticket-triage") {
		t.Errorf("spec list: %s", out)
	}
}

func TestHealth(t *testing.T) {
	srv := answerServer(t, 0.9, "a", 0.9)
	defer srv.Close()
	cfgPath := writeConfig(t, fmt.Sprintf("[endpoints.default]\nurl = %q\n[endpoints.down]\nurl = \"http://127.0.0.1:1\"\n", srv.URL))
	env(t, map[string]string{"SYSONE_CONFIG": cfgPath})
	noSleep(t)
	out, _ := capture(t, "", false)
	if code := run([]string{"health"}); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !strings.Contains(out.String(), `"model":"m1"`) {
		t.Errorf("health output: %s", out)
	}
	out, _ = capture(t, "", false)
	if code := run([]string{"health", "--all"}); code != exitNetwork {
		t.Fatalf("--all with a down endpoint: exit %d", code)
	}
	if strings.Count(out.String(), "\n") != 2 {
		t.Errorf("--all should print two rows: %s", out)
	}
}

func TestParseArgsInterspersed(t *testing.T) {
	var g globalFlags
	fs := newFlagSet("x", "")
	g.register(fs)
	pos, err := parseArgs(fs, []string{"a", "-o", "value", "b", "--raw", "c"})
	if err != nil || strings.Join(pos, ",") != "a,b,c" || g.output != "value" || !g.raw {
		t.Errorf("pos=%v out=%q raw=%v err=%v", pos, g.output, g.raw, err)
	}
}
