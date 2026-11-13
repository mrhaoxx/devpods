// Copyright 2026 The DevPod Authors.
//
// SPDX-License-Identifier: Apache-2.0

// Package gwlink is how a gateway of several clusters asks the console
// what it used to read from the cluster it ran in: which DevPod a login
// or a port-link address means, and a user's keys. The gateway dials the
// console (never the reverse); every request is signed with the key the
// gateway joined with.
package gwlink

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	devpodv1alpha1 "github.com/mrhaoxx/devpod/api/v1alpha1"
	"github.com/mrhaoxx/devpod/internal/agent"
)

// Request headers of a signed request.
const (
	HeaderGateway   = "X-DevPod-Gateway"
	HeaderDate      = "X-DevPod-Date"
	HeaderSignature = "X-DevPod-Signature"
)

// signContext names what these signatures are for.
const signContext = "devpod gateway request v1"

// MaxSkew is how far a request's date may be from the console's clock:
// a request kept and sent again later is refused.
const MaxSkew = 60 * time.Second

func message(method, uri, date string, body []byte) []byte {
	sum := sha256.Sum256(body)
	return []byte(method + "\n" + uri + "\n" + date + "\n" + hex.EncodeToString(sum[:]))
}

// Sign signs r as gateway name. body is what r carries (nil for none).
func Sign(r *http.Request, body []byte, name string, id *agent.Identity, now time.Time) {
	date := strconv.FormatInt(now.Unix(), 10)
	r.Header.Set(HeaderGateway, name)
	r.Header.Set(HeaderDate, date)
	r.Header.Set(HeaderSignature, base64.StdEncoding.EncodeToString(id.SignFor(signContext, message(r.Method, r.URL.RequestURI(), date, body))))
}

// Verify checks a signed request against the key registered for the
// gateway it names (key returns it, or an error for a gateway that is
// unknown, paused or not joined) and returns that name.
func Verify(r *http.Request, body []byte, key func(name string) (string, error), now time.Time) (string, error) {
	name, date := r.Header.Get(HeaderGateway), r.Header.Get(HeaderDate)
	sig, err := base64.StdEncoding.DecodeString(r.Header.Get(HeaderSignature))
	if name == "" || date == "" || err != nil || len(sig) == 0 {
		return "", errors.New("not a signed gateway request")
	}
	sec, err := strconv.ParseInt(date, 10, 64)
	if err != nil {
		return "", errors.New("not a signed gateway request")
	}
	if d := now.Sub(time.Unix(sec, 0)); d > MaxSkew || d < -MaxSkew {
		return "", errors.New("the request's date is too far from the console's clock")
	}
	pub, err := key(name)
	if err != nil {
		return "", err
	}
	if err := agent.VerifyFor(pub, signContext, message(r.Method, r.URL.RequestURI(), date, body), sig); err != nil {
		return "", err
	}
	return name, nil
}

/* ---- the link a gateway replica keeps to the console ---- */

// The console tells a gateway two things about a session it holds: let
// an admin watch it, and end it. It cannot dial a gateway that runs
// elsewhere, so every replica keeps connections to the console and the
// console speaks to it over those.
const (
	// LinkKind is what a gateway calls itself in that handshake; its name
	// there is "<gateway>.<replica>".
	LinkKind = "gateway"
	// LinkContext is what its proof there is made for.
	LinkContext = "devpod gateway connect v1"
)

// PurposeLive: a stream that carries HTTP to the replica's live session
// endpoint.
var PurposeLive = []byte(`{"kind":"live"}`)

// PurposeSSH: a stream that is an SSH connection to the replica's
// gateway, as if made to its SSH port. The console's web terminal comes
// this way, and logs in as any other client does.
var PurposeSSH = []byte(`{"kind":"ssh"}`)

/* ---- what is asked and answered ---- */

// Found is a DevPod and the cluster it is in.
type Found struct {
	Cluster string                 `json:"cluster"`
	DevPod  *devpodv1alpha1.DevPod `json:"devpod"`
}

