// Package automation talks to a hosted automation instance: deploying scripts, starting runs,
// and reading what they produced.
//
// SCRIPTS ARE BUNDLED HERE, exactly like functions and for the same reason — the agent's goja
// runtime resolves no imports, so a script has to arrive as one flat ES module. Doing it in the
// CLI rather than the Worker keeps a 10MB bundler out of the edge and means what you ran locally
// with `altengine-worker run` is byte-for-byte what the machine in the office will run.
//
// THE ORG API KEY LIVES HERE AND ONLY HERE. It never goes near an enrolled machine: an agent
// authenticates as itself with a credential issued at enrollment, because "the machine" in this
// service is a receptionist's PC.
package automation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Config is everything needed to reach one automation instance.
type Config struct {
	BaseURL  string
	APIKey   string
	Instance string
}

type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (c Config) do(method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	full := strings.TrimRight(c.BaseURL, "/") + "/v1/automation/" + url.PathEscape(c.Instance) + path
	req, err := http.NewRequest(method, full, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Generous, for the same reason the functions deploy is: a large bundle on a slow link is a
	// legitimately slow request, and a timeout here leaves someone unsure whether it landed.
	client := &http.Client{Timeout: 60 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, full, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 400 {
		var ae apiError
		if json.Unmarshal(raw, &ae) == nil && ae.Error.Message != "" {
			return fmt.Errorf("%s (%s)", ae.Error.Message, ae.Error.Code)
		}
		return fmt.Errorf("%s %s: %s: %s", method, full, res.Status, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("unexpected response: %w", err)
		}
	}
	return nil
}

// --- scripts --------------------------------------------------------------

type DeployResult struct {
	Name      string `json:"name"`
	Version   int    `json:"version"`
	SizeBytes int    `json:"size_bytes"`
	Active    bool   `json:"active"`
}

type deployBody struct {
	Name string `json:"name"`
	Code string `json:"code"`
	// Parallel is the AUTHOR'S declaration that this script drives nothing but browsers and
	// HTTP. A pointer so an ordinary redeploy leaves it alone: silently flipping a
	// desktop-driving script to parallel would put two of them on one screen, typing into each
	// other's windows.
	Parallel *bool `json:"parallel,omitempty"`
	Activate *bool `json:"activate,omitempty"`
}

func (c Config) Deploy(name, code string, parallel *bool, activate bool) (*DeployResult, error) {
	body := deployBody{Name: name, Code: code, Parallel: parallel}
	if !activate {
		body.Activate = &activate
	}
	var r DeployResult
	if err := c.do(http.MethodPost, "/scripts", body, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

type Script struct {
	Name          string `json:"name"`
	Parallel      bool   `json:"parallel"`
	ActiveVersion int    `json:"active_version"`
	UpdatedAt     int64  `json:"updated_at"`
}

func (c Config) Scripts() ([]Script, error) {
	var out struct {
		Scripts []Script `json:"scripts"`
	}
	err := c.do(http.MethodGet, "/scripts", nil, &out)
	return out.Scripts, err
}

type Version struct {
	Version   int    `json:"version"`
	SizeBytes int    `json:"size_bytes"`
	CodeHash  string `json:"code_hash"`
	CreatedAt int64  `json:"created_at"`
}

func (c Config) Versions(name string) ([]Version, int, error) {
	var out struct {
		Script   Script    `json:"script"`
		Versions []Version `json:"versions"`
	}
	err := c.do(http.MethodGet, "/scripts/"+url.PathEscape(name), nil, &out)
	return out.Versions, out.Script.ActiveVersion, err
}

func (c Config) Activate(name string, version int) error {
	return c.do(http.MethodPost, "/scripts/"+url.PathEscape(name)+"/activate",
		map[string]any{"version": version}, nil)
}

// --- agents ---------------------------------------------------------------

type Agent struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Hostname     string   `json:"hostname"`
	OS           string   `json:"os"`
	AgentVersion string   `json:"agent_version"`
	Labels       []string `json:"labels"`
	Online       bool     `json:"online"`
	// LastSeen is when the machine was last HEARD FROM, not when it was noticed missing.
	LastSeen int64 `json:"last_seen"`
	// NeverConnected is a THIRD state, not a shade of offline. Offline is a machine that worked
	// and stopped, which somebody should go and look at; this is a credential nobody redeemed.
	// Its hostname, OS and agent version stay blank until it first connects, because a machine
	// reports those about itself.
	NeverConnected bool `json:"never_connected"`
	// Liveness says what Online rests on, and is present only while online.
	//
	// "reporting" is a machine that answered inside HeardWithinSeconds — one that stops is
	// dropped from the roster within that window. "unverified" is a socket the control plane is
	// holding and nothing else, because that machine's agent is too old to report: a PC that was
	// switched off reads as online until its credential expires. Printing the second as the first
	// is reporting a guess as a fact about exactly the machines somebody is checking on.
	Liveness string `json:"liveness"`
	// HeardWithinSeconds is the window "reporting" is a guarantee over. Server-side and subject
	// to change, which is why it is published rather than assumed.
	HeardWithinSeconds int `json:"heard_within_seconds"`
}

// Agents lists EVERY enrolled machine, walking the pages.
//
// The endpoint is paged, and a fleet of hundreds is the ordinary shape of this service — an
// office, a chain of dealerships. A caller that read the first page as the whole fleet would
// print a shorter list with nothing to say it was short, which is the failure the cursor exists
// to prevent. `q` matches name, hostname or label.
func (c Config) Agents(q string) ([]Agent, error) {
	var all []Agent
	cursor := ""
	for {
		var out struct {
			Agents []Agent `json:"agents"`
			Cursor string  `json:"cursor"`
		}
		path := "/agents"
		v := url.Values{}
		if q != "" {
			v.Set("q", q)
		}
		if cursor != "" {
			v.Set("cursor", cursor)
		}
		if len(v) > 0 {
			path += "?" + v.Encode()
		}
		if err := c.do(http.MethodGet, path, nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Agents...)
		if out.Cursor == "" {
			return all, nil
		}
		cursor = out.Cursor
	}
}

// --- runs -----------------------------------------------------------------

type Run struct {
	ID        string  `json:"id"`
	Status    string  `json:"status"`
	Script    string  `json:"script"`
	Version   int     `json:"version"`
	AgentID   string  `json:"agent_id"`
	Error     string  `json:"error"`
	CostUSD   float64 `json:"cost_usd"`
	QueuedAt  int64   `json:"queued_at"`
	StartedAt int64   `json:"started_at"`
	EndedAt   int64   `json:"ended_at"`
}

type Artifact struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	URL       string `json:"url"`
}

type StartOptions struct {
	Script  string
	Params  map[string]string
	AgentID string
	Labels  []string
}

func (c Config) Start(o StartOptions) (*Run, error) {
	body := map[string]any{"script": o.Script}
	if len(o.Params) > 0 {
		body["params"] = o.Params
	}
	if o.AgentID != "" {
		body["agent_id"] = o.AgentID
	}
	if len(o.Labels) > 0 {
		body["labels"] = o.Labels
	}
	var out struct {
		Run Run `json:"run"`
	}
	if err := c.do(http.MethodPost, "/runs", body, &out); err != nil {
		return nil, err
	}
	return &out.Run, nil
}

func (c Config) Runs(limit int, status, script string) ([]Run, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", fmt.Sprint(limit))
	}
	if status != "" {
		q.Set("status", status)
	}
	if script != "" {
		q.Set("script", script)
	}
	var out struct {
		Runs []Run `json:"runs"`
	}
	err := c.do(http.MethodGet, "/runs?"+q.Encode(), nil, &out)
	return out.Runs, err
}

// Get returns one run and the files it produced, with freshly minted download links.
//
// The links are minted PER REQUEST and are short-lived, which is why they are not cached
// anywhere here: one that leaks from a terminal scrollback expires on its own.
func (c Config) Get(runID string) (*Run, []Artifact, error) {
	var out struct {
		Run       Run        `json:"run"`
		Artifacts []Artifact `json:"artifacts"`
	}
	err := c.do(http.MethodGet, "/runs/"+url.PathEscape(runID), nil, &out)
	return &out.Run, out.Artifacts, err
}

func (c Config) Cancel(runID string) error {
	return c.do(http.MethodPost, "/runs/"+url.PathEscape(runID)+"/cancel", map[string]any{}, nil)
}

// LogLine is one line of a run's output.
type LogLine struct {
	TS    int64  `json:"ts"`
	Level string `json:"level"`
	Msg   string `json:"msg"`
}

// LogPage is one page of a run's log.
type LogPage struct {
	Lines  []LogLine `json:"lines"`
	Cursor string    `json:"cursor"`
	// Source says whether these lines came from the agent's in-flight ring ("tail") or the
	// uploaded artifact. A live tail is not the same promise as the full record, and a reader
	// that cannot tell them apart will show a truncated log as though it were whole.
	Source string `json:"source"`
	// Complete says the log ends here. Dropped counts lines the agent or the ring discarded, so
	// a gap is visible rather than silently making a partial log look finished.
	Complete bool `json:"complete"`
	Dropped  int  `json:"dropped"`
}

func (c Config) Logs(runID, cursor string) (*LogPage, error) {
	q := url.Values{}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	var page LogPage
	err := c.do(http.MethodGet, "/runs/"+url.PathEscape(runID)+"/logs?"+q.Encode(), nil, &page)
	return &page, err
}

// --- inbound data ---------------------------------------------------------

// Delivery reports where a value went, in the machines' own words.
//
// Delivered counts runs whose machine CONFIRMED it. Unreachable is nothing written at all, so a
// retry is safe. Undetermined is the third answer and the one that has to stay separate: written
// to an open socket and not acknowledged. Reported as delivered it is a lie, and reported as
// unreachable it invites the retry that spends a second one-time code while the first may already
// be in the job's mailbox.
type Delivery struct {
	Key          string   `json:"key"`
	Delivered    int      `json:"delivered"`
	Runs         []string `json:"runs"`
	Unreachable  []string `json:"unreachable"`
	Undetermined []string `json:"undetermined"`
}

// Send hands a value to whichever job is waiting for it — job.waitForData(key) on the other end.
//
// ADDRESSED BY KEY, not by run id: the thing that will actually deliver a one-time code — an SMS
// webhook, an inbound-mail parser — was configured long before tonight's run existed. That puts a
// duty on the key, since the value reaches every job executing on the instance and only one that
// asks for that key ever sees it. `script` and `agent` narrow it.
//
// The body is the value itself rather than an envelope, because the poster is usually forwarding
// what it just parsed and wrapping it would mean unwrapping it again in the script.
func (c Config) Send(key string, value any, script, agent string) (*Delivery, error) {
	q := url.Values{}
	if script != "" {
		q.Set("script", script)
	}
	if agent != "" {
		q.Set("agent_id", agent)
	}
	path := "/data/" + url.PathEscape(key)
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out Delivery
	err := c.do(http.MethodPost, path, value, &out)
	return &out, err
}

// --- credentials ----------------------------------------------------------

// Env lists the NAMES of the credentials this fleet's scripts read as env.NAME. Values are
// write-only and are never returned, so a leaked key can replace one but not harvest it.
func (c Config) Env() ([]string, error) {
	var out struct {
		Env []string `json:"env"`
	}
	err := c.do(http.MethodGet, "/env", nil, &out)
	return out.Env, err
}

// SetEnv replaces the credential map. Omitting a name deletes it; a nil value KEEPS what is
// stored, which is the only way to rotate one without re-typing the others nobody can read.
func (c Config) SetEnv(env map[string]*string) ([]string, error) {
	var out struct {
		Env []string `json:"env"`
	}
	err := c.do(http.MethodPut, "/env", map[string]any{"env": env}, &out)
	return out.Env, err
}
