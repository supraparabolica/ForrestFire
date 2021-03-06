package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"runtime"
	"strings"
	"syscall"

	"github.com/darkhelmet/ForrestFire/bookmarklet"
	"github.com/darkhelmet/ForrestFire/looper"
	"github.com/darkhelmet/env"
	"github.com/darkhelmet/postmark"
	"github.com/darkhelmet/tinderizer"
	"github.com/darkhelmet/tinderizer/cache"
	J "github.com/darkhelmet/tinderizer/job"
	"github.com/darkhelmet/webutil"
	"github.com/gorilla/mux"
)

const (
	HeaderAccessControlAllowOrigin = "Access-Control-Allow-Origin"
	QueueSize                      = 10

	HttpRedirect = "http.redirect"

	SubmitOld      = "submit.old"
	SubmitSuccess  = "submit.success"
	SubmitError    = "submit.error"
	SubmitEmail    = "submit.email"
	PostmarkBounce = "postmark.bounce"

	ContentType           = "Content-Type"
	Location              = "Location"
	ContentTypeHTML       = "text/html; charset=utf-8"
	ContentTypePlain      = "text/plain; charset=utf-8"
	ContentTypeJavascript = "application/javascript; charset=utf-8"
	ContentTypeJSON       = "application/json; charset=utf-8"
)

var (
	doneRegex     = regexp.MustCompile("(?i:done|failed|limited|invalid|error|sorry)")
	port          = env.IntDefault("PORT", 8080)
	canonicalHost = env.StringDefaultF("CANONICAL_HOST", func() string { return fmt.Sprintf("tinderizer.dev:%d", port) })
	logger        = log.New(os.Stdout, "[server] ", env.IntDefault("LOG_FLAGS", log.LstdFlags|log.Lmicroseconds))
	templates     = template.Must(template.ParseGlob("views/*.tmpl"))
	app           *tinderizer.App
)

type JSON map[string]interface{}

func init() {
	redis := env.StringDefault("REDISCLOUD_URL", env.StringDefault("REDIS_PORT", ""))
	if redis != "" {
		cache.SetupRedis(redis, env.StringDefault("REDIS_OPTIONS", "timeout=15s&maxidle=1"))
	}

	mercuryToken := env.String("MERCURY_TOKEN")
	pmToken := env.String("POSTMARK_TOKEN")
	from := env.String("FROM")
	binary, _ := exec.LookPath(fmt.Sprintf("kindlegen-%s", runtime.GOOS))

	tlogger := log.New(os.Stdout, "[tinderizer] ", env.IntDefault("LOG_FLAGS", log.LstdFlags|log.Lmicroseconds))

	app = tinderizer.New(mercuryToken, pmToken, from, binary, tlogger)
	app.Run(QueueSize)

	// TODO: handle SIGINT
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	go shutdown(c)
}

func shutdown(c chan os.Signal) {
	<-c
	logger.Println("shutting down...")
	app.Shutdown()
	os.Exit(0)
}

type Response struct {
	http.ResponseWriter
}

func (r Response) HTML() http.ResponseWriter {
	h := r.Header()
	h.Set(ContentType, ContentTypeHTML)
	r.WriteHeader(http.StatusOK)
	return r.ResponseWriter
}

func (r Response) Javascript() http.ResponseWriter {
	h := r.Header()
	h.Set(ContentType, ContentTypeJavascript)
	r.WriteHeader(http.StatusOK)
	return r.ResponseWriter
}

func (r Response) JSON() http.ResponseWriter {
	h := r.Header()
	h.Set(ContentType, ContentTypeJSON)
	r.WriteHeader(http.StatusOK)
	return r.ResponseWriter
}

func (r Response) Plain() http.ResponseWriter {
	h := r.Header()
	h.Set(ContentType, ContentTypePlain)
	r.WriteHeader(http.StatusOK)
	return r.ResponseWriter
}

func H(f func(Response, *http.Request)) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, req *http.Request) {
		f(Response{w}, req)
	}
}

