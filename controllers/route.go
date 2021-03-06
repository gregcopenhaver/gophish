package controllers

import (
	"compress/gzip"
	"context"
	"html/template"
	"net/http"
	"net/url"
	"time"

	"github.com/NYTimes/gziphandler"
	"github.com/gophish/gophish/auth"
	"github.com/gophish/gophish/config"
	ctx "github.com/gophish/gophish/context"
	log "github.com/gophish/gophish/logger"
	mid "github.com/gophish/gophish/middleware"
	"github.com/gophish/gophish/models"
	"github.com/gophish/gophish/util"
	"github.com/gophish/gophish/worker"
	"github.com/gorilla/csrf"
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/gorilla/sessions"
	"github.com/jordan-wright/unindexed"
)

// AdminServerOption is a functional option that is used to configure the
// admin server
type AdminServerOption func(*AdminServer)

// AdminServer is an HTTP server that implements the administrative Gophish
// handlers, including the dashboard and REST API.
type AdminServer struct {
	server *http.Server
	worker worker.Worker
	config config.AdminServer
}

// WithWorker is an option that sets the background worker.
func WithWorker(w worker.Worker) AdminServerOption {
	return func(as *AdminServer) {
		as.worker = w
	}
}

// NewAdminServer returns a new instance of the AdminServer with the
// provided config and options applied.
func NewAdminServer(config config.AdminServer, options ...AdminServerOption) *AdminServer {
	defaultWorker, _ := worker.New()
	defaultServer := &http.Server{
		ReadTimeout: 10 * time.Second,
		Addr:        config.ListenURL,
	}
	as := &AdminServer{
		worker: defaultWorker,
		server: defaultServer,
		config: config,
	}
	for _, opt := range options {
		opt(as)
	}
	as.registerRoutes()
	return as
}

// Start launches the admin server, listening on the configured address.
func (as *AdminServer) Start() error {
	if as.worker != nil {
		go as.worker.Start()
	}
	if as.config.UseTLS {
		err := util.CheckAndCreateSSL(as.config.CertPath, as.config.KeyPath)
		if err != nil {
			log.Fatal(err)
			return err
		}
		log.Infof("Starting admin server at https://%s", as.config.ListenURL)
		return as.server.ListenAndServeTLS(as.config.CertPath, as.config.KeyPath)
	}
	// If TLS isn't configured, just listen on HTTP
	log.Infof("Starting admin server at http://%s", as.config.ListenURL)
	return as.server.ListenAndServe()
}

// Shutdown attempts to gracefully shutdown the server.
func (as *AdminServer) Shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()
	return as.server.Shutdown(ctx)
}

// SetupAdminRoutes creates the routes for handling requests to the web interface.
// This function returns an http.Handler to be used in http.ListenAndServe().
func (as *AdminServer) registerRoutes() {
	router := mux.NewRouter()
	// Base Front-end routes
	router.HandleFunc("/", Use(as.Base, mid.RequireLogin))
	router.HandleFunc("/login", as.Login)
	router.HandleFunc("/logout", Use(as.Logout, mid.RequireLogin))
	router.HandleFunc("/campaigns", Use(as.Campaigns, mid.RequireLogin))
	router.HandleFunc("/campaigns/{id:[0-9]+}", Use(as.CampaignID, mid.RequireLogin))
	router.HandleFunc("/templates", Use(as.Templates, mid.RequireLogin))
	router.HandleFunc("/users", Use(as.Users, mid.RequireLogin))
	router.HandleFunc("/landing_pages", Use(as.LandingPages, mid.RequireLogin))
	router.HandleFunc("/sending_profiles", Use(as.SendingProfiles, mid.RequireLogin))
	router.HandleFunc("/register", Use(as.Register, mid.RequireLogin))
	router.HandleFunc("/settings", Use(as.Settings, mid.RequireLogin))
	// Create the API routes
	api := router.PathPrefix("/api").Subrouter()
	api = api.StrictSlash(true)
	api.Use(mid.RequireAPIKey)
	api.HandleFunc("/reset", as.APIReset)
	api.HandleFunc("/campaigns/", as.APICampaigns)
	api.HandleFunc("/campaigns/summary", as.APICampaignsSummary)
	api.HandleFunc("/campaigns/{id:[0-9]+}", as.APICampaign)
	api.HandleFunc("/campaigns/{id:[0-9]+}/results", as.APICampaignResults)
	api.HandleFunc("/campaigns/{id:[0-9]+}/summary", as.APICampaignSummary)
	api.HandleFunc("/campaigns/{id:[0-9]+}/complete", as.APICampaignComplete)
	api.HandleFunc("/groups/", as.APIGroups)
	api.HandleFunc("/groups/summary", as.APIGroupsSummary)
	api.HandleFunc("/groups/{id:[0-9]+}", as.APIGroup)
	api.HandleFunc("/groups/{id:[0-9]+}/summary", as.APIGroupSummary)
	api.HandleFunc("/templates/", as.APITemplates)
	api.HandleFunc("/templates/{id:[0-9]+}", as.APITemplate)
	api.HandleFunc("/pages/", as.APIPages)
	api.HandleFunc("/pages/{id:[0-9]+}", as.APIPage)
	api.HandleFunc("/smtp/", as.APISendingProfiles)
	api.HandleFunc("/smtp/{id:[0-9]+}", as.APISendingProfile)
	api.HandleFunc("/util/send_test_email", as.APISendTestEmail)
	api.HandleFunc("/import/group", as.APIImportGroup)
	api.HandleFunc("/import/email", as.APIImportEmail)
	api.HandleFunc("/import/site", as.APIImportSite)

	// Setup static file serving
	router.PathPrefix("/").Handler(http.FileServer(unindexed.Dir("./static/")))

	// Setup CSRF Protection
	csrfHandler := csrf.Protect([]byte(auth.GenerateSecureKey()),
		csrf.FieldName("csrf_token"),
		csrf.Secure(as.config.UseTLS))
	adminHandler := csrfHandler(router)
	adminHandler = Use(adminHandler.ServeHTTP, mid.CSRFExceptions, mid.GetContext)

	// Setup GZIP compression
	gzipWrapper, _ := gziphandler.NewGzipLevelHandler(gzip.BestCompression)
	adminHandler = gzipWrapper(adminHandler)

	// Setup logging
	adminHandler = handlers.CombinedLoggingHandler(log.Writer(), adminHandler)
	as.server.Handler = adminHandler
}

