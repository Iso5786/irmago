package irmago

import (
	"fmt"
	"math/big"
	"os"
	"testing"
	"time"

	"encoding/json"

	"github.com/mhe/gabi"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	retCode := m.Run()

	// TODO make testdata/storage

	err := os.RemoveAll("testdata/storage/test")
	if err != nil {
		fmt.Println("Could not delete test storage")
		os.Exit(1)
	}

	os.Exit(retCode)
}

type IgnoringClientHandler struct{}

func (i *IgnoringClientHandler) UpdateConfigurationStore(new *IrmaIdentifierSet)            {}
func (i *IgnoringClientHandler) UpdateAttributes()                                          {}
func (i *IgnoringClientHandler) EnrollmentError(manager SchemeManagerIdentifier, err error) {}
func (i *IgnoringClientHandler) EnrollmentSuccess(manager SchemeManagerIdentifier)          {}

func parseStorage(t *testing.T) *Client {
	exists, err := PathExists("testdata/storage/test")
	require.NoError(t, err, "pathexists() failed")
	if !exists {
		require.NoError(t, os.Mkdir("testdata/storage/test", 0755), "Could not create test storage")
	}
	manager, err := NewClient(
		"testdata/storage/test",
		"testdata/irma_configuration",
		"testdata/oldstorage",
		&IgnoringClientHandler{},
	)
	require.NoError(t, err)
	return manager
}

func teardown(t *testing.T) {
	require.NoError(t, os.RemoveAll("testdata/storage/test"))
}

// A convenience function for initializing big integers from known correct (10
// base) strings. Use with care, errors are ignored.
func s2big(s string) (r *big.Int) {
	r, _ = new(big.Int).SetString(s, 10)
	return
}

func verifyManagerIsUnmarshaled(t *testing.T, client *Client) {
	cred, err := client.credential(NewCredentialTypeIdentifier("irma-demo.RU.studentCard"), 0)
	require.NoError(t, err, "could not fetch credential")
	require.NotNil(t, cred, "Credential should exist")
	require.NotNil(t, cred.Attributes[0], "Metadata attribute of irma-demo.RU.studentCard should not be nil")

	cred, err = client.credential(NewCredentialTypeIdentifier("test.test.mijnirma"), 0)
	require.NoError(t, err, "could not fetch credential")
	require.NotNil(t, cred, "Credential should exist")
	require.NotNil(t, cred.Signature.KeyshareP)

	require.NotEmpty(t, client.CredentialInfoList())

	pk, err := cred.PublicKey()
	require.NoError(t, err)
	require.True(t,
		cred.Signature.Verify(pk, cred.Attributes),
		"Credential should be valid",
	)
}

func verifyCredentials(t *testing.T, client *Client) {
	var pk *gabi.PublicKey
	var err error
	for credtype, credsmap := range client.credentials {
		for index, cred := range credsmap {
			pk, err = cred.PublicKey()
			require.NoError(t, err)
			require.True(t,
				cred.Credential.Signature.Verify(pk, cred.Attributes),
				"Credential %s-%d was invalid", credtype.String(), index,
			)
			require.Equal(t, cred.Attributes[0], client.secretkey.Key,
				"Secret key of credential %s-%d unequal to main secret key",
				cred.CredentialType().Identifier().String(), index,
			)
		}
	}
}

func verifyPaillierKey(t *testing.T, PrivateKey *paillierPrivateKey) {
	require.NotNil(t, PrivateKey)
	require.NotNil(t, PrivateKey.L)
	require.NotNil(t, PrivateKey.U)
	require.NotNil(t, PrivateKey.PublicKey.N)

	require.Equal(t, big.NewInt(1), new(big.Int).Exp(big.NewInt(2), PrivateKey.L, PrivateKey.N))
	require.Equal(t, PrivateKey.NSquared, new(big.Int).Exp(PrivateKey.N, big.NewInt(2), nil))

	plaintext := "Hello Paillier!"
	ciphertext, err := PrivateKey.Encrypt([]byte(plaintext))
	require.NoError(t, err)
	decrypted, err := PrivateKey.Decrypt(ciphertext)
	require.NoError(t, err)
	require.Equal(t, plaintext, string(decrypted))
}

