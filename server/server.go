package main

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/felixge/httpsnoop"
	hutil "github.com/kjk/common/httputil"

	"github.com/google/go-github/github"
	"github.com/kjk/common/u"
	"golang.org/x/exp/slices"
	"golang.org/x/oauth2"
)

var (
	//go:embed dist/*
	wwwFS embed.FS
	//go:embed secrets.env
	secretsEnv []byte

	fsys fs.FS
)

var (
	proxyURLStr = "http://localhost:3025"
)

var (
	githubEndpoint = oauth2.Endpoint{
		AuthURL:  "https://github.com/login/oauth/authorize",
		TokenURL: "https://github.com/login/oauth/access_token",
	}

	// https://github.com/settings/applications/2098699
	oauthGitHubConf = &oauth2.Config{
		ClientID:     "",
		ClientSecret: "",
		// select level of access you want https://developer.github.com/v3/oauth/#scopes
		Scopes:   []string{"user:email", "read:user", "gist"},
		Endpoint: githubEndpoint,
	}

	// random string for oauth2 API calls to protect against CSRF
	oauthSecretPrefix = "5576867039-"
)

// we need different oauth callbacks for dev and production so we registered 2 apps:
// https://github.com/settings/applications/1159176 : onlinetool.io Local
func setGitHubAuth() {
	oauthGitHubConf.ClientID = "389af84bdce4b478ad7b"
	oauthGitHubConf.ClientSecret = secretGitHub
	// TODO: is there a better check for running locally?
	if flgRunDev || flgRunProdLocal {
		oauthGitHubConf.ClientID = "77ba1cbe7c0eff7c462b"
		oauthGitHubConf.ClientSecret = secretGitHubLocal
	}
}

var (
	pongTxt = []byte("pong")
)

func logLogin(ctx context.Context, r *http.Request, token *oauth2.Token) {
	oauthClient := oauthGitHubConf.Client(ctx, token)
	client := github.NewClient(oauthClient)
	user, _, err := client.Users.Get(ctx, "")
	if err != nil {
		logf("client.Users.Get() faled with '%s'\n", err)
		return
	}
	logf("logged in as GitHub user: %s\n", *user.Login)
	m := map[string]string{}
	if user.Login != nil {
		m["user"] = *user.Login
	}
	if user.Email != nil {
		m["email"] = *user.Email
	}
	if user.Name != nil {
		m["name"] = *user.Name
	}
	pirschSendEvent(r, "github_login", 0, m)
}

// /auth/githubcb
// as set in https://github.com/settings/applications/1159140
func handleGithubCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	logf("handleGithubCallback: '%s'\n", r.URL)
	state := r.FormValue("state")
	if !strings.HasPrefix(state, oauthSecretPrefix) {
		logErrorf("invalid oauth state, expected '%s*', got '%s'\n", oauthSecretPrefix, state)
		http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
		return
	}

	code := r.FormValue("code")
	token, err := oauthGitHubConf.Exchange(context.Background(), code)
	if err != nil {
		logErrorf("oauthGoogleConf.Exchange() failed with '%s'\n", err)
		http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
		return
	}
	logf("token: %#v", token)
	ac := token.AccessToken
	uri := "/github_success?access_token=" + ac
	logf("token: %#v\nuri: %s\n", token, uri)
	http.Redirect(w, r, uri, http.StatusTemporaryRedirect)

	// can't put in the background because that cancels ctx
	logLogin(ctx, r, token)
}

// /auth/ghlogin
func handleLoginGitHub(w http.ResponseWriter, r *http.Request) {
	// GitHub seems to completely ignore Redir, which makes testing locally hard
	// TODO: generate temporary oathSecret
	uri := oauthGitHubConf.AuthCodeURL(oauthSecretPrefix, oauth2.AccessTypeOnline)
	logf("handleLoginGitHub: to '%s'\n", uri)
	http.Redirect(w, r, uri, http.StatusTemporaryRedirect)
}

func permRedirect(w http.ResponseWriter, r *http.Request, newURL string) {
	http.Redirect(w, r, newURL, http.StatusPermanentRedirect)
}

