package ad_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ubuntu/adsys/internal/policies/ad"
	"github.com/ubuntu/adsys/internal/testutils"
)

func TestAdsysGPOList(t *testing.T) {
	coverageOn := testutils.PythonCoverageToGoFormat(t, "adsys-gpolist", false)
	adsysGPOListcmd := "./adsys-gpolist"
	if coverageOn {
		adsysGPOListcmd = "adsys-gpolist"
	}

	// Setup samba mock
	orig := os.Getenv("PYTHONPATH")
	p, err := filepath.Abs("../../testutils/admock")
	require.NoError(t, err, "Setup: Failed to get current absolute path for mock")
	require.NoError(t, os.Setenv("PYTHONPATH", p), "Setup: Failed to set $PYTHONPATH")
	t.Cleanup(func() {
		require.NoError(t, os.Setenv("PYTHONPATH", orig), "Teardown: can't restore $PYTHONPATH to original value")
	})

	tests := map[string]struct {
		url             string
		accountName     string
		objectClass     string
		krb5ccNameState string

		wantErr        bool
		wantReturnCode int
	}{
		"Return one gpo": {
			accountName: "UserAtRoot",
		},

		"Return hierarchy": {
			accountName: "RnDUser",
		},
		"Multiple GPOs in same OU": {
			accountName: "RnDUserDep1",
		},

		"Machine GPOs": {
			accountName: "hostname1",
			objectClass: "computer",
		},

		"Disabled GPOs": {
			accountName: "RnDUserDep3",
		},

		"No GPO on OU": {
			accountName: "UserNoGPO",
		},

		// Filtering cases
		"Filter user only GPOs": {
			accountName: "hostname2",
			objectClass: "computer",
		},
		"Filter machine only GPOs": {
			accountName: "RnDUserDep7",
		},

		// Forced GPOs and inheritance handling
		"Forced GPO are first by reverse order": {
			accountName: "RndUserSubDep2ForcedPolicy",
		},
		"Block inheritance": {
			accountName: "RnDUserWithBlockedInheritance",
		},
		"Forced GPO and blocked inheritance": {
			accountName: "RnDUserWithBlockedInheritanceAndForcedPolicies",
		},

		// Access cases
		"Security descriptor missing ignores GPO": { // AD is doing that for windows client
			accountName: "RnDUserDep4",
		},
		"Fail on security descriptor access failure": {
			accountName:    "RnDUserDep5",
			wantReturnCode: 3,
			wantErr:        true,
		},
		"Security descriptor access denied ignores GPO": {
			accountName: "RnDUserDep6",
		},
		"Security descriptor accepted is for another user": {
			accountName: "RnDUserDep8",
		},

		"No gPOptions fallbacks to 0": {
			accountName: "UserNogPOptions",
		},

		"KRB5CCNAME without FILE: is supported by the samba bindings": {
			accountName:     "UserAtRoot",
			krb5ccNameState: "invalidenvformat",
		},

		// Error cases
		"Fail on no network": {
			url:            "ldap://NT_STATUS_NETWORK_UNREACHABLE",
			accountName:    "UserAtRoot",
			wantReturnCode: 2,
			wantErr:        true,
		},
		"Fail on unreachable ldap host": {
			url:            "ldap://NT_STATUS_HOST_UNREACHABLE",
			accountName:    "UserAtRoot",
			wantReturnCode: 2,
			wantErr:        true,
		},
		"Fail on ldap connection refused": {
			url:            "ldap://NT_STATUS_CONNECTION_REFUSED",
			accountName:    "UserAtRoot",
			wantReturnCode: 2,
			wantErr:        true,
		},
		"Fail on machine with no ldap": {
			url:            "ldap://NT_STATUS_OBJECT_NAME_NOT_FOUND",
			accountName:    "UserAtRoot",
			wantReturnCode: 2,
			wantErr:        true,
		},

		"Fail on non existent account": {
			accountName:    "nonexistent",
			wantReturnCode: 1,
			wantErr:        true,
		},
		"Fail on user requested but found machine": {
			accountName:    "hostname1",
			objectClass:    "user",
			wantReturnCode: 1,
			wantErr:        true,
		},
		"Fail on computer requested but found user": {
			accountName:    "UserAtRoot",
			objectClass:    "computer",
			wantReturnCode: 1,
			wantErr:        true,
		},
		"Fail invalid GPO link": {
			accountName:    "UserInvalidLink",
			wantReturnCode: 3,
			wantErr:        true,
		},

		"Fail on KRB5CCNAME unset": {
			accountName:     "UserAtRoot",
			krb5ccNameState: "unset",
			wantReturnCode:  1,
			wantErr:         true,
		},
		"Fail on invalid ticket": {
			accountName:     "UserAtRoot",
			krb5ccNameState: "invalid",
			wantReturnCode:  1,
			wantErr:         true,
		},
		"Fail on dangling ticket symlink": {
			accountName:     "UserAtRoot",
			krb5ccNameState: "dangling",
			wantReturnCode:  1,
			wantErr:         true,
		},
	}
	for name, tc := range tests {
		tc := tc
		t.Run(name, func(t *testing.T) {
			if tc.objectClass == "" {
				tc.objectClass = "user"
			}
			if tc.url == "" {
				tc.url = "ldap://ldap_url"
			}

			// Ticket creation for mock
			if tc.krb5ccNameState != "unset" {
				krb5dir := t.TempDir()
				krb5file := filepath.Join(krb5dir, "krb5file")
				krb5symlink := filepath.Join(krb5dir, "krb5symlink")
				content := "Some data for the mock"
				if tc.krb5ccNameState == "invalid" {
					content = "Some invalid ticket content for the mock"
				}
				if tc.krb5ccNameState != "dangling" {
					err = os.WriteFile(krb5file, []byte(content), 0600)
					require.NoError(t, err, "Setup: could not set create krb5file")
				}

				err = os.Symlink(krb5file, krb5symlink)
				require.NoError(t, err, "Setup: could not set krb5 file adsys symlink")

				krb5ccname := fmt.Sprintf("FILE:%s", krb5symlink)
				if tc.krb5ccNameState == "invalidenvformat" {
					krb5ccname = krb5symlink
				}

				orig := os.Getenv("KRB5CCNAME")
				err := os.Setenv("KRB5CCNAME", krb5ccname)
				require.NoError(t, err, "Setup: could not set KRB5CCNAME environment name")
				defer func() {
					err := os.Setenv("KRB5CCNAME", orig)
					require.NoError(t, err, "Teardown: could not restore KRB5CCNAME environment name")
				}()
			}

			cmd := exec.Command(adsysGPOListcmd, "--objectclass", tc.objectClass, tc.url, tc.accountName)
			got, err := cmd.CombinedOutput()
			assert.Equal(t, tc.wantReturnCode, cmd.ProcessState.ExitCode(), "adsys-gpostlist returns expected exit code")
			if tc.wantErr {
				require.Error(t, err, "adsys-gpostlist should have failed but didn’t")
				return
			}
			require.NoErrorf(t, err, "adsys-gpostlist should exit successfully: %v", string(got))

			// check collected output between FormatGPO calls
			goldPath := filepath.Join("testdata", "adsys-gpolist", "golden", name)
			// Update golden file
			if ad.Update {
				t.Logf("updating golden file %s", goldPath)
				err = os.WriteFile(goldPath, got, 0644)
				require.NoError(t, err, "Cannot write golden file")
			}
			want, err := os.ReadFile(goldPath)
			require.NoError(t, err, "Cannot load policy golden file")

			require.Equal(t, string(want), string(got), "adsys-gpolist expected output")

		})
	}
}
