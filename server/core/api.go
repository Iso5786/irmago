// Package core is the core of the IRMA server library, allowing IRMA verifiers, issuers
// or attribute-based signature applications to perform IRMA sessions with irmaclient instances
// (i.e. the IRMA app). It exposes a small interface to expose to other programming languages
// through cgo. It is used by the irmarequestor package but otherwise not meant for use in Go.
package core

import (
	"encoding/json"
	"io/ioutil"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/go-errors/errors"
	"github.com/privacybydesign/gabi"
	"github.com/privacybydesign/gabi/big"
	"github.com/privacybydesign/irmago"
	"github.com/privacybydesign/irmago/server"
	"github.com/sirupsen/logrus"
)

func Initialize(configuration *server.Configuration) error {
	conf = configuration

	if conf.Logger == nil {
		conf.Logger = logrus.New()
		conf.Logger.Level = logrus.DebugLevel
		conf.Logger.Formatter = &logrus.TextFormatter{}
	}
	server.Logger = conf.Logger
	irma.Logger = conf.Logger

	if conf.IrmaConfiguration == nil {
		var err error
		if conf.CachePath == "" {
			conf.IrmaConfiguration, err = irma.NewConfiguration(conf.IrmaConfigurationPath)
		} else {
			conf.IrmaConfiguration, err = irma.NewConfigurationFromAssets(
				filepath.Join(conf.CachePath, "irma_configuration"),
				conf.IrmaConfigurationPath,
			)
		}
		if err != nil {
			return server.LogError(err)
		}
		if err = conf.IrmaConfiguration.ParseFolder(); err != nil {
			return server.LogError(err)
		}
	}

	if len(conf.IrmaConfiguration.SchemeManagers) == 0 {
		if conf.DownloadDefaultSchemes {
			if err := conf.IrmaConfiguration.DownloadDefaultSchemes(); err != nil {
				return server.LogError(err)
			}
		} else {
			return server.LogError(errors.New("no schemes found in irma_configuration folder " + conf.IrmaConfiguration.Path))
		}
	}

	if conf.SchemeUpdateInterval != 0 {
		conf.IrmaConfiguration.AutoUpdateSchemes(uint(conf.SchemeUpdateInterval))
	}

	if conf.IssuerPrivateKeys == nil {
		conf.IssuerPrivateKeys = make(map[irma.IssuerIdentifier]*gabi.PrivateKey)
	}
	if conf.IssuerPrivateKeysPath != "" {
		files, err := ioutil.ReadDir(conf.IssuerPrivateKeysPath)
		if err != nil {
			return server.LogError(err)
		}
		for _, file := range files {
			filename := file.Name()
			issid := irma.NewIssuerIdentifier(strings.TrimSuffix(filename, filepath.Ext(filename))) // strip .xml
			if _, ok := conf.IrmaConfiguration.Issuers[issid]; !ok {
				return server.LogError(errors.Errorf("Private key %s belongs to an unknown issuer", filename))
			}
			sk, err := gabi.NewPrivateKeyFromFile(filepath.Join(conf.IssuerPrivateKeysPath, filename))
			if err != nil {
				return server.LogError(err)
			}
			conf.IssuerPrivateKeys[issid] = sk
		}
	}
	for issid, sk := range conf.IssuerPrivateKeys {
		pk, err := conf.IrmaConfiguration.PublicKey(issid, int(sk.Counter))
		if err != nil {
			return server.LogError(err)
		}
		if pk == nil {
			return server.LogError(errors.Errorf("Missing public key belonging to private key %s-%d", issid.String(), sk.Counter))
		}
		if new(big.Int).Mul(sk.P, sk.Q).Cmp(pk.N) != 0 {
			return server.LogError(errors.Errorf("Private key %s-%d does not belong to corresponding public key", issid.String(), sk.Counter))
		}
	}

	if conf.URL != "" {
		if !strings.HasSuffix(conf.URL, "/") {
			conf.URL = conf.URL + "/"
		}
	} else {
		conf.Logger.Warn("No url parameter specified in configuration; unless an url is elsewhere prepended in the QR, the IRMA client will not be able to connect")
	}

	return nil
}

func StartSession(req interface{}) (*irma.Qr, string, error) {
	rrequest, err := server.ParseSessionRequest(req)
	if err != nil {
		return nil, "", server.LogError(err)
	}

	request := rrequest.SessionRequest()
	action := request.Action()
	if action == irma.ActionIssuing {
		if err := validateIssuanceRequest(request.(*irma.IssuanceRequest)); err != nil {
			return nil, "", server.LogError(err)
		}
	}

	session := newSession(action, rrequest)
	conf.Logger.Infof("%s session started, token %s", action, session.token)
	if conf.Logger.IsLevelEnabled(logrus.DebugLevel) {
		conf.Logger.Debug("Session request: ", server.ToJson(rrequest))
	} else {
		logPurgedRequest(rrequest)
	}
	return &irma.Qr{
		Type: action,
		URL:  conf.URL + session.token,
	}, session.token, nil
}

func GetSessionResult(token string) *server.SessionResult {
	session := sessions.get(token)
	if session == nil {
		conf.Logger.Warn("Session result requested of unknown session ", token)
		return nil
	}
	return session.result
}

func GetRequest(token string) irma.RequestorRequest {
	session := sessions.get(token)
	if session == nil {
		conf.Logger.Warn("Session request requested of unknown session ", token)
		return nil
	}
	return session.rrequest
}