func RenderPage(w io.Writer, page, host string) error {
	var buffer bytes.Buffer
	if err := templates.ExecuteTemplate(&buffer, page, nil); err != nil {
		return err
	}
	return templates.ExecuteTemplate(w, "layout.tmpl", JSON{
		"host":  host,
		"yield": template.HTML(buffer.String()),
	})
}

func HandleBookmarklet(res Response, req *http.Request) {
	w := res.Javascript()
	w.Write(bookmarklet.Javascript())
}

func PageHandler(res Response, req *http.Request) {
	w := res.HTML()
	vars := mux.Vars(req)
	tmpl := fmt.Sprintf("%s.tmpl", vars["page"])
	if err := RenderPage(w, tmpl, canonicalHost); err != nil {
		logger.Printf("failed rendering page: %s", err)
	}
}

func ChunkHandler(res Response, req *http.Request) {
	w := res.HTML()
	vars := mux.Vars(req)
	tmpl := fmt.Sprintf("%s.tmpl", vars["chunk"])
	if err := templates.ExecuteTemplate(w, tmpl, nil); err != nil {
		logger.Printf("failed rendering chunk: %s", err)
	}
}

func HomeHandler(res Response, req *http.Request) {
	w := res.HTML()
	if err := RenderPage(w, "index.tmpl", canonicalHost); err != nil {
		logger.Printf("failed rendering index: %s", err)
	}
}

type EmailHeader struct {
	Name, Value string
}

type EmailToFull struct {
	Email, Name string
}

type InboundEmail struct {
	From, To, CC, ReplyTo, Subject string
	ToFull                         []EmailToFull
	MessageId, Date, MailboxHash   string
	TextBody, HtmlBody             string
	Tag                            string
	Headers                        []EmailHeader
}

func ExtractParts(e *InboundEmail) (email string, url string, err error) {
	parts := strings.Split(e.ToFull[0].Email, "@")
	if len(parts) == 0 {
		return "", "", errors.New("failed splitting email on '@'")
	}
	emailBytes, err := hex.DecodeString(parts[0])
	if err != nil {
		return "", "", fmt.Errorf("failed decoding email from hex: %s", err)
	}
	email = string(emailBytes)
	buffer := bytes.NewBufferString(strings.TrimSpace(e.TextBody))
	url, err = buffer.ReadString('\n')
	if len(url) == 0 && err != nil {
		return "", "", fmt.Errorf("failed reading line from email body: %s", err)
	}
	err = nil
	url = strings.TrimSpace(url)
	return
}

func InboundHandler(res Response, req *http.Request) {
	decoder := json.NewDecoder(req.Body)
	var inbound InboundEmail
	err := decoder.Decode(&inbound)
	if err != nil {
		logger.Printf("failed decoding inbound email: %s", err)
	} else {
		email, url, err := ExtractParts(&inbound)
		if err != nil {
			logger.Printf("failed extracting needed parts from email: %s", err)
		} else {
			logger.Printf("email submission of %#v to %#v", url, email)
			if job, err := J.New(email, url); err == nil {
				app.Queue(*job)
			}
		}
	}
	w := res.Plain()
	io.WriteString(w, "ok")
}

func BounceHandler(res Response, req *http.Request) {
	decoder := json.NewDecoder(req.Body)
	var bounce postmark.Bounce
	err := decoder.Decode(&bounce)
	if err != nil {
		logger.Printf("failed decoding bounce: %s", err)
		return
	}

	if looper.AlreadyResent(bounce.MessageID, bounce.Email) {
		logger.Printf("skipping resend of message ID %s", bounce.MessageID)
	} else {
		err = app.Reactivate(bounce)
		if err != nil {
			logger.Printf("failed reactivating bounce: %s", err)
			return
		}
		uri := looper.MarkResent(bounce.MessageID, bounce.Email)
		if job, err := J.New(bounce.Email, uri); err != nil {
			logger.Printf("bounced email failed to validate as a job: %s", err)
		} else {
			app.Queue(*job)
			logger.Printf("resending %#v to %#v after bounce", uri, bounce.Email)
		}
	}
	w := res.Plain()
	io.WriteString(w, "ok")
}

