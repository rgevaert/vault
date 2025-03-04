package http

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	kv "github.com/hashicorp/vault-plugin-secrets-kv"
	"github.com/hashicorp/vault/api"
	auditFile "github.com/hashicorp/vault/builtin/audit/file"
	"github.com/hashicorp/vault/internalshared/configutil"
	"github.com/hashicorp/vault/sdk/helper/consts"
	"github.com/hashicorp/vault/sdk/helper/logging"
	"github.com/hashicorp/vault/sdk/logical"
	"github.com/hashicorp/vault/sdk/physical"
	"github.com/hashicorp/vault/sdk/physical/inmem"

	"github.com/go-test/deep"
	log "github.com/hashicorp/go-hclog"

	"github.com/hashicorp/vault/audit"
	"github.com/hashicorp/vault/helper/namespace"
	"github.com/hashicorp/vault/vault"
)

func TestLogical(t *testing.T) {
	core, _, token := vault.TestCoreUnsealed(t)
	ln, addr := TestServer(t, core)
	defer ln.Close()
	TestServerAuth(t, addr, token)

	// WRITE
	resp := testHttpPut(t, token, addr+"/v1/secret/foo", map[string]interface{}{
		"data": "bar",
	})
	testResponseStatus(t, resp, 204)

	// READ
	// Bad token should return a 403
	resp = testHttpGet(t, token+"bad", addr+"/v1/secret/foo")
	testResponseStatus(t, resp, 403)

	resp = testHttpGet(t, token, addr+"/v1/secret/foo")
	var actual map[string]interface{}
	var nilWarnings interface{}
	expected := map[string]interface{}{
		"renewable":      false,
		"lease_duration": json.Number(strconv.Itoa(int((32 * 24 * time.Hour) / time.Second))),
		"data": map[string]interface{}{
			"data": "bar",
		},
		"auth":      nil,
		"wrap_info": nil,
		"warnings":  nilWarnings,
	}
	testResponseStatus(t, resp, 200)
	testResponseBody(t, resp, &actual)
	delete(actual, "lease_id")
	expected["request_id"] = actual["request_id"]
	if diff := deep.Equal(actual, expected); diff != nil {
		t.Fatal(diff)
	}

	// DELETE
	resp = testHttpDelete(t, token, addr+"/v1/secret/foo")
	testResponseStatus(t, resp, 204)

	resp = testHttpGet(t, token, addr+"/v1/secret/foo")
	testResponseStatus(t, resp, 404)
}

func TestLogical_noExist(t *testing.T) {
	core, _, token := vault.TestCoreUnsealed(t)
	ln, addr := TestServer(t, core)
	defer ln.Close()
	TestServerAuth(t, addr, token)

	resp := testHttpGet(t, token, addr+"/v1/secret/foo")
	testResponseStatus(t, resp, 404)
}

