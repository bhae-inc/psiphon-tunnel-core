/*
 * Copyright (c) 2015, Psiphon Inc.
 * All rights reserved.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package psiphon

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"strconv"

	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/transferstats"
)

// Session is a utility struct which holds all of the data associated
// with a Psiphon session. In addition to the established tunnel, this
// includes the session ID (used for Psiphon API requests) and a http
// client configured to make tunneled Psiphon API requests.
type Session struct {
	sessionId            string
	baseRequestUrl       string
	psiphonHttpsClient   *http.Client
	statsRegexps         *transferstats.Regexps
	clientRegion         string
	clientUpgradeVersion string
}

// MakeSessionId creates a new session ID. Making the session ID is not done
// in NewSession because:
// (1) the transport needs to send the ID in the SSH credentials before the tunnel
//     is established and NewSession performs a handshake on an established tunnel.
// (2) the same session ID is used across multi-tunnel controller runs, where each
//     tunnel has its own Session instance.
func MakeSessionId() (sessionId string, err error) {
	randomId, err := MakeSecureRandomBytes(PSIPHON_API_CLIENT_SESSION_ID_LENGTH)
	if err != nil {
		return "", ContextError(err)
	}
	return hex.EncodeToString(randomId), nil
}

// NewSession makes the tunnelled handshake request to the
// Psiphon server and returns a Session struct, initialized with the
// session ID, for use with subsequent Psiphon server API requests (e.g.,
// periodic connected and status requests).
func NewSession(tunnel *Tunnel, sessionId string) (session *Session, err error) {

	psiphonHttpsClient, err := makePsiphonHttpsClient(tunnel)
	if err != nil {
		return nil, ContextError(err)
	}
	session = &Session{
		sessionId:          sessionId,
		baseRequestUrl:     makeBaseRequestUrl(tunnel, "", sessionId),
		psiphonHttpsClient: psiphonHttpsClient,
	}

	err = session.doHandshakeRequest()
	if err != nil {
		return nil, ContextError(err)
	}

	return session, nil
}

// DoConnectedRequest performs the connected API request. This request is
// used for statistics. The server returns a last_connected token for
// the client to store and send next time it connects. This token is
// a timestamp (using the server clock, and should be rounded to the
// nearest hour) which is used to determine when a connection represents
// a unique user for a time period.
func (session *Session) DoConnectedRequest() error {
	const DATA_STORE_LAST_CONNECTED_KEY = "lastConnected"
	lastConnected, err := GetKeyValue(DATA_STORE_LAST_CONNECTED_KEY)
	if err != nil {
		return ContextError(err)
	}
	if lastConnected == "" {
		lastConnected = "None"
	}
	url := buildRequestUrl(
		session.baseRequestUrl,
		"connected",
		&ExtraParam{"session_id", session.sessionId},
		&ExtraParam{"last_connected", lastConnected})
	responseBody, err := session.doGetRequest(url)
	if err != nil {
		return ContextError(err)
	}

	var response struct {
		ConnectedTimestamp string `json:"connected_timestamp"`
	}
	err = json.Unmarshal(responseBody, &response)
	if err != nil {
		return ContextError(err)
	}

	err = SetKeyValue(DATA_STORE_LAST_CONNECTED_KEY, response.ConnectedTimestamp)
	if err != nil {
		return ContextError(err)
	}
	return nil
}

// StatsRegexps gets the Regexps used for the statistics for this tunnel.
func (session *Session) StatsRegexps() *transferstats.Regexps {
	return session.statsRegexps
}

// DoStatusRequest makes a /status request to the server, sending session stats.
func (session *Session) DoStatusRequest(
	statsPayload json.Marshaler,
	isConnected bool) error {

	statsPayloadJSON, err := json.Marshal(statsPayload)
	if err != nil {
		return ContextError(err)
	}

	url := makeStatusRequestUrl(session.sessionId, session.baseRequestUrl, isConnected)

	err = session.doPostRequest(url, "application/json", bytes.NewReader(statsPayloadJSON))
	if err != nil {
		return ContextError(err)
	}

	return nil
}

// makeStatusRequestUrl is a helper shared by DoStatusRequest
// and doUntunneledStatusRequest.
func makeStatusRequestUrl(sessionId, baseRequestUrl string, isConnected bool) string {

	// Add a random amount of padding to help prevent stats updates from being
	// a predictable size (which often happens when the connection is quiet).
	padding := MakeSecureRandomPadding(0, PSIPHON_API_STATUS_REQUEST_PADDING_MAX_BYTES)

	connected := "1"
	if !isConnected {
		connected = "0"
	}

	return buildRequestUrl(
		baseRequestUrl,
		"status",
		&ExtraParam{"session_id", sessionId},
		&ExtraParam{"connected", connected},
		// TODO: base64 encoding of padding means the padding
		// size is not exactly [0, PADDING_MAX_BYTES]
		&ExtraParam{"padding", base64.StdEncoding.EncodeToString(padding)})
}

// doHandshakeRequest performs the handshake API request. The handshake
// returns upgrade info, newly discovered server entries -- which are
// stored -- and sponsor info (home pages, stat regexes).
func (session *Session) doHandshakeRequest() error {
	extraParams := make([]*ExtraParam, 0)
	serverEntryIpAddresses, err := GetServerEntryIpAddresses()
	if err != nil {
		return ContextError(err)
	}
	// Submit a list of known servers -- this will be used for
	// discovery statistics.
	for _, ipAddress := range serverEntryIpAddresses {
		extraParams = append(extraParams, &ExtraParam{"known_server", ipAddress})
	}
	url := buildRequestUrl(session.baseRequestUrl, "handshake", extraParams...)
	responseBody, err := session.doGetRequest(url)
	if err != nil {
		return ContextError(err)
	}
	// Skip legacy format lines and just parse the JSON config line
	configLinePrefix := []byte("Config: ")
	var configLine []byte
	for _, line := range bytes.Split(responseBody, []byte("\n")) {
		if bytes.HasPrefix(line, configLinePrefix) {
			configLine = line[len(configLinePrefix):]
			break
		}
	}
	if len(configLine) == 0 {
		return ContextError(errors.New("no config line found"))
	}

	// Note:
	// - 'preemptive_reconnect_lifetime_milliseconds' is currently unused
	// - 'ssh_session_id' is ignored; client session ID is used instead
	var handshakeConfig struct {
		Homepages            []string            `json:"homepages"`
		UpgradeClientVersion string              `json:"upgrade_client_version"`
		PageViewRegexes      []map[string]string `json:"page_view_regexes"`
		HttpsRequestRegexes  []map[string]string `json:"https_request_regexes"`
		EncodedServerList    []string            `json:"encoded_server_list"`
		ClientRegion         string              `json:"client_region"`
	}
	err = json.Unmarshal(configLine, &handshakeConfig)
	if err != nil {
		return ContextError(err)
	}

	session.clientRegion = handshakeConfig.ClientRegion
	NoticeClientRegion(session.clientRegion)

	var decodedServerEntries []*ServerEntry

	// Store discovered server entries
	for _, encodedServerEntry := range handshakeConfig.EncodedServerList {
		serverEntry, err := DecodeServerEntry(encodedServerEntry)
		if err != nil {
			return ContextError(err)
		}
		err = ValidateServerEntry(serverEntry)
		if err != nil {
			// Skip this entry and continue with the next one
			continue
		}

		decodedServerEntries = append(decodedServerEntries, serverEntry)
	}

	// The reason we are storing the entire array of server entries at once rather
	// than one at a time is that some desirable side-effects get triggered by
	// StoreServerEntries that don't get triggered by StoreServerEntry.
	err = StoreServerEntries(decodedServerEntries, true)
	if err != nil {
		return ContextError(err)
	}

	// TODO: formally communicate the sponsor and upgrade info to an
	// outer client via some control interface.
	for _, homepage := range handshakeConfig.Homepages {
		NoticeHomepage(homepage)
	}

	session.clientUpgradeVersion = handshakeConfig.UpgradeClientVersion
	if handshakeConfig.UpgradeClientVersion != "" {
		NoticeClientUpgradeAvailable(handshakeConfig.UpgradeClientVersion)
	}

	var regexpsNotices []string
	session.statsRegexps, regexpsNotices = transferstats.MakeRegexps(
		handshakeConfig.PageViewRegexes,
		handshakeConfig.HttpsRequestRegexes)

	for _, notice := range regexpsNotices {
		NoticeAlert(notice)
	}

	return nil
}

// doGetRequest makes a tunneled HTTPS request and returns the response body.
func (session *Session) doGetRequest(requestUrl string) (responseBody []byte, err error) {
	response, err := session.psiphonHttpsClient.Get(requestUrl)
	if err == nil && response.StatusCode != http.StatusOK {
		response.Body.Close()
		err = fmt.Errorf("HTTP GET request failed with response code: %d", response.StatusCode)
	}
	if err != nil {
		// Trim this error since it may include long URLs
		return nil, ContextError(TrimError(err))
	}
	defer response.Body.Close()
	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return nil, ContextError(err)
	}
	return body, nil
}

// doPostRequest makes a tunneled HTTPS POST request.
func (session *Session) doPostRequest(requestUrl string, bodyType string, body io.Reader) (err error) {
	response, err := session.psiphonHttpsClient.Post(requestUrl, bodyType, body)
	if err == nil && response.StatusCode != http.StatusOK {
		response.Body.Close()
		err = fmt.Errorf("HTTP POST request failed with response code: %d", response.StatusCode)
	}
	if err != nil {
		// Trim this error since it may include long URLs
		return ContextError(TrimError(err))
	}
	response.Body.Close()
	return nil
}

// makeBaseRequestUrl makes a URL containing all the common parameters
// that are included with Psiphon API requests. These common parameters
// are used for statistics.
func makeBaseRequestUrl(tunnel *Tunnel, port, sessionId string) string {
	var requestUrl bytes.Buffer

	if port == "" {
		port = tunnel.serverEntry.WebServerPort
	}

	// Note: don't prefix with HTTPS scheme, see comment in doGetRequest.
	// e.g., don't do this: requestUrl.WriteString("https://")
	requestUrl.WriteString("http://")
	requestUrl.WriteString(tunnel.serverEntry.IpAddress)
	requestUrl.WriteString(":")
	requestUrl.WriteString(port)
	requestUrl.WriteString("/")
	// Placeholder for the path component of a request
	requestUrl.WriteString("%s")
	requestUrl.WriteString("?client_session_id=")
	requestUrl.WriteString(sessionId)
	requestUrl.WriteString("&server_secret=")
	requestUrl.WriteString(tunnel.serverEntry.WebServerSecret)
	requestUrl.WriteString("&propagation_channel_id=")
	requestUrl.WriteString(tunnel.config.PropagationChannelId)
	requestUrl.WriteString("&sponsor_id=")
	requestUrl.WriteString(tunnel.config.SponsorId)
	requestUrl.WriteString("&client_version=")
	requestUrl.WriteString(tunnel.config.ClientVersion)
	// TODO: client_tunnel_core_version
	requestUrl.WriteString("&relay_protocol=")
	requestUrl.WriteString(tunnel.protocol)
	requestUrl.WriteString("&client_platform=")
	requestUrl.WriteString(tunnel.config.ClientPlatform)
	requestUrl.WriteString("&tunnel_whole_device=")
	requestUrl.WriteString(strconv.Itoa(tunnel.config.TunnelWholeDevice))
	return requestUrl.String()
}

type ExtraParam struct{ name, value string }

// buildRequestUrl makes a URL for an API request. The URL includes the
// base request URL and any extra parameters for the specific request.
func buildRequestUrl(baseRequestUrl, path string, extraParams ...*ExtraParam) string {
	var requestUrl bytes.Buffer
	requestUrl.WriteString(fmt.Sprintf(baseRequestUrl, path))
	for _, extraParam := range extraParams {
		requestUrl.WriteString("&")
		requestUrl.WriteString(extraParam.name)
		requestUrl.WriteString("=")
		requestUrl.WriteString(extraParam.value)
	}
	return requestUrl.String()
}

// makeHttpsClient creates a Psiphon HTTPS client that tunnels requests and which validates
// the web server using the Psiphon server entry web server certificate.
// This is not a general purpose HTTPS client.
// As the custom dialer makes an explicit TLS connection, URLs submitted to the returned
// http.Client should use the "http://" scheme. Otherwise http.Transport will try to do another TLS
// handshake inside the explicit TLS session.
func makePsiphonHttpsClient(tunnel *Tunnel) (httpsClient *http.Client, err error) {
	certificate, err := DecodeCertificate(tunnel.serverEntry.WebServerCertificate)
	if err != nil {
		return nil, ContextError(err)
	}
	tunneledDialer := func(_, addr string) (conn net.Conn, err error) {
		return tunnel.sshClient.Dial("tcp", addr)
	}
	dialer := NewCustomTLSDialer(
		&CustomTLSConfig{
			Dial:                    tunneledDialer,
			Timeout:                 PSIPHON_API_SERVER_TIMEOUT,
			VerifyLegacyCertificate: certificate,
		})
	transport := &http.Transport{
		Dial: dialer,
		ResponseHeaderTimeout: PSIPHON_API_SERVER_TIMEOUT,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   PSIPHON_API_SERVER_TIMEOUT,
	}, nil
}

// TryUntunneledStatusRequest makes direct connections to the specified
// server (if supported) in an attempt to send useful bytes transferred
// and session duration stats after a tunnel has alreay failed.
// The tunnel is assumed to be closed, but its config, protocol, and
// session values must still be valid.
// TryUntunneledStatusRequest emits notices detailing failed attempts.
func TryUntunneledStatusRequest(
	tunnel *Tunnel,
	statsPayload json.Marshaler,
	isShutdown bool) error {

	for _, port := range tunnel.serverEntry.GetDirectWebRequestPorts() {
		err := doUntunneledStatusRequest(tunnel, port, statsPayload, isShutdown)
		if err == nil {
			return nil
		}
		NoticeAlert("doUntunneledStatusRequest failed for %s:%s: %s",
			tunnel.serverEntry.IpAddress, port, err)
	}

	return errors.New("all attempts failed")
}

// doUntunneledStatusRequest attempts an untunneled stratus request.
func doUntunneledStatusRequest(
	tunnel *Tunnel,
	port string,
	statsPayload json.Marshaler,
	isShutdown bool) error {

	url := makeStatusRequestUrl(
		tunnel.session.sessionId,
		makeBaseRequestUrl(tunnel, port, tunnel.session.sessionId),
		false)

	certificate, err := DecodeCertificate(tunnel.serverEntry.WebServerCertificate)
	if err != nil {
		return ContextError(err)
	}

	timeout := PSIPHON_API_SERVER_TIMEOUT
	if isShutdown {
		timeout = PSIPHON_API_SHUTDOWN_SERVER_TIMEOUT
	}

	httpClient, requestUrl, err := MakeUntunneledHttpsClient(
		tunnel.untunneledDialConfig,
		certificate,
		url,
		timeout)
	if err != nil {
		return ContextError(err)
	}

	statsPayloadJSON, err := json.Marshal(statsPayload)
	if err != nil {
		return ContextError(err)
	}

	bodyType := "application/json"
	body := bytes.NewReader(statsPayloadJSON)

	response, err := httpClient.Post(requestUrl, bodyType, body)
	if err == nil && response.StatusCode != http.StatusOK {
		response.Body.Close()
		err = fmt.Errorf("HTTP POST request failed with response code: %d", response.StatusCode)
	}
	if err != nil {
		// Trim this error since it may include long URLs
		return ContextError(TrimError(err))
	}
	response.Body.Close()

	return nil
}
