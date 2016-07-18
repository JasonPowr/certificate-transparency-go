// Package client is a CT log client implementation and contains types and code
// for interacting with RFC6962-compliant CT Log instances.
// See http://tools.ietf.org/html/rfc6962 for details
package client

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"strconv"
	"time"

	ct "github.com/google/certificate-transparency/go"
	"golang.org/x/net/context"
)

// URI paths for CT Log endpoints
const (
	AddChainPath    = "/ct/v1/add-chain"
	AddPreChainPath = "/ct/v1/add-pre-chain"
	AddJSONPath     = "/ct/v1/add-json"
	GetSTHPath      = "/ct/v1/get-sth"
	GetEntriesPath  = "/ct/v1/get-entries"
)

// LogClient represents a client for a given CT Log instance
type LogClient struct {
	uri        string       // the base URI of the log. e.g. http://ct.googleapis/pilot
	httpClient *http.Client // used to interact with the log via HTTP
}

//////////////////////////////////////////////////////////////////////////////////
// JSON structures follow.
// These represent the structures returned by the CT Log server.
//////////////////////////////////////////////////////////////////////////////////

// addChainRequest represents the JSON request body sent to the add-chain CT
// method.
type addChainRequest struct {
	Chain [][]byte `json:"chain"`
}

// addChainResponse represents the JSON response to the add-chain CT method.
// An SCT represents a Log's promise to integrate a [pre-]certificate into the
// log within a defined period of time.
type addChainResponse struct {
	SCTVersion ct.Version `json:"sct_version"` // SCT structure version
	ID         []byte     `json:"id"`          // Log ID
	Timestamp  uint64     `json:"timestamp"`   // Timestamp of issuance
	Extensions string     `json:"extensions"`  // Holder for any CT extensions
	Signature  []byte     `json:"signature"`   // Log signature for this SCT
}

// addJSONRequest represents the JSON request body sent ot the add-json CT
// method.
type addJSONRequest struct {
	Data interface{} `json:"data"`
}

// getSTHResponse respresents the JSON response to the get-sth CT method
type getSTHResponse struct {
	TreeSize          uint64 `json:"tree_size"`           // Number of certs in the current tree
	Timestamp         uint64 `json:"timestamp"`           // Time that the tree was created
	SHA256RootHash    []byte `json:"sha256_root_hash"`    // Root hash of the tree
	TreeHeadSignature []byte `json:"tree_head_signature"` // Log signature for this STH
}

// getConsistencyProofResponse represents the JSON response to the CT get-consistency-proof method
type getConsistencyProofResponse struct {
	Consistency []string `json:"consistency"`
}

// getAuditProofResponse represents the JSON response to the CT get-audit-proof method
type getAuditProofResponse struct {
	Hash     []string `json:"hash"`      // the hashes which make up the proof
	TreeSize uint64   `json:"tree_size"` // the tree size against which this proof is constructed
}

// getAcceptedRootsResponse represents the JSON response to the CT get-roots method.
type getAcceptedRootsResponse struct {
	Certificates []string `json:"certificates"`
}

// getEntryAndProodReponse represents the JSON response to the CT get-entry-and-proof method
type getEntryAndProofResponse struct {
	LeafInput string   `json:"leaf_input"` // the entry itself
	ExtraData string   `json:"extra_data"` // any chain provided when the entry was added to the log
	AuditPath []string `json:"audit_path"` // the corresponding proof
}

// New constructs a new LogClient instance.
// |uri| is the base URI of the CT log instance to interact with, e.g.
// http://ct.googleapis.com/pilot
// |hc| is the underlying client to be used for HTTP requests to the CT log.
func New(uri string, hc *http.Client) *LogClient {
	if hc == nil {
		hc = new(http.Client)
	}
	return &LogClient{uri: uri, httpClient: hc}
}

// Makes a HTTP call to |uri|, and attempts to parse the response as a
// JSON representation of the structure in |res|. Uses |ctx| to
// control the HTTP call (so it can have a timeout or be cancelled by
// the caller), and |httpClient| to make the actual HTTP call.
// Returns a non-nil |error| if there was a problem.
func fetchAndParse(ctx context.Context, httpClient *http.Client, uri string, res interface{}) error {
	req, err := http.NewRequest(http.MethodGet, uri, nil)
	if err != nil {
		return err
	}
	req.Cancel = ctx.Done()
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Make sure everything is read, so http.Client can reuse the connection.
	defer ioutil.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		return fmt.Errorf("got HTTP Status %s", resp.Status)
	}

	if err := json.NewDecoder(resp.Body).Decode(res); err != nil {
		return err
	}

	return nil
}

// Makes a HTTP POST call to |uri|, and attempts to parse the response as a JSON
// representation of the structure in |res|.
// Returns a non-nil |error| if there was a problem.
func (c *LogClient) postAndParse(uri string, req interface{}, res interface{}) (*http.Response, string, error) {
	postBody, err := json.Marshal(req)
	if err != nil {
		return nil, "", err
	}
	httpReq, err := http.NewRequest(http.MethodPost, uri, bytes.NewReader(postBody))
	if err != nil {
		return nil, "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(httpReq)
	// Read all of the body, if there is one, so that the http.Client can do
	// Keep-Alive:
	var body []byte
	if resp != nil {
		body, err = ioutil.ReadAll(resp.Body)
		resp.Body.Close()
	}
	if err != nil {
		return resp, string(body), err
	}
	if resp.StatusCode == 200 {
		if err != nil {
			return resp, string(body), err
		}
		if err = json.Unmarshal(body, &res); err != nil {
			return resp, string(body), err
		}
	}
	return resp, string(body), nil
}

func backoffForRetry(ctx context.Context, d time.Duration) error {
	backoffTimer := time.NewTimer(d)
	if ctx != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-backoffTimer.C:
		}
	} else {
		<-backoffTimer.C
	}
	return nil
}