func TestLogical_StandbyRedirect(t *testing.T) {
	ln1, addr1 := TestListener(t)
	defer ln1.Close()
	ln2, addr2 := TestListener(t)
	defer ln2.Close()

	// Create an HA Vault
	logger := logging.NewVaultLogger(log.Debug)

	inmha, err := inmem.NewInmemHA(nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	conf := &vault.CoreConfig{
		Physical:     inmha,
		HAPhysical:   inmha.(physical.HABackend),
		RedirectAddr: addr1,
		DisableMlock: true,
	}
	core1, err := vault.NewCore(conf)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer core1.Shutdown()
	keys, root := vault.TestCoreInit(t, core1)
	for _, key := range keys {
		if _, err := core1.Unseal(vault.TestKeyCopy(key)); err != nil {
			t.Fatalf("unseal err: %s", err)
		}
	}

	// Attempt to fix raciness in this test by giving the first core a chance
	// to grab the lock
	time.Sleep(2 * time.Second)

	// Create a second HA Vault
	conf2 := &vault.CoreConfig{
		Physical:     inmha,
		HAPhysical:   inmha.(physical.HABackend),
		RedirectAddr: addr2,
		DisableMlock: true,
	}
	core2, err := vault.NewCore(conf2)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	defer core2.Shutdown()
	for _, key := range keys {
		if _, err := core2.Unseal(vault.TestKeyCopy(key)); err != nil {
			t.Fatalf("unseal err: %s", err)
		}
	}

	TestServerWithListener(t, ln1, addr1, core1)
	TestServerWithListener(t, ln2, addr2, core2)
	TestServerAuth(t, addr1, root)

	// WRITE to STANDBY
	resp := testHttpPutDisableRedirect(t, root, addr2+"/v1/secret/foo", map[string]interface{}{
		"data": "bar",
	})
	logger.Debug("307 test one starting")
	testResponseStatus(t, resp, 307)
	logger.Debug("307 test one stopping")

	//// READ to standby
	resp = testHttpGet(t, root, addr2+"/v1/auth/token/lookup-self")
	var actual map[string]interface{}
	var nilWarnings interface{}
	expected := map[string]interface{}{
		"renewable":      false,
		"lease_duration": json.Number("0"),
		"data": map[string]interface{}{
			"meta":             nil,
			"num_uses":         json.Number("0"),
			"path":             "auth/token/root",
			"policies":         []interface{}{"root"},
			"display_name":     "root",
			"orphan":           true,
			"id":               root,
			"ttl":              json.Number("0"),
			"creation_ttl":     json.Number("0"),
			"explicit_max_ttl": json.Number("0"),
			"expire_time":      nil,
			"entity_id":        "",
			"type":             "service",
		},
		"warnings":  nilWarnings,
		"wrap_info": nil,
		"auth":      nil,
	}

	testResponseStatus(t, resp, 200)
	testResponseBody(t, resp, &actual)
	actualDataMap := actual["data"].(map[string]interface{})
	delete(actualDataMap, "creation_time")
	delete(actualDataMap, "accessor")
	actual["data"] = actualDataMap
	expected["request_id"] = actual["request_id"]
	delete(actual, "lease_id")
	if diff := deep.Equal(actual, expected); diff != nil {
		t.Fatal(diff)
	}

	//// DELETE to standby
	resp = testHttpDeleteDisableRedirect(t, root, addr2+"/v1/secret/foo")
	logger.Debug("307 test two starting")
	testResponseStatus(t, resp, 307)
	logger.Debug("307 test two stopping")
}

func TestLogical_CreateToken(t *testing.T) {
	core, _, token := vault.TestCoreUnsealed(t)
	ln, addr := TestServer(t, core)
	defer ln.Close()
	TestServerAuth(t, addr, token)

	// WRITE
	resp := testHttpPut(t, token, addr+"/v1/auth/token/create", map[string]interface{}{
		"data": "bar",
	})

	var actual map[string]interface{}
	var nilWarnings interface{}
	expected := map[string]interface{}{
		"lease_id":       "",
		"renewable":      false,
		"lease_duration": json.Number("0"),
		"data":           nil,
		"wrap_info":      nil,
		"auth": map[string]interface{}{
			"policies":       []interface{}{"root"},
			"token_policies": []interface{}{"root"},
			"metadata":       nil,
			"lease_duration": json.Number("0"),
			"renewable":      false,
			"entity_id":      "",
			"token_type":     "service",
			"orphan":         false,
			"num_uses":       json.Number("0"),
		},
		"warnings": nilWarnings,
	}
	testResponseStatus(t, resp, 200)
	testResponseBody(t, resp, &actual)
	delete(actual["auth"].(map[string]interface{}), "client_token")
	delete(actual["auth"].(map[string]interface{}), "accessor")
	expected["request_id"] = actual["request_id"]
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("bad:\nexpected:\n%#v\nactual:\n%#v", expected, actual)
	}
}

