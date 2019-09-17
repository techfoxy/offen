package router

import (
	"errors"
	"net/http"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/securecookie"
	"github.com/m90/go-thunk"
	"github.com/offen/offen/server/persistence"
	"github.com/sirupsen/logrus"
)

type router struct {
	db                   persistence.Database
	logger               *logrus.Logger
	cookieSigner         *securecookie.SecureCookie
	secureCookie         bool
	cookieExchangeSecret []byte
	retentionPeriod      time.Duration
}

func (rt *router) logError(err error, message string) {
	if rt.logger != nil {
		rt.logger.WithError(err).Error(message)
	}
}

type contextKey int

const (
	cookieKey                   = "user"
	optoutKey                   = "optout"
	authKey                     = "auth"
	contextKeyCookie contextKey = iota
	contextKeyAuth
)

func (rt *router) userCookie(userID string) *http.Cookie {
	return &http.Cookie{
		Name:     cookieKey,
		Value:    userID,
		Expires:  time.Now().Add(rt.retentionPeriod),
		HttpOnly: true,
		Secure:   rt.secureCookie,
		Path:     "/",
	}
}

func (rt *router) optoutCookie(optout bool) *http.Cookie {
	c := &http.Cookie{
		Name:  optoutKey,
		Value: "1",
		// the optout cookie is supposed to outlive the software, so
		// it expires in ~100 years
		Expires: time.Now().Add(time.Hour * 24 * 365 * 100),
		Path:    "/",
		// this cookie is supposed to be read by the client so it can
		// stop operating before even sending requests
		HttpOnly: false,
		SameSite: http.SameSiteDefaultMode,
	}
	if !optout {
		c.Expires = time.Unix(0, 0)
	}
	return c
}

func (rt *router) authCookie(userID string) (*http.Cookie, error) {
	c := http.Cookie{
		Name:     authKey,
		HttpOnly: true,
		SameSite: http.SameSiteDefaultMode,
	}
	if userID == "" {
		c.Expires = time.Unix(0, 0)
	} else {
		value, err := rt.cookieSigner.MaxAge(24*60*60).Encode(authKey, userID)
		if err != nil {
			return nil, err
		}
		c.Value = value
	}
	return &c, nil

}

// Config adds a configuration value to the router
type Config func(*router)

// WithDatabase sets the database the router will use
func WithDatabase(db persistence.Database) Config {
	return func(r *router) {
		r.db = db
	}
}

// WithLogger sets the logger the router will use
func WithLogger(l *logrus.Logger) Config {
	return func(r *router) {
		r.logger = l
	}
}

// WithSecureCookie determines whether the application will issue
// secure (HTTPS-only) cookies
func WithSecureCookie(sc bool) Config {
	return func(r *router) {
		r.secureCookie = sc
	}
}

// WithCookieExchangeSecret sets the secret to be used for signing secured
// cookie exchange requests
func WithCookieExchangeSecret(b []byte) Config {
	return func(r *router) {
		r.cookieExchangeSecret = b
	}
}

// WithRetentionPeriod sets the expected value for retaining event data
func WithRetentionPeriod(d time.Duration) Config {
	return func(r *router) {
		r.retentionPeriod = d
	}
}

// New creates a new application router that reads and writes data
// to the given database implementation. In the context of the application
// this expects to be the only top level router in charge of handling all
// incoming HTTP requests.
func New(opts ...Config) http.Handler {
	rt := router{}
	for _, opt := range opts {
		opt(&rt)
	}

	rt.cookieSigner = securecookie.New([]byte(rt.cookieExchangeSecret), nil)
	m := mux.NewRouter()

	dropOptout := optoutMiddleware(optoutKey)
	recovery := thunk.HandleSafelyWith(func(err error) {
		if rt.logger != nil {
			rt.logger.WithError(err).Error("Internal server error")
		}
	})
	userCookie := userCookieMiddleware(cookieKey, contextKeyCookie)
	accountAuth := rt.accountUserMiddleware(authKey, contextKeyAuth)

	m.Use(recovery)

	optout := m.PathPrefix("/opt-out").Subrouter()
	optout.HandleFunc("", rt.postOptout).Methods(http.MethodPost)
	optout.HandleFunc("", rt.getOptout).Methods(http.MethodGet)

	optin := m.PathPrefix("/opt-in").Subrouter()
	optin.HandleFunc("", rt.postOptin).Methods(http.MethodPost)
	optin.HandleFunc("", rt.getOptin).Methods(http.MethodGet)

	exchange := m.PathPrefix("/exchange").Subrouter()
	exchange.HandleFunc("", rt.getPublicKey).Methods(http.MethodGet)
	exchange.HandleFunc("", rt.postUserSecret).Methods(http.MethodPost)

	accounts := m.PathPrefix("/accounts/{accountID}").Subrouter()
	accounts.Use(accountAuth)
	accounts.Handle("", http.HandlerFunc(rt.getAccount)).Methods(http.MethodGet)

	deleted := m.PathPrefix("/deleted").Subrouter()
	deletedEventsForUser := userCookie(http.HandlerFunc(rt.getDeletedEvents))
	deleted.Handle("", deletedEventsForUser).Methods(http.MethodPost).Queries("user", "1")
	deleted.HandleFunc("", rt.getDeletedEvents).Methods(http.MethodPost)

	login := m.PathPrefix("/login").Subrouter()
	login.Handle("", accountAuth(http.HandlerFunc(rt.getLogin))).Methods(http.MethodGet)
	login.HandleFunc("", rt.postLogin).Methods(http.MethodPost)

	changePassword := m.PathPrefix("/change-password").Subrouter()
	changePassword.Use(accountAuth)
	changePassword.HandleFunc("", rt.postChangePassword).Methods(http.MethodPost)

	changeEmail := m.PathPrefix("/change-email").Subrouter()
	changeEmail.Use(accountAuth)
	changeEmail.HandleFunc("", rt.postChangeEmail).Methods(http.MethodPost)

	purge := m.PathPrefix("/purge").Subrouter()
	purge.Use(userCookie)
	purge.HandleFunc("", rt.purgeEvents).Methods(http.MethodPost)

	events := m.PathPrefix("/events").Subrouter()
	events.Handle("", userCookie(http.HandlerFunc(rt.getEvents))).Methods(http.MethodGet)
	receiveEvents := dropOptout(http.HandlerFunc(rt.postEvents))
	events.Handle("", receiveEvents).Methods(http.MethodPost).Queries("anonymous", "1")
	events.Handle("", userCookie(receiveEvents)).Methods(http.MethodPost)

	health := m.PathPrefix("/healthz").Subrouter()
	health.HandleFunc("", rt.getHealth).Methods(http.MethodGet)

	m.NotFoundHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		respondWithJSONError(w, errors.New("Not found"), http.StatusNotFound)
	})

	return m
}
