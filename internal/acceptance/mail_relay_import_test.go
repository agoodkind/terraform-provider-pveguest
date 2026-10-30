package acceptance_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/smtp"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const (
	relayKeyFileVariable    = "PVEGUEST_ACC_SMTP2GO_API_KEY_FILE"
	relayKeyEnvironment     = "PVEGUEST_SMTP2GO_API_KEY"
	relayAPIKeyHeader       = "X-Smtp2go-Api-Key"
	relayRemoveURL          = "https://api.smtp2go.com/v3/users/smtp/remove"
	relayJSONMediaType      = "application/json"
	relayRequestTimeout     = 30 * time.Second
	relaySMTPTimeout        = 30 * time.Second
	relayAddress            = "pveguest_mail_relay.test"
	relayFileName           = "msmtprc"
	relayUserPrefix         = "pveguest-acc-"
	relayUserSuffixBytes    = 4
	relayHost               = "mail.smtp2go.com"
	relayPort               = 587
	relayFrom               = "pveguest-acc@example.net"
	relayFirstVersion       = 1
	relaySecondVersion      = 2
	relayAccountLine        = "account smtp2go"
	relayAccountKeyword     = "account"
	relayLineFields         = 2
	relayLoginMechanism     = "LOGIN"
	relayUsernameChallenge  = "username:"
	relayPasswordChallenge  = "password:"
	relayHostKey            = "host"
	relayPortKey            = "port"
	relayUserKey            = "user"
	relayPasswordKey        = "password"
	relayMissingFieldFormat = "%s does not contain a %s line in the account smtp2go section"
)

// relayCredentials stores the host, port, user name, and password read from the guest file.
type relayCredentials struct {
	host     string
	port     string
	username string
	password string
}

// relayLoginAuth implements smtp.Auth for LOGIN, which net/smtp does not provide.
// It answers the Username: and Password: challenges and requires TLS.
type relayLoginAuth struct {
	username string
	password string
}

func (auth relayLoginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS {
		return "", nil, errors.New("SMTP AUTH LOGIN requires a TLS connection")
	}
	return relayLoginMechanism, nil, nil
}

func (auth relayLoginAuth) Next(challenge []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch strings.ToLower(strings.TrimSpace(string(challenge))) {
	case relayUsernameChallenge:
		return []byte(auth.username), nil
	case relayPasswordChallenge:
		return []byte(auth.password), nil
	default:
		return nil, errors.New("SMTP AUTH LOGIN sent an unexpected challenge")
	}
}

type relayRemoveRequest struct {
	Username string `json:"username"`
}

func (g *testGuest) relayConfig(username string, path string, passwordVersion int) string {
	return g.providerBlock() + fmt.Sprintf(`
resource "pveguest_mail_relay" "test" {
  node             = local.node
  vmid             = local.vmid
  kind             = local.kind
  username         = %q
  host             = %q
  port             = %d
  from             = %q
  path             = %q
  password_version = %d
}
`, username, relayHost, relayPort, relayFrom, path, passwordVersion)
}

func (g *testGuest) relayRemovedConfig() string {
	return g.providerBlock() + `
removed {
  from = pveguest_mail_relay.test
  lifecycle {
    destroy = false
  }
}
`
}

func (g *testGuest) fileHash(path string) string {
	g.t.Helper()
	fields := strings.Fields(g.mustRun("sha256sum", "--", path))
	if len(fields) == 0 {
		g.t.Fatalf("sha256sum printed nothing for %s", path)
	}
	return fields[0]
}

func (g *testGuest) fileInode(path string) string {
	g.t.Helper()
	return strings.TrimSpace(g.mustRun("stat", "-c", "%i", "--", path))
}

