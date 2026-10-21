package papurika

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/cstrahan/papurika/config"
	"github.com/cstrahan/papurika/internal/tokencache"
)

// tokenSkew is how far before nominal expiry we proactively re-authenticate.
const tokenSkew = 30 * time.Second

// Client is a salt-api client. It is safe for concurrent use. Construct one with
// New or NewFromConfig.
type Client struct {
	base   string // master URL without trailing slash
	httpc  *http.Client
	user   string
	eauth  string
	secret config.SecretSource
	store  tokencache.Store
	key    string // token-cache key for this endpoint
	retry  RetryPolicy
	log    *slog.Logger

	// retryNonIdempotent lets command POSTs be re-sent on any transient
	// failure, not only on ones that prove the request never executed.
	retryNonIdempotent bool

	mu       sync.Mutex // guards token + single-flight login
	token    string
	loggedIn bool
}

type options struct {
	httpClient *http.Client
	insecure   bool
	caCert     string
	timeout    time.Duration
	eauth      string
	user       string
	secret     config.SecretSource
	store      tokencache.Store
	retry      RetryPolicy
	logger     *slog.Logger

	retryNonIdempotent bool
}

// Option configures a Client.
type Option func(*options)

func WithHTTPClient(c *http.Client) Option     { return func(o *options) { o.httpClient = c } }
func WithInsecureSkipVerify(v bool) Option     { return func(o *options) { o.insecure = v } }
func WithCACert(path string) Option            { return func(o *options) { o.caCert = path } }
func WithTimeout(d time.Duration) Option       { return func(o *options) { o.timeout = d } }
func WithEauth(backend string) Option          { return func(o *options) { o.eauth = backend } }
func WithTokenCache(s tokencache.Store) Option { return func(o *options) { o.store = s } }
func WithRetryPolicy(p RetryPolicy) Option     { return func(o *options) { o.retry = p } }
func WithLogger(l *slog.Logger) Option         { return func(o *options) { o.logger = l } }

// WithRetryNonIdempotent allows command POSTs to be re-sent on any transient
// failure, not only on failures that prove the request never executed.
//
// Use it only for a Client whose commands are all safe to run more than once —
// read-only probes, or idempotent state applies. It is NOT safe in general:
// salt has no idempotency key, so when a submit fails ambiguously (a 500, a 502
// or 504 from a proxy, a read timeout) the master may already be running the
// job. Re-sending then starts a *second* execution under a second jid, and the
// first jid is never reported to anyone.
//
// This is a Client-level setting rather than a per-call one on purpose: "is
// every command I send through here safe to repeat" is a property of how the
// Client is used. For a mix — idempotent state applies alongside a one-shot
// reboot — use two Clients, so which one is in hand says which guarantee
// applies.
func WithRetryNonIdempotent() Option { return func(o *options) { o.retryNonIdempotent = true } }

// WithCredentials sets the username and secret source used at login.
func WithCredentials(user string, secret config.SecretSource) Option {
	return func(o *options) { o.user = user; o.secret = secret }
}

// New constructs a Client for the salt-api at masterURL.
func New(masterURL string, opts ...Option) (*Client, error) {
	o := options{timeout: 120 * time.Second, eauth: "pam"}
	for _, fn := range opts {
		fn(&o)
	}
	base := strings.TrimRight(masterURL, "/")
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		return nil, fmt.Errorf("papurika: master URL must include scheme: %q", masterURL)
	}

	httpc := o.httpClient
	if httpc == nil {
		var err error
		httpc, err = buildHTTPClient(o.insecure, o.caCert, o.timeout)
		if err != nil {
			return nil, err
		}
	}
	logger := o.logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	store := o.store
	if store == nil {
		store = tokencache.NewFileStore(path.Join(config.CacheDir(), "tokens"))
	}
	return &Client{
		base:   base,
		httpc:  httpc,
		user:   o.user,
		eauth:  o.eauth,
		secret: o.secret,
		store:  store,
		key:    tokencache.Key(base, o.user, o.eauth),
		retry:  o.retry,
		log:    logger,

		retryNonIdempotent: o.retryNonIdempotent,
	}, nil
}

