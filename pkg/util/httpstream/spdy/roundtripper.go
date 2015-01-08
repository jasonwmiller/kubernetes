/*
Copyright 2015 Google Inc. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package spdy

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/kubernetes/pkg/util"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/util/httpstream"
)

// SpdyRoundTripper knows how to upgrade an HTTP request to one that supports
// multiplexed streams. After RoundTrip() is invoked, Conn will be set
// and usable. SpdyRoundTripper implements the UpgradeRoundTripper interface.
type SpdyRoundTripper struct {
	//tlsConfig holds the TLS configuration settings to use when connecting
	//to the remote server.
	tlsConfig *tls.Config

	/* TODO according to http://golang.org/pkg/net/http/#RoundTripper, a RoundTripper
	   must be safe for use by multiple concurrent goroutines. If this is absolutely
	   necessary, we could keep a map from http.Request to net.Conn. In practice,
	   a client will create an http.Client, set the transport to a new insteace of
	   SpdyRoundTripper, and use it a single time, so this hopefully won't be an issue.
	*/
	// conn is the underlying network connection to the remote server.
	conn net.Conn
}

// NewSpdyRoundTripper creates a new SpdyRoundTripper that will use
// the specified tlsConfig.
func NewRoundTripper(tlsConfig *tls.Config) httpstream.UpgradeRoundTripper {
	return &SpdyRoundTripper{tlsConfig: tlsConfig}
}

// dial dials the host specified by req, using TLS if appropriate.
func (s *SpdyRoundTripper) dial(req *http.Request) (net.Conn, error) {
	dialAddr := util.CanonicalAddr(req.URL)

	if req.URL.Scheme == "http" {
		return net.Dial("tcp", dialAddr)
	}

	// TODO validate the TLSClientConfig is set up?
	conn, err := tls.Dial("tcp", dialAddr, s.tlsConfig)
	if err != nil {
		return nil, err
	}

	host, _, err := net.SplitHostPort(dialAddr)
	if err != nil {
		return nil, err
	}
	err = conn.VerifyHostname(host)
	if err != nil {
		return nil, err
	}

	return conn, nil
}

// RoundTrip executes the Request and upgrades it. After a successful upgrade,
// clients may call SpdyRoundTripper.Connection() to retrieve the upgraded
// connection.
func (s *SpdyRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// TODO what's the best way to clone the request?
	r := *req
	req = &r
	req.Header.Add(httpstream.HeaderConnection, httpstream.HeaderUpgrade)
	req.Header.Add(httpstream.HeaderUpgrade, HeaderSpdy31)

	conn, err := s.dial(req)
	if err != nil {
		return nil, err
	}

	err = req.Write(conn)
	if err != nil {
		return nil, err
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return nil, err
	}

	s.conn = conn

	return resp, nil
}

// NewConnection validates the upgrade response, creating and returning a new
// httpstream.Connection if there were no errors.
func (s *SpdyRoundTripper) NewConnection(resp *http.Response) (httpstream.Connection, error) {
	connectionHeader := strings.ToLower(resp.Header.Get(httpstream.HeaderConnection))
	upgradeHeader := strings.ToLower(resp.Header.Get(httpstream.HeaderUpgrade))
	if !strings.Contains(connectionHeader, strings.ToLower(httpstream.HeaderUpgrade)) || !strings.Contains(upgradeHeader, strings.ToLower(HeaderSpdy31)) {
		responseError := ""
		responseErrorBytes, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			responseError = "Unable to read error from server response"
		} else {
			responseError = string(responseErrorBytes)
		}

		return nil, fmt.Errorf("Unable to upgrade connection: %s", responseError)
	}

	return NewClientConnection(s.conn)
}
