package caddytest

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aryann/difflib"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	caddycmd "github.com/caddyserver/caddy/v2/cmd"

	// plug in Caddy modules here
	_ "github.com/caddyserver/caddy/v2/modules/standard"
)

// Defaults store any configuration required to make the tests run
type Defaults struct {
	// Port we expect caddy to listening on
	AdminPort int
	// Certificates we expect to be loaded before attempting to run the tests
	Certifcates []string
}

// Default testing values
var Default = Defaults{
	AdminPort:   2019,
	Certifcates: []string{"/caddy.localhost.crt", "/caddy.localhost.key"},
}

var (
	matchKey  = regexp.MustCompile(`(/[\w\d\.]+\.key)`)
	matchCert = regexp.MustCompile(`(/[\w\d\.]+\.crt)`)
)

// Tester represents an instance of a test client.
type Tester struct {
	Client *http.Client
	t      *testing.T
}

// NewTester will create a new testing client with an attached cookie jar
func NewTester(t *testing.T) *Tester {

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("failed to create cookiejar: %s", err)
	}

	return &Tester{
		Client: &http.Client{
			Transport: CreateTestingTransport(),
			Jar:       jar,
			Timeout:   5 * time.Second,
		},
		t: t,
	}
}

type configLoadError struct {
	Response string
}

func (e configLoadError) Error() string { return e.Response }

func timeElapsed(start time.Time, name string) {
	elapsed := time.Since(start)
	log.Printf("%s took %s", name, elapsed)
}

// InitServer this will configure the server with a configurion of a specific
// type. The configType must be either "json" or the adapter type.
func (tc *Tester) InitServer(rawConfig string, configType string) {

	if err := tc.initServer(rawConfig, configType); err != nil {
		tc.t.Logf("failed to load config: %s", err)
		tc.t.Fail()
	}
}

// InitServer this will configure the server with a configurion of a specific
// type. The configType must be either "json" or the adapter type.
func (tc *Tester) initServer(rawConfig string, configType string) error {

	if testing.Short() {
		tc.t.SkipNow()
		return nil
	}

	err := validateTestPrerequisites()
	if err != nil {
		tc.t.Skipf("skipping tests as failed integration prerequisites. %s", err)
		return nil
	}

	tc.t.Cleanup(func() {
		if tc.t.Failed() {
			res, err := http.Get(fmt.Sprintf("http://localhost:%d/config/", Default.AdminPort))
			if err != nil {
				tc.t.Log("unable to read the current config")
				return
			}
			defer res.Body.Close()
			body, err := ioutil.ReadAll(res.Body)

			var out bytes.Buffer
			json.Indent(&out, body, "", "  ")
			tc.t.Logf("----------- failed with config -----------\n%s", out.String())
		}
	})

	rawConfig = prependCaddyFilePath(rawConfig)
	client := &http.Client{
		Timeout: time.Second * 2,
	}
	start := time.Now()
	req, err := http.NewRequest("POST", fmt.Sprintf("http://localhost:%d/load", Default.AdminPort), strings.NewReader(rawConfig))
	if err != nil {
		tc.t.Errorf("failed to create request. %s", err)
		return err
	}

	if configType == "json" {
		req.Header.Add("Content-Type", "application/json")
	} else {
		req.Header.Add("Content-Type", "text/"+configType)
	}

	res, err := client.Do(req)
	if err != nil {
		tc.t.Errorf("unable to contact caddy server. %s", err)
		return err
	}
	timeElapsed(start, "caddytest: config load time")

	defer res.Body.Close()
	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		tc.t.Errorf("unable to read response. %s", err)
		return err
	}

	if res.StatusCode != 200 {
		return configLoadError{Response: string(body)}
	}

	return nil
}

var hasValidated bool
var arePrerequisitesValid bool

func validateTestPrerequisites() error {

	if hasValidated {
		if !arePrerequisitesValid {
			return errors.New("caddy integration prerequisites failed. see first error")
		}
		return nil
	}

	hasValidated = true
	arePrerequisitesValid = false

	// check certificates are found
	for _, certName := range Default.Certifcates {
		if _, err := os.Stat(getIntegrationDir() + certName); os.IsNotExist(err) {
			return fmt.Errorf("caddy integration test certificates (%s) not found", certName)
		}
	}

	if isCaddyAdminRunning() != nil {
		// start inprocess caddy server
		os.Args = []string{"caddy", "run"}
		go func() {
			caddycmd.Main()
		}()

		// wait for caddy to start
		retries := 4
		for ; retries > 0 && isCaddyAdminRunning() != nil; retries-- {
			time.Sleep(10 * time.Millisecond)
		}
	}

	// assert that caddy is running
	if err := isCaddyAdminRunning(); err != nil {
		return err
	}

	arePrerequisitesValid = true
	return nil
}