// Lookup asks which DevPods "<user>+<pod>" could mean, in Cluster or
// (empty) in every cluster.
type Lookup struct {
	User    string `json:"user"`
	Pod     string `json:"pod"`
	Cluster string `json:"cluster,omitempty"`
}

// Founds answers Lookup.
type Founds struct {
	Found []Found `json:"found"`
}

// Keys answers /gateway/v1/keys?user=: the SSH keys registered at the
// console (authorized_keys lines).
type Keys struct {
	Keys []string `json:"keys"`
}

// Cluster is a cluster whose agent a gateway accepts.
type Cluster struct {
	Name           string `json:"name"`
	AgentPublicKey string `json:"agentPublicKey"`
}

// Clusters answers /gateway/v1/clusters.
type Clusters struct {
	Clusters []Cluster `json:"clusters"`
}

// Report is what a gateway tells the console about itself.
type Report struct {
	Version  string   `json:"version,omitempty"`
	Clusters []string `json:"clusters"` // whose agents are connected to it
	// InternalKey: the key it signs into DevPods with (authorized_keys
	// line), for the DevPods of the clusters it serves to trust.
	InternalKey string `json:"internalKey,omitempty"`
}

// ReportAnswer is what the console says back to a report.
type ReportAnswer struct {
	// TrustedProxyKeys: the keys the console logs in with as a trusted
	// proxy (its web terminal), for the gateway to trust as it trusts
	// those its GatewayConfig lists.
	TrustedProxyKeys []devpodv1alpha1.TrustedProxyKey `json:"trustedProxyKeys,omitempty"`
}

/* ---- the gateway's side ---- */

// Errors a caller tells apart.
var (
	// ErrNotFound: the console answered that there is no such thing.
	ErrNotFound = errors.New("not found")
	// ErrConsoleAway: the console could not be asked, and there is no
	// answer recent enough to stand in.
	ErrConsoleAway = errors.New("the console cannot be reached")
)

// Options configures a Client.
type Options struct {
	Console  string // base URL, e.g. https://devpod.example.com
	Gateway  string // this gateway's registered name
	Identity *agent.Identity
	HTTP     *http.Client
	Now      func() time.Time
	// TTL: an answer is used this long without asking again — so a
	// withdrawn key or a removed collaborator stops working within it.
	// Grace: while the console cannot be reached, an answer no older than
	// this still serves. Defaults 5 s and 60 s.
	TTL, Grace time.Duration
}

// Client asks the console, remembering answers briefly.
type Client struct {
	o      Options
	mu     sync.Mutex
	m      map[string]*entry
	pruned time.Time
}

// maxRemembered is how many answers are kept before those past use are
// dropped.
const maxRemembered = 4096

type entry struct {
	at       time.Time
	raw      []byte
	notFound bool
}

// NewClient returns a Client.
func NewClient(o Options) *Client {
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 10 * time.Second}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.TTL <= 0 {
		o.TTL = 5 * time.Second
	}
	if o.Grace <= 0 {
		o.Grace = 60 * time.Second
	}
	o.Console = strings.TrimRight(o.Console, "/")
	return &Client{o: o, m: map[string]*entry{}}
}