func TestLogical_RawHTTP(t *testing.T) {
	core, _, token := vault.TestCoreUnsealed(t)
	ln, addr := TestServer(t, core)
	defer ln.Close()
	TestServerAuth(t, addr, token)

	resp := testHttpPost(t, token, addr+"/v1/sys/mounts/foo", map[string]interface{}{
		"type": "http",
	})
	testResponseStatus(t, resp, 204)

	// Get the raw response
	resp = testHttpGet(t, token, addr+"/v1/foo/raw")
	testResponseStatus(t, resp, 200)

	// Test the headers
	if resp.Header.Get("Content-Type") != "plain/text" {
		t.Fatalf("Bad: %#v", resp.Header)
	}

	// Get the body
	body := new(bytes.Buffer)
	io.Copy(body, resp.Body)
	if string(body.Bytes()) != "hello world" {
		t.Fatalf("Bad: %s", body.Bytes())
	}
}

func TestLogical_RequestSizeLimit(t *testing.T) {
	core, _, token := vault.TestCoreUnsealed(t)
	ln, addr := TestServer(t, core)
	defer ln.Close()
	TestServerAuth(t, addr, token)

	// Write a very large object, should fail. This test works because Go will
	// convert the byte slice to base64, which makes it significantly larger
	// than the default max request size.
	resp := testHttpPut(t, token, addr+"/v1/secret/foo", map[string]interface{}{
		"data": make([]byte, DefaultMaxRequestSize),
	})
	testResponseStatus(t, resp, http.StatusRequestEntityTooLarge)
}

func TestLogical_RequestSizeDisableLimit(t *testing.T) {
	core, _, token := vault.TestCoreUnsealed(t)
	ln, addr := TestListener(t)
	props := &vault.HandlerProperties{
		Core: core,
		ListenerConfig: &configutil.Listener{
			MaxRequestSize: -1,
			Address:        "127.0.0.1",
			TLSDisable:     true,
		},
	}
	TestServerWithListenerAndProperties(t, ln, addr, core, props)

	defer ln.Close()
	TestServerAuth(t, addr, token)

	// Write a very large object, should pass as MaxRequestSize set to -1/Negative value

	resp := testHttpPut(t, token, addr+"/v1/secret/foo", map[string]interface{}{
		"data": make([]byte, DefaultMaxRequestSize),
	})
	testResponseStatus(t, resp, http.StatusNoContent)
}

func TestLogical_ListSuffix(t *testing.T) {
	core, _, rootToken := vault.TestCoreUnsealed(t)
	req, _ := http.NewRequest("GET", "http://127.0.0.1:8200/v1/secret/foo", nil)
	req = req.WithContext(namespace.RootContext(nil))
	req.Header.Add(consts.AuthHeaderName, rootToken)

	lreq, _, status, err := buildLogicalRequest(core, nil, req)
	if err != nil {
		t.Fatal(err)
	}
	if status != 0 {
		t.Fatalf("got status %d", status)
	}
	if strings.HasSuffix(lreq.Path, "/") {
		t.Fatal("trailing slash found on path")
	}

	req, _ = http.NewRequest("GET", "http://127.0.0.1:8200/v1/secret/foo?list=true", nil)
	req = req.WithContext(namespace.RootContext(nil))
	req.Header.Add(consts.AuthHeaderName, rootToken)

	lreq, _, status, err = buildLogicalRequest(core, nil, req)
	if err != nil {
		t.Fatal(err)
	}
	if status != 0 {
		t.Fatalf("got status %d", status)
	}
	if !strings.HasSuffix(lreq.Path, "/") {
		t.Fatal("trailing slash not found on path")
	}

	req, _ = http.NewRequest("LIST", "http://127.0.0.1:8200/v1/secret/foo", nil)
	req = req.WithContext(namespace.RootContext(nil))
	req.Header.Add(consts.AuthHeaderName, rootToken)

	_, _, status, err = buildLogicalRequestNoAuth(core.PerfStandby(), nil, req)
	if err != nil || status != 0 {
		t.Fatal(err)
	}

	lreq, _, status, err = buildLogicalRequest(core, nil, req)
	if err != nil {
		t.Fatal(err)
	}
	if status != 0 {
		t.Fatalf("got status %d", status)
	}
	if !strings.HasSuffix(lreq.Path, "/") {
		t.Fatal("trailing slash not found on path")
	}
}

