package irmago

import (
	"encoding/json"
	"encoding/xml"
	"html"
	"io/ioutil"
	"math/big"
	"time"

	"github.com/go-errors/errors"
	"github.com/mhe/gabi"
)

// This file contains the update mechanism for Client
// as well as updates themselves.

type update struct {
	When    Timestamp
	Number  int
	Success bool
	Error   *string
}

var clientUpdates = []func(client *Client) error{
	func(client *Client) error {
		_, err := client.ParseAndroidStorage()
		return err
	},
}

// update performs any function from clientUpdates that has not
// already been executed in the past, keeping track of previously executed updates
// in the file at updatesFile.
func (client *Client) update() error {
	// Load and parse file containing info about already performed updates
	var err error
	if client.updates, err = client.storage.LoadUpdates(); err != nil {
		return err
	}

	// Perform all new updates
	for i := len(client.updates); i < len(clientUpdates); i++ {
		err = clientUpdates[i](client)
		u := update{
			When:    Timestamp(time.Now()),
			Number:  i,
			Success: err == nil,
		}
		if err != nil {
			str := err.Error()
			u.Error = &str
		}
		client.updates = append(client.updates, u)
	}

	return client.storage.StoreUpdates(client.updates)
}

// ParseAndroidStorage parses an Android cardemu.xml shared preferences file
// from the old Android IRMA app, parsing its credentials into the current instance,
// and saving them to storage.
// CAREFUL: this method overwrites any existing secret keys and attributes on storage.
func (client *Client) ParseAndroidStorage() (present bool, err error) {
	if client.androidStoragePath == "" {
		return false, nil
	}

	cardemuXML := client.androidStoragePath + "/shared_prefs/cardemu.xml"
	present, err = PathExists(cardemuXML)
	if err != nil || !present {
		return
	}
	present = true

	bytes, err := ioutil.ReadFile(cardemuXML)
	if err != nil {
		return
	}
	parsedxml := struct {
		Strings []struct {
			Name    string `xml:"name,attr"`
			Content string `xml:",chardata"`
		} `xml:"string"`
	}{}
	if err = xml.Unmarshal(bytes, &parsedxml); err != nil {
		return
	}

	parsedjson := make(map[string][]*struct {
		Signature    *gabi.CLSignature `json:"signature"`
		Pk           *gabi.PublicKey   `json:"-"`
		Attributes   []*big.Int        `json:"attributes"`
		SharedPoints []*big.Int        `json:"public_sks"`
	})
	client.keyshareServers = make(map[SchemeManagerIdentifier]*keyshareServer)
	for _, xmltag := range parsedxml.Strings {
		if xmltag.Name == "credentials" {
			jsontag := html.UnescapeString(xmltag.Content)
			if err = json.Unmarshal([]byte(jsontag), &parsedjson); err != nil {
				return
			}
		}
		if xmltag.Name == "keyshare" {
			jsontag := html.UnescapeString(xmltag.Content)
			if err = json.Unmarshal([]byte(jsontag), &client.keyshareServers); err != nil {
				return
			}
		}
		if xmltag.Name == "KeyshareKeypairs" {
			jsontag := html.UnescapeString(xmltag.Content)
			keys := make([]*paillierPrivateKey, 0, 3)
			if err = json.Unmarshal([]byte(jsontag), &keys); err != nil {
				return
			}
			client.paillierKeyCache = keys[0]
		}
	}

	for _, list := range parsedjson {
		client.secretkey = &secretKey{Key: list[0].Attributes[0]}
		for _, oldcred := range list {
			gabicred := &gabi.Credential{
				Attributes: oldcred.Attributes,
				Signature:  oldcred.Signature,
			}
			if oldcred.SharedPoints != nil && len(oldcred.SharedPoints) > 0 {
				gabicred.Signature.KeyshareP = oldcred.SharedPoints[0]
			}
			var cred *credential
			if cred, err = newCredential(gabicred, client.ConfigurationStore); err != nil {
				return
			}
			if cred.CredentialType() == nil {
				err = errors.New("cannot add unknown credential type")
				return
			}

			if err = client.addCredential(cred, false); err != nil {
				return
			}
		}
	}

	if len(client.credentials) > 0 {
		if err = client.storage.StoreAttributes(client.attributes); err != nil {
			return
		}
		if err = client.storage.StoreSecretKey(client.secretkey); err != nil {
			return
		}
	}

	if len(client.keyshareServers) > 0 {
		if err = client.storage.StoreKeyshareServers(client.keyshareServers); err != nil {
			return
		}
	}
	client.UnenrolledSchemeManagers = client.unenrolledSchemeManagers()

	if err = client.storage.StorePaillierKeys(client.paillierKeyCache); err != nil {
		return
	}
	if client.paillierKeyCache == nil {
		client.paillierKey(false) // trigger calculating a new one
	}

	if err = client.ConfigurationStore.Copy(client.androidStoragePath+"/app_store/irma_configuration", false); err != nil {
		return
	}
	// Copy from assets again to ensure we have the latest versions
	return present, client.ConfigurationStore.Copy(client.irmaConfigurationPath, true)
}