// NewFromConfig constructs a Client from a resolved config profile.
func NewFromConfig(cfg *config.Config, profile string, opts ...Option) (*Client, error) {
	r, err := cfg.Resolve(profile, config.Overrides{})
	if err != nil {
		return nil, err
	}
	base := []Option{
		WithCredentials(r.User, r.Secret),
		WithEauth(r.Eauth),
		WithInsecureSkipVerify(!r.VerifySSL),
		WithTimeout(r.Timeout),
	}
	if r.CACert != "" {
		base = append(base, WithCACert(r.CACert))
	}
	return New(r.MasterURL, append(base, opts...)...)
}

// Login authenticates and caches a session token. It is called implicitly by the
// other methods; call it explicitly to verify credentials up front.
func (c *Client) Login(ctx context.Context) error {
	_, err := c.login(ctx, true)
	return err
}

// token returns a valid session token, logging in (or reading the cache) if
// needed.
func (c *Client) ensureToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	tok := c.token
	c.mu.Unlock()
	if tok != "" {
		return tok, nil
	}
	if c.store != nil {
		if t, ok, _ := c.store.Get(ctx, c.key); ok && t.Valid(tokenSkew) {
			c.mu.Lock()
			c.token = t.Token
			c.mu.Unlock()
			return t.Token, nil
		}
	}
	return c.login(ctx, false)
}

// login performs POST /login. force re-authenticates even if a token is cached.
func (c *Client) login(ctx context.Context, force bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && c.token != "" {
		return c.token, nil // another goroutine won the race
	}
	if c.secret == nil {
		return "", fmt.Errorf("%w: no credentials configured", ErrAuthFailure)
	}
	pw, err := c.secret.Secret(ctx)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrAuthFailure, err)
	}
	body, _ := json.Marshal(map[string]string{"username": c.user, "password": pw, "eauth": c.eauth})

	res, err := c.doRequest(ctx, http.MethodPost, "/login", body, "", retryTransient)
	if err != nil {
		return "", err
	}
	if err := classify(res); err != nil {
		return "", err
	}
	tok, expire, perms, err := parseLogin(res.body)
	if err != nil {
		return "", err
	}
	c.token = tok
	c.loggedIn = true
	c.log.Info("authenticated", "url", c.base, "user", c.user, "eauth", c.eauth, "expire", expire)
	if c.store != nil {
		_ = c.store.Put(ctx, c.key, tokencache.Token{Token: tok, Expire: expire, Perms: perms})
	}
	return tok, nil
}

// parseLogin handles the /login return, which cherrypy emits as a one-element
// array and the docs describe as an object — accept both.
func parseLogin(body []byte) (token string, expire time.Time, perms []string, err error) {
	var env struct {
		Return json.RawMessage `json:"return"`
	}
	if e := json.Unmarshal(body, &env); e != nil {
		return "", time.Time{}, nil, apiError(ErrAuthFailure, 0, "", string(body), ModuleUnknown)
	}
	type tok struct {
		Token  string   `json:"token"`
		Expire float64  `json:"expire"`
		Perms  []string `json:"perms"`
	}
	var arr []tok
	var one tok
	if json.Unmarshal(env.Return, &arr) == nil && len(arr) > 0 {
		one = arr[0]
	} else if json.Unmarshal(env.Return, &one) != nil {
		return "", time.Time{}, nil, apiError(ErrAuthFailure, 0, "", string(body), ModuleUnknown)
	}
	if one.Token == "" {
		return "", time.Time{}, nil, apiError(ErrAuthFailure, 0, "", string(body), ModuleUnknown)
	}
	return one.Token, parseExpire(one.Expire), one.Perms, nil
}

// Exec runs one or more lowstate commands synchronously against POST / and
// returns the normalized result. full_return is injected for non-wheel commands.
// On an authentication failure it re-authenticates once and retries.
func (c *Client) Exec(ctx context.Context, cmds ...Command) (*Result, error) {
	if len(cmds) == 0 {
		return &Result{}, nil
	}
	body, err := encodeLowstate(cmds, true)
	if err != nil {
		return nil, err
	}
	res, err := c.authedExec(ctx, "/", body, c.commandRetryClass(cmds))
	if err != nil {
		return nil, err
	}
	return decodeExecResponse(cmds, res.body)
}