func TestLogical_RespondWithStatusCode(t *testing.T) {
	resp := &logical.Response{
		Data: map[string]interface{}{
			"test-data": "foo",
		},
	}

	resp404, err := logical.RespondWithStatusCode(resp, &logical.Request{ID: "id"}, http.StatusNotFound)
	if err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	respondLogical(nil, w, nil, nil, resp404, false)

	if w.Code != 404 {
		t.Fatalf("Bad Status code: %d", w.Code)
	}

	bodyRaw, err := ioutil.ReadAll(w.Body)
	if err != nil {
		t.Fatal(err)
	}

	expected := `{"request_id":"id","lease_id":"","renewable":false,"lease_duration":0,"data":{"test-data":"foo"},"wrap_info":null,"warnings":null,"auth":null}`

	if string(bodyRaw[:]) != strings.Trim(expected, "\n") {
		t.Fatalf("bad response: %s", string(bodyRaw[:]))
	}
}

func TestLogical_Audit_invalidWrappingToken(t *testing.T) {
	// Create a noop audit backend
	var noop *vault.NoopAudit
	c, _, root := vault.TestCoreUnsealedWithConfig(t, &vault.CoreConfig{
		AuditBackends: map[string]audit.Factory{
			"noop": func(ctx context.Context, config *audit.BackendConfig) (audit.Backend, error) {
				noop = &vault.NoopAudit{
					Config: config,
				}
				return noop, nil
			},
		},
	})
	ln, addr := TestServer(t, c)
	defer ln.Close()

	// Enable the audit backend

	resp := testHttpPost(t, root, addr+"/v1/sys/audit/noop", map[string]interface{}{
		"type": "noop",
	})
	testResponseStatus(t, resp, 204)

	{
		// Make a wrapping/unwrap request with an invalid token
		resp := testHttpPost(t, root, addr+"/v1/sys/wrapping/unwrap", map[string]interface{}{
			"token": "foo",
		})
		testResponseStatus(t, resp, 400)
		body := map[string][]string{}
		testResponseBody(t, resp, &body)
		if body["errors"][0] != "wrapping token is not valid or does not exist" {
			t.Fatal(body)
		}

		// Check the audit trail on request and response
		if len(noop.ReqAuth) != 1 {
			t.Fatalf("bad: %#v", noop)
		}
		auth := noop.ReqAuth[0]
		if auth.ClientToken != root {
			t.Fatalf("bad client token: %#v", auth)
		}
		if len(noop.Req) != 1 || noop.Req[0].Path != "sys/wrapping/unwrap" {
			t.Fatalf("bad:\ngot:\n%#v", noop.Req[0])
		}

		if len(noop.ReqErrs) != 1 {
			t.Fatalf("bad: %#v", noop.RespErrs)
		}
		if noop.ReqErrs[0] != consts.ErrInvalidWrappingToken {
			t.Fatalf("bad: %#v", noop.ReqErrs)
		}
	}

	{
		resp := testHttpPostWrapped(t, root, addr+"/v1/auth/token/create", nil, 10*time.Second)
		testResponseStatus(t, resp, 200)
		body := map[string]interface{}{}
		testResponseBody(t, resp, &body)

		wrapToken := body["wrap_info"].(map[string]interface{})["token"].(string)

		// Make a wrapping/unwrap request with an invalid token
		resp = testHttpPost(t, root, addr+"/v1/sys/wrapping/unwrap", map[string]interface{}{
			"token": wrapToken,
		})
		testResponseStatus(t, resp, 200)

		// Check the audit trail on request and response
		if len(noop.ReqAuth) != 3 {
			t.Fatalf("bad: %#v", noop)
		}
		auth := noop.ReqAuth[2]
		if auth.ClientToken != root {
			t.Fatalf("bad client token: %#v", auth)
		}
		if len(noop.Req) != 3 || noop.Req[2].Path != "sys/wrapping/unwrap" {
			t.Fatalf("bad:\ngot:\n%#v", noop.Req[2])
		}

		// Make sure there is only one error in the logs
		if noop.ReqErrs[1] != nil || noop.ReqErrs[2] != nil {
			t.Fatalf("bad: %#v", noop.RespErrs)
		}
	}
}