// relayCredentials reads host, port, user, and password from the account smtp2go
// section of the guest file. Its errors do not include file values.
func (g *testGuest) relayCredentials(path string) (relayCredentials, error) {
	values := make(map[string]string)
	insideAccount := false
	for line := range strings.Lines(g.readFile(path)) {
		trimmed := strings.TrimSpace(line)
		fields := strings.Fields(trimmed)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == relayAccountKeyword {
			insideAccount = trimmed == relayAccountLine
			continue
		}
		if insideAccount && len(fields) == relayLineFields {
			values[fields[0]] = fields[1]
		}
	}
	for _, key := range []string{relayHostKey, relayPortKey, relayUserKey, relayPasswordKey} {
		if values[key] == "" {
			return relayCredentials{}, fmt.Errorf(relayMissingFieldFormat, path, key)
		}
	}
	return relayCredentials{
		host:     values[relayHostKey],
		port:     values[relayPortKey],
		username: values[relayUserKey],
		password: values[relayPasswordKey],
	}, nil
}

// relayLogin connects, starts TLS, authenticates with LOGIN, and sends QUIT
// without sending a message.
func relayLogin(credentials relayCredentials) error {
	address := net.JoinHostPort(credentials.host, credentials.port)
	connection, err := net.DialTimeout("tcp", address, relaySMTPTimeout)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", address, err)
	}
	if err := connection.SetDeadline(time.Now().Add(relaySMTPTimeout)); err != nil {
		_ = connection.Close()
		return fmt.Errorf("set deadline for %s: %w", address, err)
	}
	client, err := smtp.NewClient(connection, credentials.host)
	if err != nil {
		_ = connection.Close()
		return fmt.Errorf("start SMTP session with %s: %w", address, err)
	}
	defer func() { _ = client.Close() }()
	tlsConfig := &tls.Config{ServerName: credentials.host, MinVersion: tls.VersionTLS12}
	if err := client.StartTLS(tlsConfig); err != nil {
		return fmt.Errorf("STARTTLS with %s: %w", address, err)
	}
	auth := relayLoginAuth{username: credentials.username, password: credentials.password}
	if err := client.Auth(auth); err != nil {
		return fmt.Errorf("SMTP AUTH LOGIN as %s at %s failed", credentials.username, address)
	}
	if err := client.Quit(); err != nil {
		return fmt.Errorf("QUIT at %s: %w", address, err)
	}
	return nil
}

func (g *testGuest) expectRelayLogin(path string) func() error {
	return func() error {
		credentials, err := g.relayCredentials(path)
		if err != nil {
			return err
		}
		return relayLogin(credentials)
	}
}

// checkStateOmitsRelayPassword fails if a state attribute contains the current
// password from the guest file.
func (g *testGuest) checkStateOmitsRelayPassword(path string) resource.TestCheckFunc {
	return func(state *terraform.State) error {
		credentials, err := g.relayCredentials(path)
		if err != nil {
			return err
		}
		return checkStateOmits(credentials.password)(state)
	}
}

func (g *testGuest) checkImportOmitsRelayPassword(path string) resource.ImportStateCheckFunc {
	return func(states []*terraform.InstanceState) error {
		credentials, err := g.relayCredentials(path)
		if err != nil {
			return err
		}
		for _, state := range states {
			for name, value := range state.Attributes {
				if strings.Contains(value, credentials.password) {
					return fmt.Errorf("imported attribute %s contains the mail password", name)
				}
			}
		}
		return nil
	}
}

type relayPasswordPlanCheck struct {
	guest *testGuest
	path  string
}

func (check relayPasswordPlanCheck) CheckPlan(_ context.Context, request plancheck.CheckPlanRequest, response *plancheck.CheckPlanResponse) {
	credentials, err := check.guest.relayCredentials(check.path)
	if err != nil {
		response.Error = err
		return
	}
	document, err := json.Marshal(request.Plan)
	if err != nil {
		response.Error = errors.New("cannot encode the mail relay plan for the password check")
		return
	}
	if bytes.Contains(document, []byte(credentials.password)) {
		response.Error = errors.New("the plan contains the mail password")
	}
}