// Use allows us to stack middleware to process the request
// Example taken from https://github.com/gorilla/mux/pull/36#issuecomment-25849172
func Use(handler http.HandlerFunc, mid ...func(http.Handler) http.HandlerFunc) http.HandlerFunc {
	for _, m := range mid {
		handler = m(handler)
	}
	return handler
}

type templateParams struct {
	Title   string
	Flashes []interface{}
	User    models.User
	Token   string
	Version string
}

// newTemplateParams returns the default template parameters for a user and
// the CSRF token.
func newTemplateParams(r *http.Request) templateParams {
	return templateParams{
		Token:   csrf.Token(r),
		User:    ctx.Get(r, "user").(models.User),
		Version: config.Version,
	}
}

// Register creates a new user
func (as *AdminServer) Register(w http.ResponseWriter, r *http.Request) {
	// If it is a post request, attempt to register the account
	// Now that we are all registered, we can log the user in
	params := templateParams{Title: "Register", Token: csrf.Token(r)}
	session := ctx.Get(r, "session").(*sessions.Session)
	switch {
	case r.Method == "GET":
		params.Flashes = session.Flashes()
		session.Save(r, w)
		templates := template.New("template")
		_, err := templates.ParseFiles("templates/register.html", "templates/flashes.html")
		if err != nil {
			log.Error(err)
		}
		template.Must(templates, err).ExecuteTemplate(w, "base", params)
	case r.Method == "POST":
		//Attempt to register
		succ, err := auth.Register(r)
		//If we've registered, redirect to the login page
		if succ {
			Flash(w, r, "success", "Registration successful!")
			session.Save(r, w)
			http.Redirect(w, r, "/login", 302)
			return
		}
		// Check the error
		m := err.Error()
		log.Error(err)
		Flash(w, r, "danger", m)
		session.Save(r, w)
		http.Redirect(w, r, "/register", 302)
		return
	}
}

// Base handles the default path and template execution
func (as *AdminServer) Base(w http.ResponseWriter, r *http.Request) {
	params := newTemplateParams(r)
	params.Title = "Dashboard"
	getTemplate(w, "dashboard").ExecuteTemplate(w, "base", params)
}

// Campaigns handles the default path and template execution
func (as *AdminServer) Campaigns(w http.ResponseWriter, r *http.Request) {
	params := newTemplateParams(r)
	params.Title = "Campaigns"
	getTemplate(w, "campaigns").ExecuteTemplate(w, "base", params)
}

// CampaignID handles the default path and template execution
func (as *AdminServer) CampaignID(w http.ResponseWriter, r *http.Request) {
	params := newTemplateParams(r)
	params.Title = "Campaign Results"
	getTemplate(w, "campaign_results").ExecuteTemplate(w, "base", params)
}