func verifyKeyshareIsUnmarshaled(t *testing.T, client *Client) {
	require.NotNil(t, client.paillierKeyCache)
	require.NotNil(t, client.keyshareServers)
	test := NewSchemeManagerIdentifier("test")
	require.Contains(t, client.keyshareServers, test)
	kss := client.keyshareServers[test]
	require.NotEmpty(t, kss.Nonce)

	verifyPaillierKey(t, kss.PrivateKey)
	verifyPaillierKey(t, client.paillierKeyCache)
}

func verifyStoreIsLoaded(t *testing.T, store *ConfigurationStore, android bool) {
	require.Contains(t, store.SchemeManagers, NewSchemeManagerIdentifier("irma-demo"))
	require.Contains(t, store.SchemeManagers, NewSchemeManagerIdentifier("test"))
	if android {
		require.Contains(t, store.SchemeManagers, NewSchemeManagerIdentifier("test2"))
	}

	pk, err := store.PublicKey(NewIssuerIdentifier("irma-demo.RU"), 0)
	require.NoError(t, err)
	require.NotNil(t, pk)
	require.NotNil(t, pk.N, "irma-demo.RU public key has no modulus")
	require.Equal(t,
		"Irma Demo",
		store.SchemeManagers[NewSchemeManagerIdentifier("irma-demo")].Name["en"],
		"irma-demo scheme manager has unexpected name")
	require.Equal(t,
		"Radboud University Nijmegen",
		store.Issuers[NewIssuerIdentifier("irma-demo.RU")].Name["en"],
		"irma-demo.RU issuer has unexpected name")
	require.Equal(t,
		"Student Card",
		store.CredentialTypes[NewCredentialTypeIdentifier("irma-demo.RU.studentCard")].ShortName["en"],
		"irma-demo.RU.studentCard has unexpected name")

	require.Equal(t,
		"studentID",
		store.CredentialTypes[NewCredentialTypeIdentifier("irma-demo.RU.studentCard")].Attributes[2].ID,
		"irma-demo.RU.studentCard.studentID has unexpected name")

	// Hash algorithm pseudocode:
	// Base64(SHA256("irma-demo.RU.studentCard")[0:16])
	require.Contains(t, store.reverseHashes, "1stqlPad5edpfS1Na1U+DA==",
		"irma-demo.RU.studentCard had improper hash")
	require.Contains(t, store.reverseHashes, "CLjnADMBYlFcuGOT7Z0xRg==",
		"irma-demo.MijnOverheid.root had improper hash")
}

func TestAndroidParse(t *testing.T) {
	client := parseStorage(t)
	verifyStoreIsLoaded(t, client.ConfigurationStore, true)
	verifyManagerIsUnmarshaled(t, client)
	verifyCredentials(t, client)
	verifyKeyshareIsUnmarshaled(t, client)

	teardown(t)
}

func TestUnmarshaling(t *testing.T) {
	client := parseStorage(t)

	// Do session so we can examine its log item later
	logs, err := client.Logs()
	require.NoError(t, err)
	jwt := getIssuanceJwt("testip", NewAttributeTypeIdentifier("irma-demo.RU.studentCard.studentID"))
	sessionHelper(t, jwt, "issue", client)

	newclient, err := NewClient("testdata/storage/test", "testdata/irma_configuration", "testdata/oldstorage", nil)
	require.NoError(t, err)
	verifyManagerIsUnmarshaled(t, newclient)
	verifyCredentials(t, newclient)
	verifyKeyshareIsUnmarshaled(t, newclient)

	newlogs, err := newclient.Logs()
	require.NoError(t, err)
	require.True(t, len(newlogs) == len(logs)+1)

	entry := newlogs[len(newlogs)-1]
	require.NotNil(t, entry)
	sessionjwt, err := entry.Jwt()
	require.NoError(t, err)
	require.Equal(t, "testip", sessionjwt.(*IdentityProviderJwt).ServerName)
	require.NoError(t, err)
	require.NotEmpty(t, entry.Disclosed)
	require.NotEmpty(t, entry.Received)
	response, err := entry.GetResponse()
	require.NoError(t, err)
	require.NotNil(t, response)
	require.IsType(t, &gabi.IssueCommitmentMessage{}, response)

	teardown(t)
}

func TestMetadataAttribute(t *testing.T) {
	metadata := NewMetadataAttribute()
	if metadata.Version() != 0x02 {
		t.Errorf("Unexpected metadata version: %d", metadata.Version())
	}

	expiry := metadata.SigningDate().Unix() + int64(metadata.ValidityDuration()*ExpiryFactor)
	if !time.Unix(expiry, 0).Equal(metadata.Expiry()) {
		t.Errorf("Invalid signing date")
	}

	if metadata.KeyCounter() != 0 {
		t.Errorf("Unexpected key counter")
	}
}