// ExecStateless runs commands via POST /run using the eauth trio (username/
// password/eauth) embedded per command — the portable stateless path (the
// /login session token is rejected by cherrypy's /run). No session token is used.
func (c *Client) ExecStateless(ctx context.Context, cmds ...Command) (*Result, error) {
	if len(cmds) == 0 {
		return &Result{}, nil
	}
	if c.secret == nil {
		return nil, fmt.Errorf("%w: no credentials configured", ErrAuthFailure)
	}
	pw, err := c.secret.Secret(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAuthFailure, err)
	}
	low, err := lowstates(cmds, true)
	if err != nil {
		return nil, err
	}
	for _, m := range low {
		m["username"] = c.user
		m["password"] = pw
		m["eauth"] = c.eauth
	}
	body, _ := json.Marshal(low)
	res, err := c.doRequest(ctx, http.MethodPost, "/run", body, "", c.commandRetryClass(cmds))
	if err != nil {
		return nil, err
	}
	if err := classify(res); err != nil {
		return nil, err
	}
	return decodeExecResponse(cmds, res.body)
}

// commandRetryClass is how a POST carrying cmds may be re-sent. By default only
// failures that prove the request never reached salt (see retryClass). It widens
// to any transient failure when the caller has declared the work safe to repeat
// — either fleet-wide via WithRetryNonIdempotent or per-command via
// Command.Idempotent.
//
// A multi-command POST is one indivisible request, so it is only repeatable when
// EVERY command in it is.
func (c *Client) commandRetryClass(cmds []Command) retryClass {
	if c.retryNonIdempotent {
		return retryTransient
	}
	for _, cmd := range cmds {
		if !cmd.Idempotent {
			return retryUnexecutedOnly
		}
	}
	if len(cmds) == 0 {
		return retryUnexecutedOnly
	}
	return retryTransient
}

// authedExec performs an authed POST with one automatic re-auth on auth failure.
func (c *Client) authedExec(ctx context.Context, p string, body []byte, class retryClass) (*httpResult, error) {
	tok, err := c.ensureToken(ctx)
	if err != nil {
		return nil, err
	}
	res, err := c.doRequest(ctx, http.MethodPost, p, body, tok, class)
	if err != nil {
		return nil, err
	}
	cerr := classify(res)
	if cerr == nil {
		return res, nil
	}
	// Re-auth exactly once on an authn failure (token expired/evicted). Authz
	// denial is never retried — re-login cannot grant a missing permission.
	if asAuthFailure(cerr) {
		c.log.Warn("token rejected; re-authenticating", "path", p)
		c.mu.Lock()
		c.token = "" // force fresh login
		c.mu.Unlock()
		tok, lerr := c.login(ctx, true)
		if lerr != nil {
			return nil, lerr
		}
		res, err = c.doRequest(ctx, http.MethodPost, p, body, tok, class)
		if err != nil {
			return nil, err
		}
		if cerr2 := classify(res); cerr2 != nil {
			return nil, cerr2
		}
		return res, nil
	}
	return nil, cerr
}

// authedGet performs an authed GET with transport retries (idempotent) and one
// re-auth on auth failure.
func (c *Client) authedGet(ctx context.Context, p string) (*httpResult, error) {
	tok, err := c.ensureToken(ctx)
	if err != nil {
		return nil, err
	}
	res, err := c.doRequest(ctx, http.MethodGet, p, nil, tok, retryTransient)
	if err != nil {
		return nil, err
	}
	if cerr := classify(res); cerr != nil {
		if asAuthFailure(cerr) {
			c.mu.Lock()
			c.token = ""
			c.mu.Unlock()
			tok, lerr := c.login(ctx, true)
			if lerr != nil {
				return nil, lerr
			}
			res, err = c.doRequest(ctx, http.MethodGet, p, nil, tok, retryTransient)
			if err != nil {
				return nil, err
			}
			if cerr2 := classify(res); cerr2 != nil {
				return nil, cerr2
			}
			return res, nil
		}
		return nil, cerr
	}
	return res, nil
}

func asAuthFailure(err error) bool {
	ae, ok := asAPIError(err)
	return ok && ae.Kind == ErrAuthFailure
}

// encodeLowstate marshals commands to the POST body, injecting full_return.
func encodeLowstate(cmds []Command, fullReturn bool) ([]byte, error) {
	low, err := lowstates(cmds, fullReturn)
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(low)
	return b, nil
}
