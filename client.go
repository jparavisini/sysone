package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Wire question types. The CLI says "yesno"; the server says "noul".
const (
	typeChoice = "choice"
	typeScore  = "score"
	typeYesNo  = "yesno"
	wireYesNo  = "noul"
)

// Question is one typed question, in CLI vocabulary (type "yesno").
// Criteria is a map[string]string for choice, []string for score, nil for yesno.
type Question struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

// Answer is the canonical, model-agnostic result for one question.
type Answer struct {
	ID            string             `json:"id"`
	Type          string             `json:"type"`
	Answer        any                `json:"answer"`
	P             float64            `json:"p"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Expected      *float64           `json:"expected,omitempty"`
	Abstain       bool               `json:"abstain,omitempty"`
	Model         string             `json:"model,omitempty"`
	LatencyMS     int64              `json:"latency_ms"`
	Raw           json.RawMessage    `json:"raw,omitempty"`
}

// Result is the answer set for one state.
type Result struct {
	Answers   []Answer `json:"answers"`
	Abstain   bool     `json:"abstain"`
	Model     string   `json:"model,omitempty"`
	LatencyMS int64    `json:"latency_ms"`
}

// sleepFn is swapped out in tests so retries do not wait.
var sleepFn = time.Sleep

// Client talks to one endpoint. Keep-alive connections are reused for the
// life of the process.
type Client struct {
	ep   *Resolved
	http *http.Client

	healthOnce  sync.Once
	healthModel string

	// Swapped out in tests.
	sleep func(time.Duration)
}

func newClient(ep *Resolved) (*Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = max(ep.Concurrency, 2)
	if ep.Insecure || ep.CAFile != "" {
		tc := &tls.Config{InsecureSkipVerify: ep.Insecure} //nolint:gosec // user opted in via config
		if ep.CAFile != "" {
			pem, err := os.ReadFile(expandHome(ep.CAFile))
			if err != nil {
				return nil, fail(exitUsage, "ca_file: %v", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fail(exitUsage, "ca_file %s: no certificates found", ep.CAFile)
			}
			tc.RootCAs = pool
		}
		tr.TLSClientConfig = tc
	}
	return &Client{
		ep:    ep,
		http:  &http.Client{Transport: tr, Timeout: ep.Timeout},
		sleep: sleepFn,
	}, nil
}

const maxTries = 3

// do performs one request with retries on connection errors, 503 and 5xx.
// 4xx is never retried. Returns the body and status of the final attempt.
func (c *Client) do(ctx context.Context, method, path string, body []byte, auth bool) ([]byte, int, error) {
	var lastErr error
	for attempt := 1; attempt <= maxTries; attempt++ {
		if attempt > 1 {
			c.sleep(backoff(attempt, lastErr))
		}
		resp, status, retryAfter, err := c.once(ctx, method, path, body, auth, attempt)
		if err == nil && status < 500 {
			return resp, status, nil
		}
		if err == nil {
			lastErr = &httpError{status: status, retryAfter: retryAfter, body: resp}
		} else {
			lastErr = err
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, 0, lastErr
}

// httpError is a 5xx response kept for retry decisions and final reporting.
type httpError struct {
	status     int
	retryAfter time.Duration
	body       []byte
}

func (e *httpError) Error() string {
	return fmt.Sprintf("server returned %d: %s", e.status, serverMessage(e.body))
}

// once performs a single attempt. It returns the body, status, any
// Retry-After delay, and a transport error.
func (c *Client) once(ctx context.Context, method, path string, body []byte, auth bool, attempt int) ([]byte, int, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.ep.URL+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, 0, fail(exitUsage, "bad request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "sysone/"+resolveVersion())
	for k, v := range c.ep.Headers {
		req.Header.Set(k, v)
	}
	if auth {
		key, err := c.ep.Key()
		if err != nil {
			return nil, 0, 0, err
		}
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
	}
	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		if c.ep.Verbose {
			fmt.Fprintf(stderr, "sysone: %s %s attempt %d: %v\n", method, path, attempt, err)
		}
		return nil, 0, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if c.ep.Verbose {
		fmt.Fprintf(stderr, "sysone: %s %s -> %d (%dms, attempt %d)\n",
			method, path, resp.StatusCode, time.Since(start).Milliseconds(), attempt)
	}
	if err != nil {
		return nil, 0, 0, err
	}
	return b, resp.StatusCode, parseRetryAfter(resp.Header.Get("Retry-After")), nil
}

// backoff returns a jittered exponential delay, or the server's Retry-After.
func backoff(attempt int, lastErr error) time.Duration {
	var he *httpError
	if errors.As(lastErr, &he) && he.retryAfter > 0 {
		return he.retryAfter
	}
	base := 300 * time.Millisecond * time.Duration(math.Pow(2, float64(attempt-2)))
	jitter := time.Duration(rand.Int64N(int64(base / 2)))
	return base + jitter
}

// parseRetryAfter handles seconds or an HTTP date.
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return time.Until(t)
	}
	return 0
}

// serverMessage extracts {"error": "..."} or trims the body.
func serverMessage(b []byte) string {
	var e struct {
		Error  string `json:"error"`
		Detail any    `json:"detail"`
	}
	if json.Unmarshal(b, &e) == nil {
		if e.Error != "" {
			return e.Error
		}
		if e.Detail != nil {
			d, _ := json.Marshal(e.Detail)
			return string(d)
		}
	}
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	if s == "" {
		return "(empty body)"
	}
	return s
}

// mapStatus converts a final error or non-2xx status into an exitError.
func mapStatus(status int, body []byte, err error) error {
	if err != nil {
		var he *httpError
		if errors.As(err, &he) {
			return fail(exitNetwork, "%v", he)
		}
		var ee *exitError
		if errors.As(err, &ee) {
			return err
		}
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return fail(exitNetwork, "request timed out: %v", err)
		}
		return fail(exitNetwork, "connection failed: %v", err)
	}
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == 401 || status == 403:
		return fail(exitAuth, "authentication failed (%d): %s", status, serverMessage(body))
	case status == 400 || status == 413 || status == 422:
		return fail(exitBadInput, "server rejected the request (%d): %s", status, serverMessage(body))
	case status == 404:
		return fail(exitNetwork, "not found (404): the endpoint URL may be wrong or the path unsupported")
	default:
		return fail(exitNetwork, "server returned %d: %s", status, serverMessage(body))
	}
}

// Health calls GET /health (no auth) and returns the raw JSON and latency.
func (c *Client) Health(ctx context.Context) (json.RawMessage, time.Duration, error) {
	start := time.Now()
	b, status, err := c.do(ctx, http.MethodGet, "/health", nil, false)
	lat := time.Since(start)
	if e := mapStatus(status, b, err); e != nil {
		return nil, lat, e
	}
	if !json.Valid(b) {
		b, _ = json.Marshal(map[string]string{"body": strings.TrimSpace(string(b))})
	}
	return b, lat, nil
}

// healthModelName fetches the model name from /health once per process.
// Used only when the answer response carries no model field.
func (c *Client) healthModelName(ctx context.Context) string {
	c.healthOnce.Do(func() {
		b, _, err := c.Health(ctx)
		if err != nil {
			return
		}
		var h struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(b, &h) == nil {
			c.healthModel = h.Model
		}
	})
	return c.healthModel
}

// Ask sends every question about one state in a single request and returns
// the normalised answers in the order of ids.
func (c *Client) Ask(ctx context.Context, state any, ids []string, qs map[string]Question) (*Result, error) {
	wire := make(map[string]map[string]any, len(qs))
	for id, q := range qs {
		w := map[string]any{"instructions": q.Instructions}
		switch q.Type {
		case typeYesNo:
			w["type"] = wireYesNo
		case typeChoice, typeScore:
			w["type"] = q.Type
			w["criteria"] = q.Criteria
		default:
			return nil, fail(exitBadInput, "question %q: unknown type %q (choice|yesno|score)", id, q.Type)
		}
		wire[id] = w
	}
	body, err := json.Marshal(map[string]any{"state": state, "questions": wire})
	if err != nil {
		return nil, fail(exitBadInput, "encode request: %v", err)
	}

	start := time.Now()
	b, status, err := c.do(ctx, http.MethodPost, "/v1/systemone", body, true)
	lat := time.Since(start).Milliseconds()
	if e := mapStatus(status, b, err); e != nil {
		return nil, e
	}

	var resp wireResponse
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, fail(exitNetwork, "unreadable response: %v", err)
	}
	model := resp.Model
	if model == "" {
		model = c.healthModelName(ctx)
	}

	res := &Result{Model: model, LatencyMS: lat}
	for _, id := range ids {
		rawAns, ok := resp.Answers[id]
		if !ok {
			return nil, fail(exitNetwork, "response has no answer for question %q", id)
		}
		a, err := normalise(id, qs[id], rawAns)
		if err != nil {
			return nil, err
		}
		a.Model, a.LatencyMS = model, lat
		a.Abstain = a.P < c.ep.Threshold
		if c.ep.Raw {
			a.Raw = rawAns
		}
		res.Abstain = res.Abstain || a.Abstain
		res.Answers = append(res.Answers, *a)
	}
	return res, nil
}

type wireResponse struct {
	Answers map[string]json.RawMessage `json:"answers"`
	Model   string                     `json:"model"`
}

// wireAnswer is the union of every field a server might send for one answer.
// Every field is optional; normalise never assumes one exists.
type wireAnswer struct {
	Choice           *string            `json:"choice"`
	Score            *json.Number       `json:"score"`
	Expected         *float64           `json:"expected"`
	Noul             *json.RawMessage   `json:"noul"`
	Probability      *float64           `json:"probability"`
	Value            *bool              `json:"value"`
	Probabilities    map[string]float64 `json:"probabilities"`
	Confidence       *float64           `json:"confidence"`
	AnswerConfidence *float64           `json:"answer_confidence"`
}

// normalise turns one server answer into the canonical Answer shape.
// p precedence: answer_confidence, then probabilities[answer], then
// confidence (yes/no only).
func normalise(id string, q Question, raw json.RawMessage) (*Answer, error) {
	var w wireAnswer
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&w); err != nil {
		return nil, fail(exitNetwork, "answer %q: %v", id, err)
	}
	a := &Answer{ID: id, Type: q.Type, Probabilities: w.Probabilities, Expected: w.Expected}

	switch q.Type {
	case typeChoice:
		label := ""
		if w.Choice != nil {
			label = *w.Choice
		} else if len(w.Probabilities) > 0 {
			label = argmax(w.Probabilities)
		} else {
			return nil, fail(exitNetwork, "answer %q: no choice or probabilities in response", id)
		}
		a.Answer = label
		a.P = pick(w.AnswerConfidence, lookup(w.Probabilities, label), nil)

	case typeScore:
		var idx int
		switch {
		case w.Score != nil:
			n, err := w.Score.Int64()
			if err != nil {
				f, ferr := w.Score.Float64()
				if ferr != nil {
					return nil, fail(exitNetwork, "answer %q: bad score %s", id, *w.Score)
				}
				n = int64(math.Round(f))
			}
			idx = int(n)
		case len(w.Probabilities) > 0:
			idx, _ = strconv.Atoi(argmax(w.Probabilities))
		default:
			return nil, fail(exitNetwork, "answer %q: no score or probabilities in response", id)
		}
		a.Answer = idx
		a.P = pick(w.AnswerConfidence, lookup(w.Probabilities, strconv.Itoa(idx)), nil)

	case typeYesNo:
		var pYes *float64
		if w.Probability != nil {
			pYes = w.Probability
		} else if w.Noul != nil {
			var f float64
			if json.Unmarshal(*w.Noul, &f) == nil {
				pYes = &f
			}
		}
		if pYes == nil && w.Probabilities != nil {
			if v, ok := w.Probabilities["yes"]; ok {
				pYes = &v
			} else if v, ok := w.Probabilities["true"]; ok {
				pYes = &v
			}
		}
		var yes bool
		switch {
		case w.Value != nil:
			yes = *w.Value
		case pYes != nil:
			yes = *pYes >= 0.5
		default:
			return nil, fail(exitNetwork, "answer %q: no value or probability in response", id)
		}
		a.Answer = yes
		if pYes != nil {
			a.Probabilities = map[string]float64{"yes": *pYes, "no": 1 - *pYes}
		}
		var fromProb *float64
		if pYes != nil {
			v := *pYes
			if !yes {
				v = 1 - v
			}
			fromProb = &v
		}
		a.P = pick(w.AnswerConfidence, fromProb, w.Confidence)
	}
	return a, nil
}

func pick(vals ...*float64) float64 {
	for _, v := range vals {
		if v != nil {
			return *v
		}
	}
	return 0
}

func lookup(m map[string]float64, k string) *float64 {
	if v, ok := m[k]; ok {
		return &v
	}
	return nil
}

func argmax(m map[string]float64) string {
	best, bestV := "", math.Inf(-1)
	for k, v := range m {
		if v > bestV || (v == bestV && k < best) {
			best, bestV = k, v
		}
	}
	return best
}