// invalidRelayImportSteps checks import errors through OpenTofu and real guest files.
func (g *testGuest) invalidRelayImportSteps(config string, importID string, path string) []resource.TestStep {
	invalidPath := path + ".invalid"
	invalidID := importID + ".invalid"
	segments := strings.SplitN(importID, "/", 5)
	username := segments[3]
	cases := []struct {
		oldLine string
		newLine string
		message string
	}{
		{oldLine: "host " + relayHost + "\n", message: "does not contain a host line"},
		{oldLine: "port 587\n", message: "does not contain a port line"},
		{oldLine: "user " + username + "\n", message: "does not contain a user line"},
		{oldLine: "from " + relayFrom + "\n", message: "does not contain a from line"},
		{oldLine: "host " + relayHost, newLine: "host invalid/host", message: "host line.*must contain only"},
		{oldLine: "from " + relayFrom, newLine: "from invalid", message: "from line.*must contain exactly one @"},
		{oldLine: "port 587", newLine: "port invalid", message: "port line.*must contain a number"},
		{oldLine: "host " + relayHost, newLine: "host " + relayHost + "\nhost " + relayHost, message: "must contain exactly one host line"},
	}
	steps := []resource.TestStep{
		{
			Config:        config,
			ResourceName:  relayAddress,
			ImportState:   true,
			ImportStateId: invalidID,
			ExpectError:   regexp.MustCompile(`does not exist`),
		},
	}
	for _, testCase := range cases {
		steps = append(steps, resource.TestStep{
			Config:        config,
			ResourceName:  relayAddress,
			ImportState:   true,
			ImportStateId: invalidID,
			ExpectError:   regexp.MustCompile("(?s)" + testCase.message),
			PreConfig: func() {
				content := g.readFile(path)
				g.mustRun("cp", "--", path, invalidPath)
				g.writeFile(invalidPath, strings.Replace(content, testCase.oldLine, testCase.newLine, 1))
			},
		})
	}
	for _, id := range []string{"invalid", "node/invalid/lxc/user/1", "node/1/invalid/user/1", "node/1/lxc/user/invalid", "node/1/lxc/user/1/"} {
		steps = append(steps, resource.TestStep{
			Config:        config,
			ResourceName:  relayAddress,
			ImportState:   true,
			ImportStateId: id,
			ExpectError:   regexp.MustCompile(`must have the form`),
		})
	}
	segments[3] = username + "-mismatch"
	steps = append(steps, resource.TestStep{
		Config:        config,
		ResourceName:  relayAddress,
		ImportState:   true,
		ImportStateId: strings.Join(segments, "/"),
		ExpectError:   regexp.MustCompile(`(?s)user line.*does not match the import username`),
	})
	return steps
}

func relayAPIKey(t *testing.T) string {
	t.Helper()
	keyFile := requireVariable(t, relayKeyFileVariable)
	keyBytes, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatalf("%s: %v", relayKeyFileVariable, err)
	}
	key := strings.TrimSpace(string(keyBytes))
	if key == "" {
		t.Fatalf("the file in %s is empty", relayKeyFileVariable)
	}
	return key
}

func relayTestUsername(t *testing.T) string {
	t.Helper()
	suffix := make([]byte, relayUserSuffixBytes)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	return relayUserPrefix + hex.EncodeToString(suffix)
}

