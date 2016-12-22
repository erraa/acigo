package aci

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// ClientOptions is used to specify options for the Client.
type ClientOptions struct {
	Hosts []string // List of apic hostnames. If unspecified, env var APIC_HOSTS is used.
	User  string   // Username. If unspecified, env var APIC_USER is used.
	Pass  string   // Password. If unspecified, env var APIC_PASS is used.
	Debug bool     // Debug enables verbose debugging messages to console.
}

// Client is an instance for interacting with ACI using API calls.
type Client struct {
	Opt                 ClientOptions // Options for the APIC client
	host                int
	cli                 *http.Client
	loginToken          string
	loginRefreshTimeout time.Duration
}

const (
	ApicHosts = "APIC_HOSTS" // Env var. List of apic hostnames. Example: "1.1.1.1" or "1.1.1.1,2.2.2.2,3.3.3.3" or "apic1,4.4.4.4"
	ApicUser  = "APIC_USER"  // Env var. Username. Example: "joe"
	ApicPass  = "APIC_PASS"  // Env var. Password. Example: "joesecret"
)

// New creates a new Client instance for interacting with ACI using API calls.
func New(o ClientOptions) (*Client, error) {
	if len(o.Hosts) < 1 {
		hosts := os.Getenv(ApicHosts)
		if hosts == "" {
			return nil, fmt.Errorf("missing apic hosts: %s=%s", ApicHosts, o.Hosts)
		}
		o.Hosts = strings.Split(hosts, ",")
		if len(o.Hosts) < 1 {
			return nil, fmt.Errorf("missing apic hosts: %s=%s", ApicHosts, o.Hosts)
		}
		for _, h := range o.Hosts {
			if strings.TrimSpace(h) == "" {
				return nil, fmt.Errorf("blank apic hostname '%s' in %s=%s", h, ApicHosts, o.Hosts)
			}
		}
	}

	if o.User == "" {
		o.User = os.Getenv(ApicUser)
		if o.User == "" {
			return nil, fmt.Errorf("missing apic user: %s=%s", ApicUser, o.User)
		}
	}

	if o.Pass == "" {
		o.Pass = os.Getenv(ApicPass)
		if o.Pass == "" {
			return nil, fmt.Errorf("missing apic pass: %s=%s", ApicPass, o.Pass)
		}
	}

	c := &Client{Opt: o}

	c.newHttpClient()

	c.debugf("new client: hosts=%s user=%s pass=%s", c.Opt.Hosts, c.Opt.User, c.Opt.Pass)

	return c, nil
}

func (c *Client) debugf(fmt string, v ...interface{}) {
	if c.Opt.Debug {
		c.logf("debug "+fmt, v...)
	}
}

func (c *Client) logf(fmt string, v ...interface{}) {
	log.Printf("aci client: "+fmt, v...)
}

func (c *Client) jsonAaaUser() string {
	return fmt.Sprintf(`{"aaaUser": {"attributes": {"name": "%s", "pwd": "%s"}}}`, c.Opt.User, c.Opt.Pass)
}

// Logout close a session to APIC using the API aaaLogout.
func (c *Client) Logout() error {

	logoutApi := "/api/aaaLogout.json"

	aaaUser := c.jsonAaaUser()

	c.debugf("logout: api=%s json=%s", logoutApi, aaaUser)

	body, errPost := c.postLogin(logoutApi, "application/json", bytes.NewBufferString(aaaUser))
	if errPost != nil {
		return errPost
	}

	c.debugf("logout: reply: %s", string(body))

	return nil
}

// Login opens a new session into APIC using the API aaaLogin.
func (c *Client) Login() error {

	loginApi := "/api/aaaLogin.json"

	aaaUser := c.jsonAaaUser()

	c.debugf("login: api=%s json=%s", loginApi, aaaUser)

	body, errPost := c.postLogin(loginApi, "application/json", bytes.NewBufferString(aaaUser))
	if errPost != nil {
		return errPost
	}

	var reply interface{}
	errJson := json.Unmarshal(body, &reply)
	if errJson != nil {
		return errJson
	}

	imdata, imdataError := mapGet(reply, "imdata")
	if imdataError != nil {
		return fmt.Errorf("login: json imdata error: %s", string(body))
	}

	first, firstError := sliceGet(imdata, 0)
	if firstError != nil {
		return fmt.Errorf("login: imdata first error: %s", string(body))
	}

	mm, mmMap := first.(map[string]interface{})
	if !mmMap {
		return fmt.Errorf("login: imdata slice first member not map: %s", string(body))
	}

	for k, v := range mm {
		switch k {
		case "error":
			attr := mapSimple(v, "attributes")
			code := mapString(attr, "code")
			text := mapString(attr, "text")
			return fmt.Errorf("login: error: code=%s text=%s", code, text)
		case "aaaLogin":
			attr := mapSimple(v, "attributes")
			token := mapString(attr, "token")
			refresh := mapString(attr, "refreshTimeoutSeconds")

			c.refreshUpdate(refresh) // save refresh
			c.loginToken = token     // save token
			c.debugf("login: ok: timeout=%v token=%s", c.RefreshTimeout(), token)

			return nil // ok
		}
	}

	return fmt.Errorf("login: could not find aaaLogin response: %s", string(body))
}

