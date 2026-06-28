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

	res, err := c.doRequest(ctx, http.MethodPost, "/login", body, "", true)
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
	body := encodeLowstate(cmds, true)
	res, err := c.authedExec(ctx, "/", body)
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
	low := make([]map[string]any, len(cmds))
	for i, cmd := range cmds {
		m := cmd.lowstate(true)
		m["username"] = c.user
		m["password"] = pw
		m["eauth"] = c.eauth
		low[i] = m
	}
	body, _ := json.Marshal(low)
	res, err := c.doRequest(ctx, http.MethodPost, "/run", body, "", false)
	if err != nil {
		return nil, err
	}
	if err := classify(res); err != nil {
		return nil, err
	}
	return decodeExecResponse(cmds, res.body)
}

// authedExec performs an authed POST with one automatic re-auth on auth failure.
func (c *Client) authedExec(ctx context.Context, p string, body []byte) (*httpResult, error) {
	tok, err := c.ensureToken(ctx)
	if err != nil {
		return nil, err
	}
	// Command POSTs are not auto-retried at the transport level (non-idempotent).
	res, err := c.doRequest(ctx, http.MethodPost, p, body, tok, false)
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
		c.mu.Lock()
		c.token = "" // force fresh login
		c.mu.Unlock()
		tok, lerr := c.login(ctx, true)
		if lerr != nil {
			return nil, lerr
		}
		res, err = c.doRequest(ctx, http.MethodPost, p, body, tok, false)
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
	res, err := c.doRequest(ctx, http.MethodGet, p, nil, tok, true)
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
			res, err = c.doRequest(ctx, http.MethodGet, p, nil, tok, true)
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
func encodeLowstate(cmds []Command, fullReturn bool) []byte {
	low := make([]map[string]any, len(cmds))
	for i, cmd := range cmds {
		low[i] = cmd.lowstate(fullReturn)
	}
	b, _ := json.Marshal(low)
	return b
}