// send makes one signed request. A transport error or a 5xx is the
// console being away; 404 is an answer.
func (c *Client) send(ctx context.Context, method, path string, body []byte) (raw []byte, notFound bool, err error) {
	req, err := http.NewRequestWithContext(ctx, method, c.o.Console+path, bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	Sign(req, body, c.o.Gateway, c.o.Identity, c.o.Now())
	resp, err := c.o.HTTP.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", ErrConsoleAway, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ = io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, true, nil
	case resp.StatusCode >= 500:
		return nil, false, fmt.Errorf("%w: %s", ErrConsoleAway, resp.Status)
	case resp.StatusCode >= 300:
		return nil, false, fmt.Errorf("the console refused the gateway: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	return raw, false, nil
}

// ask answers from memory within TTL, else asks; while the console is
// away, an answer within Grace stands in (stale).
func (c *Client) ask(ctx context.Context, method, path string, body []byte, out any) (stale bool, err error) {
	key := method + " " + path + " " + string(body)
	now := c.o.Now()
	c.mu.Lock()
	e := c.m[key]
	c.mu.Unlock()
	if e == nil || now.Sub(e.at) > c.o.TTL {
		raw, notFound, err := c.send(ctx, method, path, body)
		switch {
		case err == nil:
			e = &entry{at: now, raw: raw, notFound: notFound}
			c.mu.Lock()
			c.m[key] = e
			// Questions come from whoever knocks (a login is looked up
			// before its key is checked), so what is remembered is bounded:
			// past use first, and all of it if that is not enough.
			if len(c.m) > maxRemembered && now.Sub(c.pruned) > time.Second {
				c.pruned = now
				for k, v := range c.m {
					if now.Sub(v.at) > c.o.Grace {
						delete(c.m, k)
					}
				}
			}
			if len(c.m) > 4*maxRemembered {
				c.m = map[string]*entry{key: e}
			}
			c.mu.Unlock()
		case errors.Is(err, ErrConsoleAway) && e != nil && now.Sub(e.at) <= c.o.Grace:
			stale = true
		default:
			return false, err
		}
	}
	if e.notFound {
		return stale, ErrNotFound
	}
	return stale, json.Unmarshal(e.raw, out)
}

// Lookup asks which DevPods a login could mean.
func (c *Client) Lookup(ctx context.Context, user, pod, cluster string) ([]Found, bool, error) {
	body, _ := json.Marshal(Lookup{User: user, Pod: pod, Cluster: cluster})
	var out Founds
	stale, err := c.ask(ctx, http.MethodPost, "/gateway/v1/lookup", body, &out)
	return out.Found, stale, err
}

// ByHTTPID asks which DevPod a port-link id means; ErrNotFound if none.
func (c *Client) ByHTTPID(ctx context.Context, id string) (*Found, bool, error) {
	var out Found
	stale, err := c.ask(ctx, http.MethodGet, "/gateway/v1/http?id="+url.QueryEscape(id), nil, &out)
	if err != nil {
		return nil, stale, err
	}
	return &out, stale, nil
}

// Keys asks for a user's keys registered at the console; ErrNotFound for
// a user the console does not know.
func (c *Client) Keys(ctx context.Context, user string) ([]string, bool, error) {
	var out Keys
	stale, err := c.ask(ctx, http.MethodGet, "/gateway/v1/keys?user="+url.QueryEscape(user), nil, &out)
	return out.Keys, stale, err
}

// Clusters asks whose agents this gateway accepts.
func (c *Client) Clusters(ctx context.Context) ([]Cluster, bool, error) {
	var out Clusters
	stale, err := c.ask(ctx, http.MethodGet, "/gateway/v1/clusters", nil, &out)
	return out.Clusters, stale, err
}

// Report tells the console what this gateway is and serves (not cached),
// and returns what the console says back.
func (c *Client) Report(ctx context.Context, r Report) (ReportAnswer, error) {
	body, _ := json.Marshal(r)
	var ans ReportAnswer
	raw, _, err := c.send(ctx, http.MethodPost, "/gateway/v1/report", body)
	if err != nil {
		return ans, err
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		err = json.Unmarshal(raw, &ans)
	}
	return ans, err
}

// Join registers the gateway's public key with the one-time token.
func Join(ctx context.Context, hc *http.Client, console, gateway, token string, id *agent.Identity) error {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	body, _ := json.Marshal(map[string]string{"gateway": gateway, "token": token, "publicKey": id.PublicKey()})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(console, "/")+"/gateway/join", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	// 204 and nothing else: a console of an earlier version answers 200
	// to any path (its page), which is not a join
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return fmt.Errorf("the console answered %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
}