// Attempts to add |chain| to the log, using the api end-point specified by
// |path|. If provided context expires before submission is complete an
// error will be returned.
func (c *LogClient) addChainWithRetry(ctx context.Context, path string, chain []ct.ASN1Cert) (*ct.SignedCertificateTimestamp, error) {
	var resp addChainResponse
	var req addChainRequest
	for _, link := range chain {
		req.Chain = append(req.Chain, link)
	}
	httpStatus := "Unknown"
	backoffSeconds := 0
	done := false
	for !done {
		if backoffSeconds > 0 {
			log.Printf("Got %s, backing-off %d seconds", httpStatus, backoffSeconds)
		}
		err := backoffForRetry(ctx, time.Second*time.Duration(backoffSeconds))
		if err != nil {
			return nil, err
		}
		if backoffSeconds > 0 {
			backoffSeconds = 0
		}
		httpResp, errorBody, err := c.postAndParse(c.uri+path, &req, &resp)
		if err != nil {
			backoffSeconds = 10
			continue
		}
		switch {
		case httpResp.StatusCode == 200:
			done = true
		case httpResp.StatusCode == 408:
			// request timeout, retry immediately
		case httpResp.StatusCode == 503:
			// Retry
			backoffSeconds = 10
			if retryAfter := httpResp.Header.Get("Retry-After"); retryAfter != "" {
				if seconds, err := strconv.Atoi(retryAfter); err == nil {
					backoffSeconds = seconds
				}
			}
		default:
			return nil, fmt.Errorf("got HTTP Status %s: %s", httpResp.Status, errorBody)
		}
		httpStatus = httpResp.Status
	}

	ds, err := ct.UnmarshalDigitallySigned(bytes.NewReader(resp.Signature))
	if err != nil {
		return nil, err
	}
	var logID ct.SHA256Hash
	copy(logID[:], resp.ID)
	return &ct.SignedCertificateTimestamp{
		SCTVersion: resp.SCTVersion,
		LogID:      logID,
		Timestamp:  resp.Timestamp,
		Extensions: ct.CTExtensions(resp.Extensions),
		Signature:  *ds}, nil
}

// AddChain adds the (DER represented) X509 |chain| to the log.
func (c *LogClient) AddChain(chain []ct.ASN1Cert) (*ct.SignedCertificateTimestamp, error) {
	return c.addChainWithRetry(nil, AddChainPath, chain)
}

// AddPreChain adds the (DER represented) Precertificate |chain| to the log.
func (c *LogClient) AddPreChain(chain []ct.ASN1Cert) (*ct.SignedCertificateTimestamp, error) {
	return c.addChainWithRetry(nil, AddPreChainPath, chain)
}

// AddChainWithContext adds the (DER represented) X509 |chain| to the log and
// fails if the provided context expires before the chain is submitted.
func (c *LogClient) AddChainWithContext(ctx context.Context, chain []ct.ASN1Cert) (*ct.SignedCertificateTimestamp, error) {
	return c.addChainWithRetry(ctx, AddChainPath, chain)
}

func (c *LogClient) AddJSON(data interface{}) (*ct.SignedCertificateTimestamp, error) {
	req := addJSONRequest{
		Data: data,
	}
	var resp addChainResponse
	_, _, err := c.postAndParse(c.uri+AddJSONPath, &req, &resp)
	if err != nil {
		return nil, err
	}
	ds, err := ct.UnmarshalDigitallySigned(bytes.NewReader(resp.Signature))
	if err != nil {
		return nil, err
	}
	var logID ct.SHA256Hash
	copy(logID[:], resp.ID)
	return &ct.SignedCertificateTimestamp{
		SCTVersion: resp.SCTVersion,
		LogID:      logID,
		Timestamp:  resp.Timestamp,
		Extensions: ct.CTExtensions(resp.Extensions),
		Signature:  *ds}, nil
}

// GetSTH retrieves the current STH from the log.
// Returns a populated SignedTreeHead, or a non-nil error.
func (c *LogClient) GetSTH() (sth *ct.SignedTreeHead, err error) {
	var resp getSTHResponse
	if err = fetchAndParse(context.TODO(), c.httpClient, c.uri+GetSTHPath, &resp); err != nil {
		return
	}
	sth = &ct.SignedTreeHead{
		TreeSize:  resp.TreeSize,
		Timestamp: resp.Timestamp,
	}

	if len(resp.SHA256RootHash) != sha256.Size {
		return nil, fmt.Errorf("sha256_root_hash is invalid length, expected %d got %d", sha256.Size, len(resp.SHA256RootHash))
	}
	copy(sth.SHA256RootHash[:], resp.SHA256RootHash)

	ds, err := ct.UnmarshalDigitallySigned(bytes.NewReader(resp.TreeHeadSignature))
	if err != nil {
		return nil, err
	}
	// TODO(alcutter): Verify signature
	sth.TreeHeadSignature = *ds
	return
}