type Submission struct {
	Url     string `json:"url"`
	Email   string `json:"email"`
	Content string `json:"content"`
}

func SubmitHandler(res Response, req *http.Request) {
	decoder := json.NewDecoder(req.Body)
	var submission Submission
	if err := decoder.Decode(&submission); err != nil {
		logger.Printf("failed decoding submission: %s", err)
	} else {
		logger.Printf("submission of %#v to %#v", submission.Url, submission.Email)
	}

	w := res.JSON()
	encoder := json.NewEncoder(w)
	Submit(encoder, submission.Email, submission.Url)
}

func OldSubmitHandler(res Response, req *http.Request) {
	w := res.JSON()
	encoder := json.NewEncoder(w)
	email := req.URL.Query().Get("email")
	url := req.URL.Query().Get("url")
	Submit(encoder, email, url)
}

func HandleSubmitError(encoder *json.Encoder, err error) {
	logger.Printf("submit error: %s", err)
	encoder.Encode(JSON{"message": err.Error()})
}

func Submit(encoder *json.Encoder, email, url string) {
	job, err := J.New(email, url)
	if err != nil {
		HandleSubmitError(encoder, err)
		return
	}

	job.Progress("Working...")
	app.Queue(*job)
	encoder.Encode(JSON{
		"message": "Submitted! Hang tight...",
		"id":      job.Key.String(),
	})
}

func StatusHandler(res Response, req *http.Request) {
	vars := mux.Vars(req)
	w := res.JSON()
	message := "No job with that ID found."
	done := true
	if v, err := app.Status(vars["id"]); err == nil {
		message = v
		done = doneRegex.MatchString(message)
	}
	encoder := json.NewEncoder(w)
	encoder.Encode(JSON{
		"message": message,
		"done":    done,
	})
}

type CanonicalHostHandler struct {
	http.Handler
}

func (c CanonicalHostHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	prefix := strings.HasPrefix(r.URL.Path, "/ajax")
	get := r.Method == "GET"
	chost := r.Host == canonicalHost
	if prefix || !get || chost {
		c.Handler.ServeHTTP(w, r)
	} else {
		r.URL.Host = canonicalHost
		r.URL.Scheme = "http"
		http.Redirect(w, r, r.URL.String(), http.StatusMovedPermanently)
	}
}

func main() {
	submitRoute := "/ajax/submit.json"
	statusRoute := "/ajax/status/{id:[^.]+}.json"

	r := mux.NewRouter()
	r.HandleFunc("/", H(HomeHandler)).Methods("GET")
	r.HandleFunc("/inbound", H(InboundHandler)).Methods("POST")
	r.HandleFunc("/bounce", H(BounceHandler)).Methods("POST")
	r.HandleFunc("/static/bookmarklet.js", H(HandleBookmarklet)).Methods("GET")
	r.HandleFunc("/{page:(faq|bugs|contact)}", H(PageHandler)).Methods("GET")
	r.HandleFunc("/{chunk:(firefox|safari|chrome|ie|ios|kindle-email)}", H(ChunkHandler)).Methods("GET")
	r.HandleFunc(submitRoute, H(SubmitHandler)).Methods("POST")
	r.HandleFunc(submitRoute, H(OldSubmitHandler)).Methods("GET")
	r.HandleFunc(statusRoute, H(StatusHandler)).Methods("GET")
	r.PathPrefix("/").Handler(http.FileServer(http.Dir("public")))

	var handler http.Handler = r
	handler = webutil.AlwaysHeaderHandler{handler, http.Header{HeaderAccessControlAllowOrigin: {"*"}}}
	handler = webutil.GzipHandler{handler}
	handler = CanonicalHostHandler{handler}
	handler = webutil.EnsureRequestBodyClosedHandler{handler}

	http.Handle("/", handler)

	logger.Printf("Tinderizer is starting on 0.0.0.0:%d", port)
	err := http.ListenAndServe(fmt.Sprintf("0.0.0.0:%d", port), nil)
	if err != nil {
		logger.Fatalf("failed to serve: %s", err)
	}
}