func TestMetadataCompatibility(t *testing.T) {
	store, err := NewConfigurationStore("testdata/irma_configuration", "")
	require.NoError(t, err)
	require.NoError(t, store.ParseFolder())

	// An actual metadata attribute of an IRMA credential extracted from the IRMA app
	attr := MetadataFromInt(s2big("49043481832371145193140299771658227036446546573739245068"), store)
	require.NotNil(t, attr.CredentialType(), "attr.CredentialType() should not be nil")

	require.Equal(t,
		NewCredentialTypeIdentifier("irma-demo.RU.studentCard"),
		attr.CredentialType().Identifier(),
		"Metadata credential type was not irma-demo.RU.studentCard",
	)
	require.Equal(t, byte(0x02), attr.Version(), "Unexpected metadata version")
	require.Equal(t, time.Unix(1499904000, 0), attr.SigningDate(), "Unexpected signing date")
	require.Equal(t, time.Unix(1516233600, 0), attr.Expiry(), "Unexpected expiry date")
	require.Equal(t, 2, attr.KeyCounter(), "Unexpected key counter")

	teardown(t)
}

func TestAttributeDisjunctionMarshaling(t *testing.T) {
	store, err := NewConfigurationStore("testdata/irma_configuration", "")
	require.NoError(t, err)
	require.NoError(t, store.ParseFolder())
	disjunction := AttributeDisjunction{}

	var _ json.Unmarshaler = &disjunction
	var _ json.Marshaler = &disjunction

	id := NewAttributeTypeIdentifier("MijnOverheid.ageLower.over18")

	attrsjson := `
	{
		"label": "Over 18",
		"attributes": {
			"MijnOverheid.ageLower.over18": "yes",
			"Thalia.age.over18": "Yes"
		}
	}`
	require.NoError(t, json.Unmarshal([]byte(attrsjson), &disjunction))
	require.True(t, disjunction.HasValues())
	require.Contains(t, disjunction.Attributes, id)
	require.Contains(t, disjunction.Values, id)
	require.Equal(t, disjunction.Values[id], "yes")

	disjunction = AttributeDisjunction{}
	attrsjson = `
	{
		"label": "Over 18",
		"attributes": [
			"MijnOverheid.ageLower.over18",
			"Thalia.age.over18"
		]
	}`
	require.NoError(t, json.Unmarshal([]byte(attrsjson), &disjunction))
	require.False(t, disjunction.HasValues())
	require.Contains(t, disjunction.Attributes, id)

	require.True(t, disjunction.MatchesStore(store))

	require.False(t, disjunction.Satisfied())
	disjunction.selected = &disjunction.Attributes[0]
	require.True(t, disjunction.Satisfied())
}

func TestCandidates(t *testing.T) {
	client := parseStorage(t)

	attrtype := NewAttributeTypeIdentifier("irma-demo.RU.studentCard.studentID")
	disjunction := &AttributeDisjunction{
		Attributes: []AttributeTypeIdentifier{attrtype},
	}
	attrs := client.Candidates(disjunction)
	require.NotNil(t, attrs)
	require.Len(t, attrs, 1)

	attr := attrs[0]
	require.NotNil(t, attr)
	require.Equal(t, attr.Type, attrtype)

	disjunction = &AttributeDisjunction{
		Attributes: []AttributeTypeIdentifier{attrtype},
		Values:     map[AttributeTypeIdentifier]string{attrtype: "456"},
	}
	attrs = client.Candidates(disjunction)
	require.NotNil(t, attrs)
	require.Len(t, attrs, 1)

	disjunction = &AttributeDisjunction{
		Attributes: []AttributeTypeIdentifier{attrtype},
		Values:     map[AttributeTypeIdentifier]string{attrtype: "foobarbaz"},
	}
	attrs = client.Candidates(disjunction)
	require.NotNil(t, attrs)
	require.Empty(t, attrs)

	teardown(t)
}

func TestTimestamp(t *testing.T) {
	mytime := Timestamp(time.Unix(1500000000, 0))
	timestruct := struct{ Time *Timestamp }{Time: &mytime}
	bytes, err := json.Marshal(timestruct)
	require.NoError(t, err)

	timestruct = struct{ Time *Timestamp }{}
	require.NoError(t, json.Unmarshal(bytes, &timestruct))
	require.Equal(t, time.Time(*timestruct.Time).Unix(), int64(1500000000))
}