func (c *Client) refreshUpdate(refresh string) {
	timeout, timeoutErr := strconv.Atoi(refresh)
	if timeoutErr != nil {
		c.logf("refreshUpdate: bad refresh timeout '%s': %v", refresh, timeoutErr)
		timeout = 60 // defaults to 60 seconds
	}
	c.loginRefreshTimeout = time.Duration(timeout) * time.Second // save
}

// Refresh resets the session timer on APIC using the API aaaRefresh.
func (c *Client) Refresh() error {

	refreshApi := "/api/aaaRefresh.json"

	url := c.getURL(refreshApi)

	body, errGet := c.get(url)
	if errGet != nil {
		return errGet
	}

	var reply interface{}
	errJson := json.Unmarshal(body, &reply)
	if errJson != nil {
		return errJson
	}

	imdata, imdataError := mapGet(reply, "imdata")
	if imdataError != nil {
		return fmt.Errorf("refresh: json imdata error: %s", string(body))
	}

	first, firstError := sliceGet(imdata, 0)
	if firstError != nil {
		return fmt.Errorf("refresh: imdata first error: %s", string(body))
	}

	mm, mmMap := first.(map[string]interface{})
	if !mmMap {
		return fmt.Errorf("refresh: imdata slice first member not map: %s", string(body))
	}

	for k, v := range mm {
		switch k {
		case "error":
			attr := mapSimple(v, "attributes")
			code := mapString(attr, "code")
			text := mapString(attr, "text")
			return fmt.Errorf("refresh: error: code=%s text=%s", code, text)
		case "aaaLogin":
			attr := mapSimple(v, "attributes")
			token := mapString(attr, "token")
			refresh := mapString(attr, "refreshTimeoutSeconds")

			c.refreshUpdate(refresh) // save refresh
			c.loginToken = token     // save token
			c.debugf("refresh: ok: timeout=%v token=%s", c.RefreshTimeout(), token)

			return nil // ok
		}
	}

	return fmt.Errorf("refresh: could not find aaaLogin response: %s", string(body))
}

// RefreshTimeout gets the session timeout reported by last API call to APIC.
func (c *Client) RefreshTimeout() time.Duration {
	return c.loginRefreshTimeout
}

func (c *Client) newHttpClient() {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			CipherSuites:             []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA, tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA},
			PreferServerCipherSuites: true,
			InsecureSkipVerify:       true,
			MaxVersion:               tls.VersionTLS11,
			MinVersion:               tls.VersionTLS11,
		},
		DisableCompression: true,
		DisableKeepAlives:  true,
		Dial: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 10 * time.Second,
		}).Dial,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	c.cli = &http.Client{
		Transport: tr,
		Timeout:   15 * time.Second,
	}
}

func (c *Client) getURL(api string) string {
	url := "https://" + c.Opt.Hosts[c.host] + api
	return url
}

func (c *Client) postLogin(api string, contentType string, r io.Reader) ([]byte, error) {
	var last error

	for ; c.host < len(c.Opt.Hosts); c.host++ {

		url := c.getURL(api)

		body, errPost := c.post(url, contentType, r)
		if errPost != nil {
			c.debugf("post error: apic: %s: %v", url, errPost)
			last = errPost
			continue
		}

		return body, nil
	}

	return nil, fmt.Errorf("no more apic hosts to try - last: %v", last)
}

func (c *Client) showCookies(urlStr string) {
	if c.cli.Jar == nil {
		c.debugf("no cookies to send")
		return
	}

	u, errUrl := url.Parse(urlStr)
	if errUrl != nil {
		c.debugf("showCookies: %s: %v", urlStr, errUrl)
		return
	}

	cookies := c.cli.Jar.Cookies(u)
	for _, ck := range cookies {
		c.debugf("cookie to send: %s", ck.Name)
	}
}

func (c *Client) learnCookies(resp *http.Response) error {
	cookies := resp.Cookies()
	for _, ck := range cookies {
		c.debugf("learnCookies: seen: url=%s cookie=%s", resp.Request.URL, ck.Name)
		if ck.Name == "APIC-cookie" {
			if c.cli.Jar == nil {
				var errNew error
				c.cli.Jar, errNew = cookiejar.New(nil) // new jar
				if errNew != nil {
					return errNew
				}
			}
			c.cli.Jar.SetCookies(resp.Request.URL, []*http.Cookie{ck}) // add single cookie to jar
			c.debugf("learnCookies: learnt: url=%s cookie=%s value=%s", resp.Request.URL, ck.Name, ck.Value)
			break
		}
	}
	return nil
}

func (c *Client) post(url string, contentType string, r io.Reader) ([]byte, error) {
	c.debugf("post: apic endpoint: %s", url)

	c.showCookies(url)

	resp, errPost := c.cli.Post(url, contentType, r)
	if errPost != nil {
		return nil, errPost
	}

	if errLearn := c.learnCookies(resp); errLearn != nil {
		return nil, errLearn
	}

	body, errBody := ioutil.ReadAll(resp.Body)
	resp.Body.Close()

	return body, errBody
}

func (c *Client) get(url string) ([]byte, error) {
	c.debugf("get: apic endpoint: %s", url)

	c.showCookies(url)

	resp, errPost := c.cli.Get(url)
	if errPost != nil {
		return nil, errPost
	}

	if errLearn := c.learnCookies(resp); errLearn != nil {
		return nil, errLearn
	}

	body, errBody := ioutil.ReadAll(resp.Body)
	resp.Body.Close()

	return body, errBody
}