func CancelSession(token string) error {
	session := sessions.get(token)
	if session == nil {
		return server.LogError(errors.Errorf("can't cancel unknown session %s", token))
	}
	session.handleDelete()
	return nil
}

func ParsePath(path string) (string, string, error) {
	pattern := regexp.MustCompile("(\\w+)/?(|commitments|proofs|status|statusevents)$")
	matches := pattern.FindStringSubmatch(path)
	if len(matches) != 3 {
		return "", "", server.LogWarning(errors.Errorf("Invalid URL: %s", path))
	}
	return matches[1], matches[2], nil
}

func SubscribeServerSentEvents(w http.ResponseWriter, r *http.Request, token string) error {
	session := sessions.get(token)
	if session == nil {
		return server.LogError(errors.Errorf("can't subscribe to server sent events of unknown session %s", token))
	}
	if session.status.Finished() {
		return server.LogError(errors.Errorf("can't subscribe to server sent events of finished session %s", token))
	}

	session.Lock()
	defer session.Unlock()
	session.eventSource().ServeHTTP(w, r)
	return nil
}

func HandleProtocolMessage(
	path string,
	method string,
	headers map[string][]string,
	message []byte,
) (status int, output []byte, result *server.SessionResult) {
	// Parse path into session and action
	if len(path) > 0 { // Remove any starting and trailing slash
		if path[0] == '/' {
			path = path[1:]
		}
		if path[len(path)-1] == '/' {
			path = path[:len(path)-1]
		}
	}

	conf.Logger.Debugf("Routing protocol message: %s %s", method, path)
	if len(message) > 0 {
		conf.Logger.Trace("POST body: ", string(message))
	}
	conf.Logger.Trace("HTTP headers: ", server.ToJson(headers))
	token, noun, err := ParsePath(path)
	if err != nil {
		status, output = server.JsonResponse(nil, server.RemoteError(server.ErrorUnsupported, ""))
		return
	}

	// Fetch the session
	session := sessions.get(token)
	if session == nil {
		conf.Logger.Warnf("Session not found: %s", token)
		status, output = server.JsonResponse(nil, server.RemoteError(server.ErrorSessionUnknown, ""))
		return
	}
	session.Lock()
	defer session.Unlock()

	// However we return, if the session has been finished or cancelled by any of the handlers
	// then we should inform the user by returning a SessionResult - but only if we have not
	// already done this in the past, e.g. by a previous HTTP call handled by this function
	defer func() {
		if session.status.Finished() && !session.returned {
			session.returned = true
			result = session.result
			conf.Logger.Infof("Session %s done, status %s", session.token, session.result.Status)
		}
	}()

	// Route to handler
	switch len(noun) {
	case 0:
		if method == http.MethodDelete {
			session.handleDelete()
			status = http.StatusOK
			return
		}
		if method == http.MethodGet {
			h := http.Header(headers)
			min := &irma.ProtocolVersion{}
			max := &irma.ProtocolVersion{}
			if err := json.Unmarshal([]byte(h.Get(irma.MinVersionHeader)), min); err != nil {
				status, output = server.JsonResponse(nil, session.fail(server.ErrorMalformedInput, err.Error()))
				return
			}
			if err := json.Unmarshal([]byte(h.Get(irma.MaxVersionHeader)), max); err != nil {
				status, output = server.JsonResponse(nil, session.fail(server.ErrorMalformedInput, err.Error()))
				return
			}
			status, output = server.JsonResponse(session.handleGetRequest(min, max))
			return
		}
		status, output = server.JsonResponse(nil, session.fail(server.ErrorInvalidRequest, ""))
		return
	default:
		if noun == "statusevents" {
			err := server.RemoteError(server.ErrorInvalidRequest, "server sent events not supported by this server")
			status, output = server.JsonResponse(nil, err)
			return
		}

		if method == http.MethodGet && noun == "status" {
			status, output = server.JsonResponse(session.handleGetStatus())
			return
		}

		// Below are only POST enpoints
		if method != http.MethodPost {
			status, output = server.JsonResponse(nil, session.fail(server.ErrorInvalidRequest, ""))
			return
		}

		if noun == "commitments" && session.action == irma.ActionIssuing {
			commitments := &irma.IssueCommitmentMessage{}
			if err := irma.UnmarshalValidate(message, commitments); err != nil {
				status, output = server.JsonResponse(nil, session.fail(server.ErrorMalformedInput, ""))
				return
			}
			status, output = server.JsonResponse(session.handlePostCommitments(commitments))
			return
		}
		if noun == "proofs" && session.action == irma.ActionDisclosing {
			disclosure := irma.Disclosure{}
			if err := irma.UnmarshalValidate(message, &disclosure); err != nil {
				status, output = server.JsonResponse(nil, session.fail(server.ErrorMalformedInput, ""))
				return
			}
			status, output = server.JsonResponse(session.handlePostDisclosure(disclosure))
			return
		}
		if noun == "proofs" && session.action == irma.ActionSigning {
			signature := &irma.SignedMessage{}
			if err := irma.UnmarshalValidate(message, signature); err != nil {
				status, output = server.JsonResponse(nil, session.fail(server.ErrorMalformedInput, ""))
				return
			}
			status, output = server.JsonResponse(session.handlePostSignature(signature))
			return
		}

		status, output = server.JsonResponse(nil, session.fail(server.ErrorInvalidRequest, ""))
		return
	}
}