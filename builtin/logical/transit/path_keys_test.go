package transit_test

import (
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	uuid "github.com/hashicorp/go-uuid"
	"github.com/hashicorp/vault/api"
	"github.com/hashicorp/vault/audit"
	"github.com/hashicorp/vault/builtin/audit/file"
	"github.com/hashicorp/vault/builtin/logical/transit"
	vaulthttp "github.com/hashicorp/vault/http"
	"github.com/hashicorp/vault/sdk/logical"
	"github.com/hashicorp/vault/vault"
)

func TestTransit_Issue_2958(t *testing.T) {
	coreConfig := &vault.CoreConfig{
		LogicalBackends: map[string]logical.Factory{
			"transit": transit.Factory,
		},
		AuditBackends: map[string]audit.Factory{
			"file": file.Factory,
		},
	}

	cluster := vault.NewTestCluster(t, coreConfig, &vault.TestClusterOptions{
		HandlerFunc: vaulthttp.Handler,
	})
	cluster.Start()
	defer cluster.Cleanup()

	cores := cluster.Cores

	vault.TestWaitActive(t, cores[0].Core)

	client := cores[0].Client

	err := client.Sys().EnableAuditWithOptions("file", &api.EnableAuditOptions{
		Type: "file",
		Options: map[string]string{
			"file_path": "/dev/null",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	err = client.Sys().Mount("transit", &api.MountInput{
		Type: "transit",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.Logical().Write("transit/keys/foo", map[string]interface{}{
		"type": "ecdsa-p256",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.Logical().Write("transit/keys/foobar", map[string]interface{}{
		"type": "ecdsa-p384",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.Logical().Write("transit/keys/bar", map[string]interface{}{
		"type": "ed25519",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.Logical().Read("transit/keys/foo")
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.Logical().Read("transit/keys/foobar")
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.Logical().Read("transit/keys/bar")
	if err != nil {
		t.Fatal(err)
	}
}

func TestTransit_CreateKeyWithAutorotation(t *testing.T) {
	tests := map[string]struct {
		autoRotateInterval interface{}
		shouldError        bool
		expectedValue      time.Duration
	}{
		"default (no value)": {
			shouldError: false,
		},
		"0 (int)": {
			autoRotateInterval: 0,
			shouldError:        false,
			expectedValue:      0,
		},
		"0 (string)": {
			autoRotateInterval: "0",
			shouldError:        false,
			expectedValue:      0,
		},
		"5 seconds": {
			autoRotateInterval: "5s",
			shouldError:        true,
		},
		"5 hours": {
			autoRotateInterval: "5h",
			shouldError:        false,
			expectedValue:      5 * time.Hour,
		},
		"negative value": {
			autoRotateInterval: "-1800s",
			shouldError:        true,
		},
		"invalid string": {
			autoRotateInterval: "this shouldn't work",
			shouldError:        true,
		},
	}

	coreConfig := &vault.CoreConfig{
		LogicalBackends: map[string]logical.Factory{
			"transit": transit.Factory,
		},
	}
	cluster := vault.NewTestCluster(t, coreConfig, &vault.TestClusterOptions{
		HandlerFunc: vaulthttp.Handler,
	})
	cluster.Start()
	defer cluster.Cleanup()
	cores := cluster.Cores
	vault.TestWaitActive(t, cores[0].Core)
	client := cores[0].Client
	err := client.Sys().Mount("transit", &api.MountInput{
		Type: "transit",
	})
	if err != nil {
		t.Fatal(err)
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			keyNameBytes, err := uuid.GenerateRandomBytes(16)
			if err != nil {
				t.Fatal(err)
			}
			keyName := hex.EncodeToString(keyNameBytes)

			_, err = client.Logical().Write(fmt.Sprintf("transit/keys/%s", keyName), map[string]interface{}{
				"auto_rotate_interval": test.autoRotateInterval,
			})
			switch {
			case test.shouldError && err == nil:
				t.Fatal("expected non-nil error")
			case !test.shouldError && err != nil:
				t.Fatal(err)
			}

			if !test.shouldError {
				resp, err := client.Logical().Read(fmt.Sprintf("transit/keys/%s", keyName))
				if err != nil {
					t.Fatal(err)
				}
				if resp == nil {
					t.Fatal("expected non-nil response")
				}
				got := resp.Data["auto_rotate_interval"]
				want := test.expectedValue.String()
				if got != want {
					t.Fatalf("incorrect auto_rotate_interval returned, got: %s, want: %s", got, want)
				}
			}
		})
	}
}