// Templates handles the default path and template execution
func (as *AdminServer) Templates(w http.ResponseWriter, r *http.Request) {
	params := newTemplateParams(r)
	params.Title = "Email Templates"
	getTemplate(w, "templates").ExecuteTemplate(w, "base", params)
}

// Users handles the default path and template execution
func (as *AdminServer) Users(w http.ResponseWriter, r *http.Request) {
	params := newTemplateParams(r)
	params.Title = "Users & Groups"
	getTemplate(w, "users").ExecuteTemplate(w, "base", params)
}

// LandingPages handles the default path and template execution
func (as *AdminServer) LandingPages(w http.ResponseWriter, r *http.Request) {
	params := newTemplateParams(r)
	params.Title = "Landing Pages"
	getTemplate(w, "landing_pages").ExecuteTemplate(w, "base", params)
}

// SendingProfiles handles the default path and template execution
func (as *AdminServer) SendingProfiles(w http.ResponseWriter, r *http.Request) {
	params := newTemplateParams(r)
	params.Title = "Sending Profiles"
	getTemplate(w, "sending_profiles").ExecuteTemplate(w, "base", params)
}

// Settings handles the changing of settings
func (as *AdminServer) Settings(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == "GET":
		params := newTemplateParams(r)
		params.Title = "Settings"
		getTemplate(w, "settings").ExecuteTemplate(w, "base", params)
	case r.Method == "POST":
		err := auth.ChangePassword(r)
		msg := models.Response{Success: true, Message: "Settings Updated Successfully"}
		if err == auth.ErrInvalidPassword {
			msg.Message = "Invalid Password"
			msg.Success = false
			JSONResponse(w, msg, http.StatusBadRequest)
			return
		}
		if err != nil {
			msg.Message = err.Error()
			msg.Success = false
			JSONResponse(w, msg, http.StatusBadRequest)
			return
		}
		JSONResponse(w, msg, http.StatusOK)
	}
}

// Login handles the authentication flow for a user. If credentials are valid,
// a session is created
func (as *AdminServer) Login(w http.ResponseWriter, r *http.Request) {
	params := struct {
		User    models.User
		Title   string
		Flashes []interface{}
		Token   string
	}{Title: "Login", Token: csrf.Token(r)}
	session := ctx.Get(r, "session").(*sessions.Session)
	switch {
	case r.Method == "GET":
		params.Flashes = session.Flashes()
		session.Save(r, w)
		templates := template.New("template")
		_, err := templates.ParseFiles("templates/login.html", "templates/flashes.html")
		if err != nil {
			log.Error(err)
		}
		template.Must(templates, err).ExecuteTemplate(w, "base", params)
	case r.Method == "POST":
		//Attempt to login
		succ, u, err := auth.Login(r)
		if err != nil {
			log.Error(err)
		}
		//If we've logged in, save the session and redirect to the dashboard
		if succ {
			session.Values["id"] = u.Id
			session.Save(r, w)
			next := "/"
			url, err := url.Parse(r.FormValue("next"))
			if err == nil {
				path := url.Path
				if path != "" {
					next = path
				}
			}
			http.Redirect(w, r, next, 302)
		} else {
			Flash(w, r, "danger", "Invalid Username/Password")
			params.Flashes = session.Flashes()
			session.Save(r, w)
			templates := template.New("template")
			_, err := templates.ParseFiles("templates/login.html", "templates/flashes.html")
			if err != nil {
				log.Error(err)
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			template.Must(templates, err).ExecuteTemplate(w, "base", params)
		}
	}
}

// Logout destroys the current user session
func (as *AdminServer) Logout(w http.ResponseWriter, r *http.Request) {
	session := ctx.Get(r, "session").(*sessions.Session)
	delete(session.Values, "id")
	Flash(w, r, "success", "You have successfully logged out")
	session.Save(r, w)
	http.Redirect(w, r, "/login", 302)
}

func getTemplate(w http.ResponseWriter, tmpl string) *template.Template {
	templates := template.New("template")
	_, err := templates.ParseFiles("templates/base.html", "templates/"+tmpl+".html", "templates/flashes.html")
	if err != nil {
		log.Error(err)
	}
	return template.Must(templates, err)
}

// Flash handles the rendering flash messages
func Flash(w http.ResponseWriter, r *http.Request, t string, m string) {
	session := ctx.Get(r, "session").(*sessions.Session)
	session.AddFlash(models.Flash{
		Type:    t,
		Message: m,
	})
}
