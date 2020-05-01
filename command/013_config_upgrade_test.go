package command

import (
	"bytes"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	svchost "github.com/hashicorp/terraform-svchost"
	"github.com/hashicorp/terraform-svchost/disco"
	"github.com/hashicorp/terraform/helper/copy"
	"github.com/hashicorp/terraform/internal/getproviders"
	"github.com/mitchellh/cli"
)

// This map from provider type name to namespace is used by the fake registry
// when called via LookupLegacyProvider. Providers not in this map will return
// a 404 Not Found error.
var legacyProviderNamespaces = map[string]string{
	"foo": "hashicorp",
	"bar": "hashicorp",
	"baz": "terraform-providers",
}

func TestZeroThirteenUpgrade_success(t *testing.T) {
	registrySource, close := testRegistrySource(t)
	defer close()

	testCases := map[string]struct {
		path string
		args []string
		out  string
	}{
		"implicit": {
			path: "013upgrade-implicit-providers",
			out:  "providers.tf",
		},
		"explicit": {
			path: "013upgrade-explicit-providers",
			out:  "providers.tf",
		},
		"provider not found": {
			path: "013upgrade-provider-not-found",
			out:  "providers.tf",
		},
		"file exists": {
			path: "013upgrade-file-exists",
			out:  "providers-1.tf",
		},
	}
	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			td := tempDir(t)
			copy.CopyDir(testFixturePath(tc.path), td)
			defer os.RemoveAll(td)
			defer testChdir(t, td)()

			ui := new(cli.MockUi)
			c := &ZeroThirteenUpgradeCommand{
				Meta: Meta{
					testingOverrides: metaOverridesForProvider(testProvider()),
					ProviderSource:   registrySource,
					Ui:               ui,
				},
			}

			if code := c.Run(tc.args); code != 0 {
				t.Fatalf("bad: \n%s", ui.ErrorWriter.String())
			}

			output := ui.OutputWriter.String()
			if !strings.Contains(output, "Upgrade complete") {
				t.Fatal("unexpected output:", output)
			}

			actual, err := ioutil.ReadFile(tc.out)
			if err != nil {
				t.Fatalf("failed to read output %s: %s", tc.out, err)
			}
			expected, err := ioutil.ReadFile("expected/providers.tf")
			if err != nil {
				t.Fatal("failed to read expected/providers.tf", err)
			}

			if !bytes.Equal(actual, expected) {
				t.Fatalf("actual output: \n%s\nexpected output: \n%s", string(actual), string(expected))
			}
		})
	}
}

func TestZeroThirteenUpgrade_invalidFlags(t *testing.T) {
	td := tempDir(t)
	os.MkdirAll(td, 0755)
	defer os.RemoveAll(td)
	defer testChdir(t, td)()

	ui := new(cli.MockUi)
	c := &ZeroThirteenUpgradeCommand{
		Meta: Meta{
			testingOverrides: metaOverridesForProvider(testProvider()),
			Ui:               ui,
		},
	}

	if code := c.Run([]string{"--whoops"}); code == 0 {
		t.Fatal("expected error, got:", ui.OutputWriter)
	}

	errMsg := ui.ErrorWriter.String()
	if !strings.Contains(errMsg, "Usage: terraform 0.13upgrade") {
		t.Fatal("unexpected error:", errMsg)
	}
}

func TestZeroThirteenUpgrade_tooManyArguments(t *testing.T) {
	td := tempDir(t)
	os.MkdirAll(td, 0755)
	defer os.RemoveAll(td)
	defer testChdir(t, td)()

	ui := new(cli.MockUi)
	c := &ZeroThirteenUpgradeCommand{
		Meta: Meta{
			testingOverrides: metaOverridesForProvider(testProvider()),
			Ui:               ui,
		},
	}

	if code := c.Run([]string{".", "./modules/test"}); code == 0 {
		t.Fatal("expected error, got:", ui.OutputWriter)
	}

	errMsg := ui.ErrorWriter.String()
	if !strings.Contains(errMsg, "Error: Too many arguments") {
		t.Fatal("unexpected error:", errMsg)
	}
}

func TestZeroThirteenUpgrade_empty(t *testing.T) {
	td := tempDir(t)
	os.MkdirAll(td, 0755)
	defer os.RemoveAll(td)
	defer testChdir(t, td)()

	ui := new(cli.MockUi)
	c := &ZeroThirteenUpgradeCommand{
		Meta: Meta{
			testingOverrides: metaOverridesForProvider(testProvider()),
			Ui:               ui,
		},
	}

	if code := c.Run(nil); code == 0 {
		t.Fatal("expected error, got:", ui.OutputWriter)
	}

	errMsg := ui.ErrorWriter.String()
	if !strings.Contains(errMsg, "Not a module directory") {
		t.Fatal("unexpected error:", errMsg)
	}
}

// testServices starts up a local HTTP server running a fake provider registry
// service which responds only to discovery requests and legacy provider lookup
// API calls.
//
// The final return value is a function to call at the end of a test function
// to shut down the test server. After you call that function, the discovery
// object becomes useless.
func testServices(t *testing.T) (services *disco.Disco, cleanup func()) {
	server := httptest.NewServer(http.HandlerFunc(fakeRegistryHandler))

	services = disco.New()
	services.ForceHostServices(svchost.Hostname("registry.terraform.io"), map[string]interface{}{
		"providers.v1": server.URL + "/providers/v1/",
	})

	return services, func() {
		server.Close()
	}
}

// testRegistrySource is a wrapper around testServices that uses the created
// discovery object to produce a Source instance that is ready to use with the
// fake registry services.
//
// As with testServices, the final return value is a function to call at the end
// of your test in order to shut down the test server.
func testRegistrySource(t *testing.T) (source *getproviders.RegistrySource, cleanup func()) {
	services, close := testServices(t)
	source = getproviders.NewRegistrySource(services)
	return source, close
}

func fakeRegistryHandler(resp http.ResponseWriter, req *http.Request) {
	path := req.URL.EscapedPath()

	if !strings.HasPrefix(path, "/providers/v1/") {
		resp.WriteHeader(404)
		resp.Write([]byte(`not a provider registry endpoint`))
		return
	}

	pathParts := strings.Split(path, "/")[3:]

	if len(pathParts) != 2 {
		resp.WriteHeader(404)
		resp.Write([]byte(`unrecognized path scheme`))
		return
	}

	if pathParts[0] != "-" {
		resp.WriteHeader(404)
		resp.Write([]byte(`this registry only supports legacy namespace lookup requests`))
	}

	if namespace, ok := legacyProviderNamespaces[pathParts[1]]; ok {
		resp.Header().Set("Content-Type", "application/json")
		resp.WriteHeader(200)
		resp.Write([]byte(`{"namespace":"` + namespace + `"}`))
	} else {
		resp.WriteHeader(404)
		resp.Write([]byte(`provider not found`))
	}
}