// removeRelayUser deletes the SMTP2GO test user and reports failures with
// t.Errorf without printing the API key.
func removeRelayUser(t *testing.T, apiKey string, username string) {
	t.Helper()
	document, err := json.Marshal(relayRemoveRequest{Username: username})
	if err != nil {
		t.Errorf("encode SMTP2GO remove request: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), relayRequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, relayRemoveURL, bytes.NewReader(document))
	if err != nil {
		t.Errorf("build SMTP2GO remove request: %v", err)
		return
	}
	request.Header.Set("Content-Type", relayJSONMediaType)
	request.Header.Set("Accept", relayJSONMediaType)
	request.Header.Set(relayAPIKeyHeader, apiKey)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Errorf("SMTP2GO remove request for %s failed: %v", username, err)
		return
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Errorf("SMTP2GO remove request for %s returned HTTP status %d", username, response.StatusCode)
	}
}

func TestAccMailRelayImport(t *testing.T) {
	guest := newTestGuest(t)
	apiKey := relayAPIKey(t)
	t.Setenv(relayKeyEnvironment, apiKey)
	guest.useTestDirectory()
	username := relayTestUsername(t)
	t.Cleanup(func() { removeRelayUser(t, apiKey, username) })
	path := testDirectory + "/" + relayFileName

	firstConfig := guest.relayConfig(username, path, relayFirstVersion)
	secondConfig := guest.relayConfig(username, path, relaySecondVersion)
	importID := strings.Join([]string{
		guest.guest.Node,
		strconv.FormatInt(guest.guest.VMID, 10),
		string(guest.guest.Kind),
		username,
		strconv.Itoa(relayFirstVersion),
	}, "/") + path

	var createdHash string
	var createdInode string
	captureFile := func() error {
		createdHash = guest.fileHash(path)
		createdInode = guest.fileInode(path)
		return nil
	}
	expectCreatedFile := func() error {
		gotHash := guest.fileHash(path)
		gotInode := guest.fileInode(path)
		if gotHash != createdHash || gotInode != createdInode {
			return fmt.Errorf(
				"%s has hash %s and inode %s, want %s and %s",
				path, gotHash, gotInode, createdHash, createdInode,
			)
		}
		return nil
	}
	expectRotatedFile := func() error {
		if guest.fileHash(path) == createdHash {
			return fmt.Errorf("%s still has the hash from before the password_version increase", path)
		}
		return nil
	}
	expectImportedHash := func(states []*terraform.InstanceState) error {
		if len(states) != 1 {
			return fmt.Errorf("import produced %d resources, want 1", len(states))
		}
		if got := states[0].Attributes["sha256"]; got != createdHash {
			return fmt.Errorf("imported sha256 is %s, want %s", got, createdHash)
		}
		return guest.checkImportOmitsRelayPassword(path)(states)
	}

	passwordPlanCheck := relayPasswordPlanCheck{guest: guest, path: path}
	steps := []resource.TestStep{
		{
			Config: firstConfig,
			ConfigPlanChecks: resource.ConfigPlanChecks{
				PostApplyPostRefresh: []plancheck.PlanCheck{passwordPlanCheck},
			},
			Check: resource.ComposeAggregateTestCheckFunc(
				checkGuest(captureFile),
				checkGuest(guest.expectRelayLogin(path)),
				guest.checkStateOmitsRelayPassword(path),
			),
		},
	}
	steps = append(steps, guest.invalidRelayImportSteps(firstConfig, importID, path)...)
	steps = append(steps,
		resource.TestStep{
			Config: guest.relayRemovedConfig(),
			Check:  checkGuest(expectCreatedFile),
		},
		resource.TestStep{
			Config:             firstConfig,
			ResourceName:       relayAddress,
			ImportState:        true,
			ImportStateId:      importID,
			ImportStatePersist: true,
			ImportStateCheck:   expectImportedHash,
		},
		resource.TestStep{
			Config: firstConfig,
			ConfigPlanChecks: resource.ConfigPlanChecks{
				PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan(), passwordPlanCheck},
			},
			Check: resource.ComposeAggregateTestCheckFunc(
				checkGuest(expectCreatedFile),
				checkGuest(guest.expectRelayLogin(path)),
				guest.checkStateOmitsRelayPassword(path),
			),
		},
		resource.TestStep{
			Config: secondConfig,
			ConfigPlanChecks: resource.ConfigPlanChecks{
				PreApply:             []plancheck.PlanCheck{plancheck.ExpectResourceAction(relayAddress, plancheck.ResourceActionUpdate), passwordPlanCheck},
				PostApplyPostRefresh: []plancheck.PlanCheck{passwordPlanCheck},
			},
			Check: resource.ComposeAggregateTestCheckFunc(
				checkGuest(expectRotatedFile),
				checkGuest(guest.expectRelayLogin(path)),
				guest.checkStateOmitsRelayPassword(path),
			),
		},
	)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories,
		CheckDestroy: checkGuest(func() error {
			if guest.exists(path) {
				return fmt.Errorf("%s still exists after destroy", path)
			}
			return nil
		}),
		Steps: steps,
	})
}
