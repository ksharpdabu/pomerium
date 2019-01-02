package authenticate // import "github.com/pomerium/pomerium/authenticate"

import (
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"net/url"
	"strings"
	"time"

	"github.com/pomerium/envconfig"

	"github.com/pomerium/pomerium/authenticate/providers"
	"github.com/pomerium/pomerium/internal/aead"
	"github.com/pomerium/pomerium/internal/sessions"
	"github.com/pomerium/pomerium/internal/templates"
)

// Options permits the configuration of the authentication service
type Options struct {
	// e.g.
	Host string `envconfig:"HOST"`
	//
	ProxyClientID     string `envconfig:"PROXY_CLIENT_ID"`
	ProxyClientSecret string `envconfig:"PROXY_CLIENT_SECRET"`

	// Coarse authorization based on user email domain
	EmailDomains     []string `envconfig:"SSO_EMAIL_DOMAIN"`
	ProxyRootDomains []string `envconfig:"PROXY_ROOT_DOMAIN"`

	// Session/Cookie management
	CookieName     string
	CookieSecret   string        `envconfig:"COOKIE_SECRET"`
	CookieDomain   string        `envconfig:"COOKIE_DOMAIN"`
	CookieExpire   time.Duration `envconfig:"COOKIE_EXPIRE" default:"168h"`
	CookieRefresh  time.Duration `envconfig:"COOKIE_REFRESH" default:"1h"`
	CookieSecure   bool          `envconfig:"COOKIE_SECURE" default:"true"`
	CookieHTTPOnly bool          `envconfig:"COOKIE_HTTP_ONLY" default:"true"`

	AuthCodeSecret string `envconfig:"AUTH_CODE_SECRET"`

	SessionLifetimeTTL time.Duration `envconfig:"SESSION_LIFETIME_TTL" default:"720h"`

	// Authentication provider configuration vars
	RedirectURL  *url.URL `envconfig:"IDP_REDIRECT_URL" ` // e.g. auth.example.com/oauth/callback
	ClientID     string   `envconfig:"IDP_CLIENT_ID"`     // IdP ClientID
	ClientSecret string   `envconfig:"IDP_CLIENT_SECRET"` // IdP Secret
	Provider     string   `envconfig:"IDP_PROVIDER"`      //Provider name e.g. "oidc","okta","google",etc
	ProviderURL  *url.URL `envconfig:"IDP_PROVIDER_URL"`
	Scopes       []string `envconfig:"IDP_SCOPE" default:"openid,email,profile"`

	// todo(bdd) : can delete?`
	ApprovalPrompt string        `envconfig:"IDP_APPROVAL_PROMPT" default:"consent"`
	RequestLogging bool          `envconfig:"REQUEST_LOGGING" default:"true"`
	RequestTimeout time.Duration `envconfig:"REQUEST_TIMEOUT" default:"2s"`
}

var defaultOptions = &Options{
	EmailDomains:       []string{"*"},
	CookieName:         "_pomerium_authenticate",
	CookieSecure:       true,
	CookieHTTPOnly:     true,
	CookieExpire:       time.Duration(168) * time.Hour,
	CookieRefresh:      time.Duration(1) * time.Hour,
	RequestTimeout:     time.Duration(2) * time.Second,
	SessionLifetimeTTL: time.Duration(720) * time.Hour,

	ApprovalPrompt: "consent",
	Scopes:         []string{"openid", "email", "profile"},
}

// OptionsFromEnvConfig builds the authentication service's configuration
// options from provided environmental variables
func OptionsFromEnvConfig() (*Options, error) {
	o := defaultOptions
	if err := envconfig.Process("", o); err != nil {
		return nil, err
	}
	return o, nil
}