// in dev, proxyHandler redirects assets to vite web server
// in prod, assets must be pre-built in frontend/dist directory
func makeHTTPServer(proxyHandler *httputil.ReverseProxy, fsys fs.FS) *http.Server {
	mainHandler := func(w http.ResponseWriter, r *http.Request) {

		serveOpts := hutil.ServeFileOptions{
			FS:               fsys,
			SupportCleanURLS: true,
			ForceCleanURLS:   true,
			ServeCompressed:  false,
		}
		uri := r.URL.Path

		switch uri {
		case "/ping", "/ping.txt":
			content := bytes.NewReader(pongTxt)
			http.ServeContent(w, r, "foo.txt", time.Time{}, content)
			return
		case "/auth/ghlogin":
			handleLoginGitHub(w, r)
			return
		case "/auth/githubcb":
			handleGithubCallback(w, r)
			return
		}

		if strings.HasPrefix(uri, "/event/") {
			handleEvent(w, r)
			return
		}

		if strings.HasPrefix(uri, "/api/goplay/") {
			handleGoPlayground(w, r)
			return
		}

		tryServeRedirect := func(uri string) bool {
			if uri == "/home" {
				http.Redirect(w, r, "/", http.StatusPermanentRedirect)
				return true
			}
			return false
		}
		if tryServeRedirect(uri) {
			return
		}

		if proxyHandler != nil {
			transformRequestForProxy := func() {
				uris := []string{"/github_success", "/gisteditor/nogist", "/gisteditor/edit"}
				shouldProxyURI := slices.Contains(uris, uri)
				if !shouldProxyURI {
					return
				}
				newPath := uri + ".html"
				newURI := strings.Replace(r.URL.String(), uri, newPath, 1)
				var err error
				r.URL, err = url.Parse(newURI)
				must(err)
			}

			transformRequestForProxy()
			proxyHandler.ServeHTTP(w, r)
			return
		}

		if hutil.TryServeFile(w, r, &serveOpts) {
			return
		}

		http.NotFound(w, r)
	}

	handlerWithMetrics := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := httpsnoop.CaptureMetrics(http.HandlerFunc(mainHandler), w, r)
		defer func() {
			if p := recover(); p != nil {
				logf("handlerWithMetrics: panicked with with %v\n", p)
				errStr := fmt.Sprintf("Error: %v", p)
				http.Error(w, errStr, http.StatusInternalServerError)
				return
			}
			logHTTPReq(r, m.Code, m.Written, m.Duration)
			if m.Code == 200 {
				pirschSendHit(r)
			}
			axiomLogHTTPReq(ctx(), r, m.Code, int(m.Written), m.Duration)
		}()
	})

	httpSrv := &http.Server{
		ReadTimeout:  120 * time.Second,
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  120 * time.Second,
		Handler:      http.HandlerFunc(handlerWithMetrics),
	}
	httpAddr := fmt.Sprintf(":%d", httpPort)
	if isWinOrMac() {
		httpAddr = "localhost" + httpAddr
	}
	httpSrv.Addr = httpAddr
	return httpSrv
}

func runServerProd() {
	var fsys fs.FS
	fromZip := len(frontendZipData) > 0
	if fromZip {
		var err error
		fsys, err = u.NewMemoryFSForZipData(frontendZipData)
		must(err)
		sizeStr := u.FormatSize(int64(len(frontendZipData)))
		logf("runServerProd(): will serve files from embedded zip of size '%v'\n", sizeStr)
	} else {
		panicIf(isLinux(), "if running on Linux, must use frontendZipDataa")

		rebuildFrontend()
		// assuming this is not deployment: re-build the frontend
		panicIf(!u.DirExists(frontEndBuildDir), "dir '%s' doesn't exist", frontEndBuildDir)
		fsys = os.DirFS(frontEndBuildDir)
	}

	httpSrv := makeHTTPServer(nil, fsys)
	logf("runServerProd(): starting on 'http://%s', dev: %v\n", httpSrv.Addr, isDev())
	if isWinOrMac() {
		time.Sleep(time.Second * 2)
		u.OpenBrowser("http://" + httpSrv.Addr)
	}
	waitFn := serverListenAndWait(httpSrv)
	waitFn()
}

func runServerDev() {
	if hasBun() {
		u.RunLoggedInDir("frontend", "bun", "install")
		closeDev, err := startLoggedInDir("frontend", "bun", "run", "dev")
		must(err)
		defer closeDev()
	} else {
		u.RunLoggedInDir("frontend", "yarn")
		closeDev, err := startLoggedInDir("frontend", "yarn", "dev")
		must(err)
		defer closeDev()
	}

	// must be same as vite.config.js
	proxyURL, err := url.Parse(proxyURLStr)
	must(err)
	proxyHandler := httputil.NewSingleHostReverseProxy(proxyURL)

	fsys := os.DirFS(filepath.Join("frontend", "public"))
	httpSrv := makeHTTPServer(proxyHandler, fsys)

	//closeHTTPLog := OpenHTTPLog("codeeval")
	//defer closeHTTPLog()

	logf("runServerDev(): starting on '%s', dev: %v\n", httpSrv.Addr, isDev())
	if isWinOrMac() {
		time.Sleep(time.Second * 2)
		u.OpenBrowser("http://" + httpSrv.Addr)
	}
	waitFn := serverListenAndWait(httpSrv)
	waitFn()
}

func serverListenAndWait(httpSrv *http.Server) func() {
	chServerClosed := make(chan bool, 1)
	go func() {
		err := httpSrv.ListenAndServe()
		// mute error caused by Shutdown()
		if err == http.ErrServerClosed {
			err = nil
		}
		if err == nil {
			logf("HTTP server shutdown gracefully\n")
		} else {
			logf("httpSrv.ListenAndServe error '%s'\n", err)
		}
		chServerClosed <- true
	}()

	return func() {
		// Ctrl-C sends SIGINT
		sctx, stop := signal.NotifyContext(ctx(), os.Interrupt /*SIGINT*/, os.Kill /* SIGKILL */, syscall.SIGTERM)
		defer stop()
		<-sctx.Done()

		logf("Got one of the signals. Shutting down http server\n")
		_ = httpSrv.Shutdown(ctx())
		select {
		case <-chServerClosed:
			// do nothing
		case <-time.After(time.Second * 5):
			// timeout
			logf("timed out trying to shut down http server")
		}
	}
}