func TestServiceProvider(t *testing.T) {
	var spjwt ServiceProviderJwt

	var spjson = `{
		"sprequest": {
			"validity": 60,
			"timeout": 60,
			"request": {
				"content": [
					{
						"label": "ID",
						"attributes": ["irma-demo.RU.studentCard.studentID"]
					}
				]
			}
		}
	}`

	require.NoError(t, json.Unmarshal([]byte(spjson), &spjwt))
	require.NotNil(t, spjwt.Request.Request.Content)
	require.NotEmpty(t, spjwt.Request.Request.Content)
	require.NotNil(t, spjwt.Request.Request.Content[0])
	require.NotEmpty(t, spjwt.Request.Request.Content[0])
	require.NotNil(t, spjwt.Request.Request.Content[0].Attributes)
	require.NotEmpty(t, spjwt.Request.Request.Content[0].Attributes)
	require.Equal(t, spjwt.Request.Request.Content[0].Attributes[0].Name(), "studentID")

	require.NotNil(t, spjwt.Request.Request.Content.Find(NewAttributeTypeIdentifier("irma-demo.RU.studentCard.studentID")))
}

func TestTransport(t *testing.T) {
	transport := NewHTTPTransport("https://xkcd.com")
	obj := &struct {
		Num   int    `json:"num"`
		Img   string `json:"img"`
		Title string `json:"title"`
	}{}

	err := transport.Get("614/info.0.json", obj)
	if err != nil { // require.NoError() does not work because of the type of err
		t.Fatalf("%+v\n", err)
	}
}

func TestPaillier(t *testing.T) {
	client := parseStorage(t)

	challenge, _ := gabi.RandomBigInt(256)
	comm, _ := gabi.RandomBigInt(1000)
	resp, _ := gabi.RandomBigInt(1000)

	sk := client.paillierKey(true)
	bytes, err := sk.Encrypt(challenge.Bytes())
	require.NoError(t, err)
	cipher := new(big.Int).SetBytes(bytes)

	bytes, err = sk.Encrypt(comm.Bytes())
	require.NoError(t, err)
	commcipher := new(big.Int).SetBytes(bytes)

	// [[ c ]]^resp * [[ comm ]]
	cipher.Exp(cipher, resp, sk.NSquared).Mul(cipher, commcipher).Mod(cipher, sk.NSquared)

	bytes, err = sk.Decrypt(cipher.Bytes())
	require.NoError(t, err)
	plaintext := new(big.Int).SetBytes(bytes)
	expected := new(big.Int).Set(challenge)
	expected.Mul(expected, resp).Add(expected, comm)

	require.Equal(t, plaintext, expected)

	teardown(t)
}

func TestCredentialRemoval(t *testing.T) {
	client := parseStorage(t)
	id := NewCredentialTypeIdentifier("irma-demo.RU.studentCard")
	id2 := NewCredentialTypeIdentifier("test.test.mijnirma")

	cred, err := client.credential(id, 0)
	require.NoError(t, err)
	require.NotNil(t, cred)
	err = client.RemoveCredentialByHash(cred.AttributeList().hash())
	require.NoError(t, err)
	cred, err = client.credential(id, 0)
	require.NoError(t, err)
	require.Nil(t, cred)

	cred, err = client.credential(id2, 0)
	require.NoError(t, err)
	require.NotNil(t, cred)
	err = client.RemoveCredential(id2, 0)
	require.NoError(t, err)
	cred, err = client.credential(id2, 0)
	require.NoError(t, err)
	require.Nil(t, cred)

	teardown(t)
}

func TestDownloadSchemeManager(t *testing.T) {
	client := parseStorage(t)
	require.NoError(t, client.ConfigurationStore.RemoveSchemeManager(NewSchemeManagerIdentifier("irma-demo")))
	url := "https://raw.githubusercontent.com/credentials/irma_configuration/translate/irma-demo"
	sm, err := client.ConfigurationStore.DownloadSchemeManager(url)
	require.NoError(t, err)
	require.NotNil(t, sm)

	require.NoError(t, client.ConfigurationStore.AddSchemeManager(sm))

	jwt := getIssuanceJwt("testip", NewAttributeTypeIdentifier("irma-demo.RU.studentCard.studentID"))
	sessionHelper(t, jwt, "issue", client)

	teardown(t)
}
