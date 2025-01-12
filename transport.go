package digest

import (
	"bytes"
	"io"
	"net/http"
	"sync"
)

type cached struct {
	chal  *Challenge
	count int
}

// Transport implements http.RoundTripper
type Transport struct {
	Username      string
	Password      string
	Transport     http.RoundTripper
	Digest        func(*http.Request, *Challenge, Options) (*Credentials, error)
	FindChallenge func(http.Header) (*Challenge, error)
	Jar           http.CookieJar

	cacheMu sync.Mutex
	cache   map[string]*cached
}

// save parses the digest challenge from the response
// and adds it to the cache
func (t *Transport) save(res *http.Response) error {
	// save cookies
	if t.Jar != nil {
		t.Jar.SetCookies(res.Request.URL, res.Cookies())
	}
	// find and save digest challenge
	find := t.FindChallenge
	if find == nil {
		find = FindChallenge
	}
	chal, err := find(res.Header)
	if err != nil {
		return err
	}
	host := res.Request.URL.Hostname()
	t.cacheMu.Lock()
	if t.cache == nil {
		t.cache = map[string]*cached{}
	}
	// TODO: if the challenge contains a domain, we should be using that
	//       to match against outgoing requests. We're currently ignoring
	//       it and just matching the hostname.
	t.cache[host] = &cached{chal: chal}
	t.cacheMu.Unlock()
	return nil
}

// digest creates credentials from the cached challenge
func (t *Transport) digest(req *http.Request, chal *Challenge, count int) (*Credentials, error) {
	opt := Options{
		Method:   req.Method,
		URI:      req.URL.RequestURI(),
		Count:    count,
		Username: t.Username,
		Password: t.Password,
	}
	if t.Digest != nil {
		return t.Digest(req, chal, opt)
	}
	return Digest(chal, opt)
}

// challenge returns a cached challenge and count for the provided request
func (t *Transport) challenge(req *http.Request) (*Challenge, int, bool) {
	t.cacheMu.Lock()
	defer t.cacheMu.Unlock()
	host := req.URL.Hostname()
	cc, ok := t.cache[host]
	if !ok {
		return nil, 0, false
	}
	cc.count++
	return cc.chal, cc.count, true
}

// prepare attempts to find a cached challenge that matches the
// requested domain, and use it to set the Authorization header
func (t *Transport) prepare(req *http.Request) error {
	// add cookies
	if t.Jar != nil {
		for _, cookie := range t.Jar.Cookies(req.URL) {
			req.AddCookie(cookie)
		}
	}
	// add auth
	chal, count, ok := t.challenge(req)
	if !ok {
		return nil
	}
	cred, err := t.digest(req, chal, count)
	if err != nil {
		return err
	}
	
	if cred != nil {
		req.Header.Set("Authorization", cred.String())
	}
	
	return nil
}

// RoundTrip will try to authorize the request using a cached challenge.
// If that doesn't work and we receive a 401, we'll try again using that challenge.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	// use the configured transport if there is one
	tr := t.Transport
	if tr == nil {
		tr = http.DefaultTransport
	}
	// don't modify the original request
	clone, err := cloner(req)
	if err != nil {
		return nil, err
	}
	// make a copy of the request
	first, err := clone()
	if err != nil {
		return nil, err
	}
	// prepare the first request using a cached challenge
	if err := t.prepare(first); err != nil {
		return nil, err
	}
	// the first request will either succeed or return a 401
	res, err := tr.RoundTrip(first)
	if err != nil || res.StatusCode != http.StatusUnauthorized {
		return res, err
	}
	// drain and close the first message body
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	// save the challenge for future use
	if err := t.save(res); err != nil {
		return nil, err
	}
	// make a second copy of the request
	second, err := clone()
	if err != nil {
		return nil, err
	}
	// prepare the second request based on the new challenge
	if err := t.prepare(second); err != nil {
		return nil, err
	}
	return tr.RoundTrip(second)
}

// cloner returns a function which makes clones of the provided request
func cloner(req *http.Request) (func() (*http.Request, error), error) {
	getbody := req.GetBody
	if getbody == nil && req.Body != nil {
		// if there's no GetBody function set we have to copy the body
		// into memory to use for future clones
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		getbody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
	}
	return func() (*http.Request, error) {
		clone := req.Clone(req.Context())
		if getbody != nil {
			body, err := getbody()
			if err != nil {
				return nil, err
			}
			clone.Body = body
		}
		return clone, nil
	}, nil
}