func isCaddyAdminRunning() error {
	// assert that caddy is running
	client := &http.Client{
		Timeout: time.Second * 2,
	}
	_, err := client.Get(fmt.Sprintf("http://localhost:%d/config/", Default.AdminPort))
	if err != nil {
		return errors.New("caddy integration test caddy server not running. Expected to be listening on localhost:2019")
	}

	return nil
}

func getIntegrationDir() string {

	_, filename, _, ok := runtime.Caller(1)
	if !ok {
		panic("unable to determine the current file path")
	}

	return path.Dir(filename)
}

// use the convention to replace /[certificatename].[crt|key] with the full path
// this helps reduce the noise in test configurations and also allow this
// to run in any path
func prependCaddyFilePath(rawConfig string) string {
	r := matchKey.ReplaceAllString(rawConfig, getIntegrationDir()+"$1")
	r = matchCert.ReplaceAllString(r, getIntegrationDir()+"$1")
	return r
}

// CreateTestingTransport creates a testing transport that forces call dialing connections to happen locally
func CreateTestingTransport() *http.Transport {

	dialer := net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 5 * time.Second,
		DualStack: true,
	}

	dialContext := func(ctx context.Context, network, addr string) (net.Conn, error) {
		parts := strings.Split(addr, ":")
		destAddr := fmt.Sprintf("127.0.0.1:%s", parts[1])
		log.Printf("caddytest: redirecting the dialer from %s to %s", addr, destAddr)
		return dialer.DialContext(ctx, network, destAddr)
	}

	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
	}
}

// AssertLoadError will load a config and expect an error
func AssertLoadError(t *testing.T, rawConfig string, configType string, expectedError string) {

	tc := NewTester(t)

	err := tc.initServer(rawConfig, configType)
	if !strings.Contains(err.Error(), expectedError) {
		t.Errorf("expected error \"%s\" but got \"%s\"", expectedError, err.Error())
	}
}

// AssertRedirect makes a request and asserts the redirection happens
func (tc *Tester) AssertRedirect(requestURI string, expectedToLocation string, expectedStatusCode int) *http.Response {

	redirectPolicyFunc := func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}

	// using the existing client, we override the check redirect policy for this test
	old := tc.Client.CheckRedirect
	tc.Client.CheckRedirect = redirectPolicyFunc
	defer func() { tc.Client.CheckRedirect = old }()

	resp, err := tc.Client.Get(requestURI)
	if err != nil {
		tc.t.Errorf("failed to call server %s", err)
		return nil
	}

	if expectedStatusCode != resp.StatusCode {
		tc.t.Errorf("requesting \"%s\" expected status code: %d but got %d", requestURI, expectedStatusCode, resp.StatusCode)
	}

	loc, err := resp.Location()
	if err != nil {
		tc.t.Errorf("requesting \"%s\" expected location: \"%s\" but got error: %s", requestURI, expectedToLocation, err)
	}

	if expectedToLocation != loc.String() {
		tc.t.Errorf("requesting \"%s\" expected location: \"%s\" but got \"%s\"", requestURI, expectedToLocation, loc.String())
	}

	return resp
}

// AssertAdapt adapts a config and then tests it against an expected result
func AssertAdapt(t *testing.T, rawConfig string, adapterName string, expectedResponse string) {

	cfgAdapter := caddyconfig.GetAdapter(adapterName)
	if cfgAdapter == nil {
		t.Errorf("unrecognized config adapter '%s'", adapterName)
		return
	}

	options := make(map[string]interface{})
	options["pretty"] = "true"

	result, warnings, err := cfgAdapter.Adapt([]byte(rawConfig), options)
	if err != nil {
		t.Errorf("adapting config using %s adapter: %v", adapterName, err)
		return
	}

	if len(warnings) > 0 {
		for _, w := range warnings {
			t.Logf("warning: directive: %s : %s", w.Directive, w.Message)
		}
	}

	diff := difflib.Diff(
		strings.Split(expectedResponse, "\n"),
		strings.Split(string(result), "\n"))

	// scan for failure
	failed := false
	for _, d := range diff {
		if d.Delta != difflib.Common {
			failed = true
			break
		}
	}

	if failed {
		for _, d := range diff {
			switch d.Delta {
			case difflib.Common:
				fmt.Printf("  %s\n", d.Payload)
			case difflib.LeftOnly:
				fmt.Printf(" - %s\n", d.Payload)
			case difflib.RightOnly:
				fmt.Printf(" + %s\n", d.Payload)
			}
		}
		t.Fail()
	}
}