// Validate checks to see if configuration values are valid for authentication service.
// The checks do not modify the internal state of the Option structure. Function returns
// on first error found.
func (o *Options) Validate() error {
	if o.ProviderURL == nil {
		return errors.New("missing setting: identity provider url")
	}
	if o.RedirectURL == nil {
		return errors.New("missing setting: identity provider redirect url")
	}
	redirectPath := "/oauth2/callback"
	if o.RedirectURL.Path != redirectPath {
		return fmt.Errorf("setting redirect-url was %s path should be %s", o.RedirectURL.Path, redirectPath)
	}
	if o.ClientID == "" {
		return errors.New("missing setting: client id")
	}
	if o.ClientSecret == "" {
		return errors.New("missing setting: client secret")
	}
	if len(o.EmailDomains) == 0 {
		return errors.New("missing setting email domain")
	}
	if len(o.ProxyRootDomains) == 0 {
		return errors.New("missing setting: proxy root domain")
	}
	if o.ProxyClientID == "" {
		return errors.New("missing setting: proxy client id")
	}
	if o.ProxyClientSecret == "" {
		return errors.New("missing setting: proxy client secret")
	}

	decodedCookieSecret, err := base64.StdEncoding.DecodeString(o.CookieSecret)
	if err != nil {
		return fmt.Errorf("authenticate options: cookie secret invalid"+
			"must be a base64-encoded, 256 bit key e.g. `head -c32 /dev/urandom | base64`"+
			"got %q", err)
	}

	validCookieSecretLength := false
	for _, i := range []int{32, 64} {
		if len(decodedCookieSecret) == i {
			validCookieSecretLength = true
		}
	}

	if !validCookieSecretLength {
		return fmt.Errorf("authenticate options: invalid cookie secret strength want 32 to 64 bytes, got %d bytes", len(decodedCookieSecret))
	}

	if o.CookieRefresh >= o.CookieExpire {
		return fmt.Errorf("cookie_refresh (%s) must be less than cookie_expire (%s)", o.CookieRefresh.String(), o.CookieExpire.String())
	}

	return nil
}

// Authenticator stores all the information associated with proxying the request.
type Authenticator struct {
	Validator func(string) bool

	EmailDomains     []string
	ProxyRootDomains []string
	Host             string
	CookieSecure     bool

	ProxyClientID     string
	ProxyClientSecret string

	SessionLifetimeTTL time.Duration

	decodedCookieSecret []byte
	templates           *template.Template
	// sesion related
	csrfStore    sessions.CSRFStore
	sessionStore sessions.SessionStore
	cipher       aead.Cipher

	redirectURL *url.URL
	provider    providers.Provider
}

// NewAuthenticator creates a Authenticator struct and applies the optional functions slice to the struct.
func NewAuthenticator(opts *Options, optionFuncs ...func(*Authenticator) error) (*Authenticator, error) {
	if opts == nil {
		return nil, errors.New("options cannot be nil")
	}
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	decodedAuthCodeSecret, err := base64.StdEncoding.DecodeString(opts.AuthCodeSecret)
	if err != nil {
		return nil, err
	}
	cipher, err := aead.NewMiscreantCipher([]byte(decodedAuthCodeSecret))
	if err != nil {
		return nil, err
	}
	decodedCookieSecret, err := base64.StdEncoding.DecodeString(opts.CookieSecret)
	if err != nil {
		return nil, err
	}
	cookieStore, err := sessions.NewCookieStore(opts.CookieName,
		sessions.CreateMiscreantCookieCipher(decodedCookieSecret),
		func(c *sessions.CookieStore) error {
			c.CookieDomain = opts.CookieDomain
			c.CookieHTTPOnly = opts.CookieHTTPOnly
			c.CookieExpire = opts.CookieExpire
			c.CookieSecure = opts.CookieSecure
			return nil
		})

	if err != nil {
		return nil, err
	}

	p := &Authenticator{
		ProxyClientID:     opts.ProxyClientID,
		ProxyClientSecret: opts.ProxyClientSecret,
		EmailDomains:      opts.EmailDomains,
		ProxyRootDomains:  dotPrependDomains(opts.ProxyRootDomains),
		CookieSecure:      opts.CookieSecure,
		redirectURL:       opts.RedirectURL,
		templates:         templates.New(),
		csrfStore:         cookieStore,
		sessionStore:      cookieStore,
		cipher:            cipher,
	}
	// p.ServeMux = p.Handler()
	p.provider, err = newProvider(opts)
	if err != nil {
		return nil, err
	}

	// apply the option functions
	for _, optFunc := range optionFuncs {
		err := optFunc(p)
		if err != nil {
			return nil, err
		}
	}
	return p, nil
}

func newProvider(opts *Options) (providers.Provider, error) {
	pd := &providers.ProviderData{
		RedirectURL:        opts.RedirectURL,
		ProviderName:       opts.Provider,
		ClientID:           opts.ClientID,
		ClientSecret:       opts.ClientSecret,
		ApprovalPrompt:     opts.ApprovalPrompt,
		SessionLifetimeTTL: opts.SessionLifetimeTTL,
		ProviderURL:        opts.ProviderURL,
		Scopes:             opts.Scopes,
	}
	np, err := providers.New(opts.Provider, pd)
	if err != nil {
		return nil, err
	}
	return providers.NewSingleFlightProvider(np), nil

}

func dotPrependDomains(d []string) []string {
	for i := range d {
		if d[i] != "" && !strings.HasPrefix(d[i], ".") {
			d[i] = fmt.Sprintf(".%s", d[i])
		}
	}
	return d
}