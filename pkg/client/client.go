package client

import (
	"bytes"
	"encoding/binary"
	"io"
	"io/ioutil"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/tink/go/hybrid"
	"github.com/google/tink/go/insecurecleartextkeyset"
	"github.com/google/tink/go/keyset"
	"github.com/google/tink/go/tink"
	"github.com/google/uuid"
	jsoniter "github.com/json-iterator/go"
	"github.com/pkg/errors"
	"github.com/projectdiscovery/interactsh/pkg/server"
	"github.com/projectdiscovery/retryablehttp-go"
	"github.com/rs/xid"
	"gopkg.in/corvus-ch/zbase32.v1"
)

var objectIDCounter = uint32(0)

// Client is a client for communicating with interactsh server instance.
type Client struct {
	correlationID     string
	secretKey         string
	serverURL         *url.URL
	httpClient        *retryablehttp.Client
	decrypter         tink.HybridDecrypt
	quitChan          chan struct{}
	persistentSession bool
}

// Options contains configuration options for interactsh client
type Options struct {
	// ServerURL is the URL for the interactsh server.
	ServerURL string
	// PersistentSession keeps the session open for future requests.
	PersistentSession bool
}

// New creates a new client instance based on provided options
func New(options *Options) (*Client, error) {
	parsed, err := url.Parse(options.ServerURL)
	if err != nil {
		return nil, errors.Wrap(err, "could not parse server URL")
	}
	// Generate a random ksuid which will be used as server secret.
	client := &Client{
		serverURL:         parsed,
		secretKey:         uuid.New().String(), // uuid as more secure
		correlationID:     xid.New().String(),
		persistentSession: options.PersistentSession,
		httpClient:        retryablehttp.NewClient(retryablehttp.DefaultOptionsSingle),
	}
	// Generate an RSA Public / Private key for interactsh client
	if err := client.generateRSAKeyPair(); err != nil {
		return nil, err
	}
	return client, nil
}

// InteractionCallback is a callback function for a reported interaction
type InteractionCallback func(*server.Interaction)

// StartPolling starts polling the server each duration and returns any events
// that may have been captured by the collaborator server.
func (c *Client) StartPolling(duration time.Duration, callback InteractionCallback) {
	ticker := time.NewTicker(duration)
	c.quitChan = make(chan struct{})
	go func() {
		for {
			select {
			case <-ticker.C:
				c.getInteractions(callback)
			case <-c.quitChan:
				ticker.Stop()
				return
			}
		}
	}()
}

// getInteractions returns the interactions from the server.
func (c *Client) getInteractions(callback InteractionCallback) {
	builder := &strings.Builder{}
	builder.WriteString(c.serverURL.String())
	builder.WriteString("/poll?id=")
	builder.WriteString(c.correlationID)
	builder.WriteString("&secret=")
	builder.WriteString(c.secretKey)
	req, err := retryablehttp.NewRequest("GET", builder.String(), nil)
	if err != nil {
		return
	}

	resp, err := c.httpClient.Do(req)
	defer func() {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
			io.Copy(ioutil.Discard, resp.Body)
		}
	}()
	if err != nil {
		return
	}
	if resp.StatusCode != 200 {
		return
	}
	response := &server.PollResponse{}
	if err := jsoniter.NewDecoder(resp.Body).Decode(response); err != nil {
		return
	}

	for _, data := range response.Data {
		plaintext, err := c.decrypter.Decrypt(data, nil)
		if err != nil {
			continue
		}
		interaction := &server.Interaction{}
		if err := jsoniter.Unmarshal(plaintext, interaction); err != nil {
			continue
		}
		callback(interaction)
	}
}

// StopPolling stops the polling to the interactsh server.
func (c *Client) StopPolling() {
	close(c.quitChan)
}

// Close closes the collaborator client and deregisters from the
// collaborator server if not explicitly asked by the user.
func (c *Client) Close() error {
	if !c.persistentSession {
		register := server.DeregisterRequest{
			CorrelationID: c.correlationID,
		}
		data, err := jsoniter.Marshal(register)
		if err != nil {
			return errors.Wrap(err, "could not marshal deregister request")
		}
		URL := c.serverURL.String() + "/deregister"
		req, err := retryablehttp.NewRequest("POST", URL, bytes.NewReader(data))
		if err != nil {
			return errors.Wrap(err, "could not create new request")
		}
		req.ContentLength = int64(len(data))

		resp, err := c.httpClient.Do(req)
		defer func() {
			if resp != nil && resp.Body != nil {
				resp.Body.Close()
				io.Copy(ioutil.Discard, resp.Body)
			}
		}()
		if err != nil {
			return errors.Wrap(err, "could not make deregister request")
		}
		if resp.StatusCode != 200 {
			return errors.Wrap(err, "could not deregister to server")
		}
	}
	return nil
}

// generateRSAKeyPair generates an RSA public-private keypair and
// registers the current client with the master server using the
// provided RSA Public Key as well as Correlation Key.
func (c *Client) generateRSAKeyPair() error {
	khPriv, err := keyset.NewHandle(hybrid.ECIESHKDFAES128CTRHMACSHA256KeyTemplate())
	if err != nil {
		return errors.Wrap(err, "could not generate encryption keyset")
	}
	hd, err := hybrid.NewHybridDecrypt(khPriv)
	if err != nil {
		return errors.Wrap(err, "could not create new decrypter")
	}
	c.decrypter = hd

	khPub, err := khPriv.Public()
	if err != nil {
		return errors.Wrap(err, "could not get keyset public-key")
	}

	exportedPub := &keyset.MemReaderWriter{}
	if err = insecurecleartextkeyset.Write(khPub, exportedPub); err != nil {
		return errors.Wrap(err, "could not write keyset public key")
	}
	keyset, err := exportedPub.Read()
	key, err := keyset.XXX_Marshal(nil, false)
	if err != nil {
		return errors.Wrap(err, "could not marshal public key")
	}

	register := server.RegisterRequest{
		PublicKey:     key,
		SecretKey:     c.secretKey,
		CorrelationID: c.correlationID,
	}
	data, err := jsoniter.Marshal(register)
	if err != nil {
		return errors.Wrap(err, "could not marshal register request")
	}
	URL := c.serverURL.String() + "/register"
	req, err := retryablehttp.NewRequest("POST", URL, bytes.NewReader(data))
	if err != nil {
		return errors.Wrap(err, "could not create new request")
	}
	req.ContentLength = int64(len(data))

	resp, err := c.httpClient.Do(req)
	defer func() {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
			io.Copy(ioutil.Discard, resp.Body)
		}
	}()
	if err != nil {
		return errors.Wrap(err, "could not make register request")
	}
	if resp.StatusCode != 200 {
		return errors.Wrap(err, "could not register to server")
	}
	return nil
}

// URL returns a new URL that can be be used for external interaction requests.
func (c *Client) URL() string {
	random := make([]byte, 8)
	i := atomic.AddUint32(&objectIDCounter, 1)
	binary.BigEndian.PutUint32(random[0:4], uint32(time.Now().Unix()))
	binary.BigEndian.PutUint32(random[4:8], i)

	builder := &strings.Builder{}
	builder.WriteString(c.correlationID)
	builder.WriteString(zbase32.StdEncoding.EncodeToString(random))
	builder.WriteString(".")
	builder.WriteString("")
	builder.WriteString(c.serverURL.Host)
	URL := builder.String()
	return URL
}