// Generic request functions

func applyHeaders(t *testing.T, req *http.Request, requestHeaders []string) {
	requestContentType := ""
	for _, requestHeader := range requestHeaders {
		arr := strings.SplitAfterN(requestHeader, ":", 2)
		k := strings.TrimRight(arr[0], ":")
		v := strings.TrimSpace(arr[1])
		if k == "Content-Type" {
			requestContentType = v
		}
		t.Logf("Request header: %s => %s", k, v)
		req.Header.Set(k, v)
	}

	if requestContentType == "" {
		t.Logf("Content-Type header not provided")
	}
}

// AssertResponseCode will execute the request and verify the status code, returns a response for additional assertions
func (tc *Tester) AssertResponseCode(req *http.Request, expectedStatusCode int) *http.Response {

	resp, err := tc.Client.Do(req)
	if err != nil {
		tc.t.Fatalf("failed to call server %s", err)
	}

	if expectedStatusCode != resp.StatusCode {
		tc.t.Errorf("requesting \"%s\" expected status code: %d but got %d", req.RequestURI, expectedStatusCode, resp.StatusCode)
	}

	return resp
}

// AssertResponse request a URI and assert the status code and the body contains a string
func (tc *Tester) AssertResponse(req *http.Request, expectedStatusCode int, expectedBody string) (*http.Response, string) {

	resp := tc.AssertResponseCode(req, expectedStatusCode)

	defer resp.Body.Close()
	bytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		tc.t.Fatalf("unable to read the response body %s", err)
	}

	body := string(bytes)

	if !strings.Contains(body, expectedBody) {
		tc.t.Errorf("requesting \"%s\" expected response body \"%s\" but got \"%s\"", req.RequestURI, expectedBody, body)
	}

	return resp, body
}

// Verb specific test functions

// AssertGetResponse GET a URI and expect a statusCode and body text
func (tc *Tester) AssertGetResponse(requestURI string, expectedStatusCode int, expectedBody string) (*http.Response, string) {

	req, err := http.NewRequest("GET", requestURI, nil)
	if err != nil {
		tc.t.Fatalf("unable to create request %s", err)
	}

	return tc.AssertResponse(req, expectedStatusCode, expectedBody)
}

// AssertDeleteResponse request a URI and expect a statusCode and body text
func (tc *Tester) AssertDeleteResponse(requestURI string, expectedStatusCode int, expectedBody string) (*http.Response, string) {

	req, err := http.NewRequest("DELETE", requestURI, nil)
	if err != nil {
		tc.t.Fatalf("unable to create request %s", err)
	}

	return tc.AssertResponse(req, expectedStatusCode, expectedBody)
}

// AssertPostResponseBody POST to a URI and assert the response code and body
func (tc *Tester) AssertPostResponseBody(requestURI string, requestHeaders []string, requestBody *bytes.Buffer, expectedStatusCode int, expectedBody string) (*http.Response, string) {

	req, err := http.NewRequest("POST", requestURI, requestBody)
	if err != nil {
		tc.t.Errorf("failed to create request %s", err)
		return nil, ""
	}

	applyHeaders(tc.t, req, requestHeaders)

	return tc.AssertResponse(req, expectedStatusCode, expectedBody)
}

// AssertPutResponseBody PUT to a URI and assert the response code and body
func (tc *Tester) AssertPutResponseBody(requestURI string, requestHeaders []string, requestBody *bytes.Buffer, expectedStatusCode int, expectedBody string) (*http.Response, string) {

	req, err := http.NewRequest("PUT", requestURI, requestBody)
	if err != nil {
		tc.t.Errorf("failed to create request %s", err)
		return nil, ""
	}

	applyHeaders(tc.t, req, requestHeaders)

	return tc.AssertResponse(req, expectedStatusCode, expectedBody)
}

// AssertPatchResponseBody PATCH to a URI and assert the response code and body
func (tc *Tester) AssertPatchResponseBody(requestURI string, requestHeaders []string, requestBody *bytes.Buffer, expectedStatusCode int, expectedBody string) (*http.Response, string) {

	req, err := http.NewRequest("PATCH", requestURI, requestBody)
	if err != nil {
		tc.t.Errorf("failed to create request %s", err)
		return nil, ""
	}

	applyHeaders(tc.t, req, requestHeaders)

	return tc.AssertResponse(req, expectedStatusCode, expectedBody)
}