func TestLogical_ShouldParseForm(t *testing.T) {
	const formCT = "application/x-www-form-urlencoded"

	tests := map[string]struct {
		prefix      string
		contentType string
		isForm      bool
	}{
		"JSON":                 {`{"a":42}`, formCT, false},
		"JSON 2":               {`[42]`, formCT, false},
		"JSON w/leading space": {"   \n\n\r\t  [42]  ", formCT, false},
		"Form":                 {"a=42&b=dog", formCT, true},
		"Form w/wrong CT":      {"a=42&b=dog", "application/json", false},
	}

	for name, test := range tests {
		isForm := isForm([]byte(test.prefix), test.contentType)

		if isForm != test.isForm {
			t.Fatalf("%s fail: expected isForm %t, got %t", name, test.isForm, isForm)
		}
	}
}

func TestLogical_AuditPort(t *testing.T) {
	coreConfig := &vault.CoreConfig{
		LogicalBackends: map[string]logical.Factory{
			"kv": kv.VersionedKVFactory,
		},
		AuditBackends: map[string]audit.Factory{
			"file": auditFile.Factory,
		},
	}

	cluster := vault.NewTestCluster(t, coreConfig, &vault.TestClusterOptions{
		HandlerFunc: Handler,
	})

	cluster.Start()
	defer cluster.Cleanup()

	cores := cluster.Cores

	core := cores[0].Core
	c := cluster.Cores[0].Client
	vault.TestWaitActive(t, core)

	if err := c.Sys().Mount("kv/", &api.MountInput{
		Type: "kv-v2",
	}); err != nil {
		t.Fatalf("kv-v2 mount attempt failed - err: %#v\n", err)
	}

	auditLogFile, err := ioutil.TempFile("", "auditport")
	if err != nil {
		t.Fatal(err)
	}

	err = c.Sys().EnableAuditWithOptions("file", &api.EnableAuditOptions{
		Type: "file",
		Options: map[string]string{
			"file_path": auditLogFile.Name(),
		},
	})
	if err != nil {
		t.Fatalf("failed to enable audit file, err: %#v\n", err)
	}

	writeData := map[string]interface{}{
		"data": map[string]interface{}{
			"bar": "a",
		},
	}

	resp, err := c.Logical().Write("kv/data/foo", writeData)
	if err != nil {
		t.Fatalf("write request failed, err: %#v, resp: %#v\n", err, resp)
	}

	decoder := json.NewDecoder(auditLogFile)

	var auditRecord map[string]interface{}
	count := 0
	for decoder.Decode(&auditRecord) == nil {
		count += 1

		// Skip the first line
		if count == 1 {
			continue
		}

		auditRequest := map[string]interface{}{}

		if req, ok := auditRecord["request"]; ok {
			auditRequest = req.(map[string]interface{})
		}

		if _, ok := auditRequest["remote_address"].(string); !ok {
			t.Fatalf("remote_port should be a number, not %T", auditRequest["remote_address"])
		}

		if _, ok := auditRequest["remote_port"].(float64); !ok {
			t.Fatalf("remote_port should be a number, not %T", auditRequest["remote_port"])
		}
	}

	if count != 4 {
		t.Fatalf("wrong number of audit entries: %d", count)
	}
}
