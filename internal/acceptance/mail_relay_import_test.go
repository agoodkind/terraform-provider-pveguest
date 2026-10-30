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
	"io"
	"net"
	"net/http"
	"net/smtp"
	"net/textproto"
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
	relayViewURL            = "https://api.smtp2go.com/v3/users/smtp/view"
	relayJSONMediaType      = "application/json"
	relayRequestTimeout     = 30 * time.Second
	relayResponseLimit      = 1 << 20
	relaySMTPTimeout        = 30 * time.Second
	relayReadyTimeout       = 60 * time.Second
	relayReadyInterval      = 2 * time.Second
	relayRejectedLoginCode  = 535
	relayAuthOperation      = "AUTH LOGIN"
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

var (
	errRelayLoginTLS       = errors.New("SMTP AUTH LOGIN requires a TLS connection")
	errRelayLoginChallenge = errors.New("SMTP AUTH LOGIN sent an unexpected challenge")
)

func (auth relayLoginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS {
		return "", nil, errRelayLoginTLS
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
		return nil, errRelayLoginChallenge
	}
}

type relayRemoveRequest struct {
	Username string `json:"username"`
}

type relayViewUser struct {
	Username      string  `json:"username"`
	Status        *string `json:"status"`
	EmailPassword *string `json:"email_password"`
}

type relayViewData struct {
	Results *[]relayViewUser `json:"results"`
}

type relayViewResponse struct {
	Data relayViewData `json:"data"`
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
	return relayLoginUntil(credentials, time.Now().Add(relaySMTPTimeout))
}

func relayLoginUntil(credentials relayCredentials, deadline time.Time) error {
	address := net.JoinHostPort(credentials.host, credentials.port)
	dialer := net.Dialer{Deadline: deadline}
	connection, err := dialer.Dial("tcp", address)
	if err != nil {
		return relaySMTPFailure("connect", err)
	}
	if err := connection.SetDeadline(deadline); err != nil {
		_ = connection.Close()
		return relaySMTPFailure("set deadline", err)
	}
	client, err := smtp.NewClient(connection, credentials.host)
	if err != nil {
		_ = connection.Close()
		return relaySMTPFailure("greeting", err)
	}
	defer func() { _ = client.Close() }()
	tlsConfig := &tls.Config{ServerName: credentials.host, MinVersion: tls.VersionTLS12}
	if err := client.StartTLS(tlsConfig); err != nil {
		return relaySMTPFailure("STARTTLS", err)
	}
	auth := relayLoginAuth{username: credentials.username, password: credentials.password}
	if err := client.Auth(auth); err != nil {
		return relaySMTPFailure(relayAuthOperation, err)
	}
	if err := client.Quit(); err != nil {
		return relaySMTPFailure("QUIT", err)
	}
	return nil
}

type relaySMTPError struct {
	operation  string
	replyCode  int
	errorClass string
}

func (failure *relaySMTPError) Error() string {
	if failure.replyCode != 0 {
		return fmt.Sprintf("SMTP %s failed with reply code %d", failure.operation, failure.replyCode)
	}
	return fmt.Sprintf("SMTP %s failed with error class %s", failure.operation, failure.errorClass)
}

// relaySMTPFailure excludes server reply text and credential values.
func relaySMTPFailure(operation string, failure error) error {
	var reply *textproto.Error
	if errors.As(failure, &reply) {
		return &relaySMTPError{operation: operation, replyCode: reply.Code}
	}
	if errors.Is(failure, errRelayLoginChallenge) {
		return &relaySMTPError{operation: operation, errorClass: "unexpected-login-challenge"}
	}
	if errors.Is(failure, errRelayLoginTLS) {
		return &relaySMTPError{operation: operation, errorClass: "tls-required"}
	}
	var networkError net.Error
	if errors.As(failure, &networkError) && networkError.Timeout() {
		return &relaySMTPError{operation: operation, errorClass: "timeout"}
	}
	return &relaySMTPError{operation: operation, errorClass: fmt.Sprintf("%T", failure)}
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

// expectRelayReady observes only the generated user after a credential write.
// It retries AUTH reply 535 until one deadline without changing the credentials.
func (g *testGuest) expectRelayReady(path string, apiKey string, username string) func() error {
	return func() error {
		credentials, err := g.relayCredentials(path)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(username, relayUserPrefix) || credentials.username != username {
			return errors.New("SMTP readiness requires the generated test username")
		}
		started := time.Now()
		deadline := started.Add(relayReadyTimeout)
		if err := g.inspectRelayUser(apiKey, credentials, deadline); err != nil {
			return err
		}
		return g.waitRelayLogin(credentials, started, deadline)
	}
}

func (g *testGuest) inspectRelayUser(apiKey string, credentials relayCredentials, deadline time.Time) error {
	content, err := relayUserRequestUntil(apiKey, credentials.username, relayViewURL, deadline)
	if err != nil {
		return fmt.Errorf("inspect the generated SMTP2GO user: %w", err)
	}
	var response relayViewResponse
	if err := json.Unmarshal(content, &response); err != nil {
		return errors.New("cannot decode the generated SMTP2GO user response")
	}
	if response.Data.Results == nil || len(*response.Data.Results) != 1 {
		return errors.New("SMTP2GO must return exactly one generated test user")
	}
	user := (*response.Data.Results)[0]
	if user.Username != credentials.username || user.Status == nil {
		return errors.New("SMTP2GO must return the generated username and its status")
	}
	g.t.Logf("SMTP API status allowed: %t", *user.Status == "allowed")
	g.t.Logf("SMTP API password present: %t", user.EmailPassword != nil)
	if user.EmailPassword != nil {
		g.t.Logf("SMTP API password matches guest: %t", *user.EmailPassword == credentials.password)
	}
	return nil
}

func (g *testGuest) waitRelayLogin(credentials relayCredentials, started time.Time, deadline time.Time) error {
	for attempt := 1; ; attempt++ {
		attemptDeadline := time.Now().Add(relaySMTPTimeout)
		if deadline.Before(attemptDeadline) {
			attemptDeadline = deadline
		}
		err := relayLoginUntil(credentials, attemptDeadline)
		elapsed := time.Since(started).Round(time.Millisecond)
		if err == nil {
			g.t.Logf("SMTP readiness attempt %d succeeded after %s", attempt, elapsed)
			return nil
		}
		g.t.Logf("SMTP readiness attempt %d failed after %s: %v", attempt, elapsed, err)
		var failure *relaySMTPError
		if !errors.As(err, &failure) || failure.operation != relayAuthOperation || failure.replyCode != relayRejectedLoginCode {
			return err
		}
		remaining := time.Until(deadline)
		if remaining > 0 {
			time.Sleep(min(relayReadyInterval, remaining))
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf(
				"SMTP readiness deadline %s expired after %s: %w",
				deadline.UTC().Format(time.RFC3339), time.Since(started).Round(time.Millisecond), err,
			)
		}
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

func relayImportErrorPattern(message string) *regexp.Regexp {
	words := strings.Fields(message)
	for index, word := range words {
		words[index] = regexp.QuoteMeta(word)
	}
	return regexp.MustCompile(strings.Join(words, `\s+`))
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
		{oldLine: "host " + relayHost + "\n", message: "the account smtp2go section does not contain a host line"},
		{oldLine: "port 587\n", message: "the account smtp2go section does not contain a port line"},
		{oldLine: "user " + username + "\n", message: "the account smtp2go section does not contain a user line"},
		{oldLine: "from " + relayFrom + "\n", message: "the account smtp2go section does not contain a from line"},
		{oldLine: "host " + relayHost, newLine: "host invalid/host", message: "the host line in the account smtp2go section must contain only letters, digits, periods, colons, or hyphens"},
		{oldLine: "from " + relayFrom, newLine: "from invalid", message: "the from line in the account smtp2go section must contain exactly one @ with nonempty text on both sides and without whitespace"},
		{oldLine: "port 587", newLine: "port invalid", message: "the port line in the account smtp2go section must contain a number from 1 through 65535"},
		{oldLine: "host " + relayHost, newLine: "host " + relayHost + "\nhost " + relayHost, message: "the account smtp2go section must contain exactly one host line"},
	}
	steps := []resource.TestStep{
		{
			Config:        config,
			ResourceName:  relayAddress,
			ImportState:   true,
			ImportStateId: invalidID,
			ExpectError:   relayImportErrorPattern("the file " + invalidPath + " does not exist."),
		},
	}
	for _, testCase := range cases {
		steps = append(steps, resource.TestStep{
			Config:        config,
			ResourceName:  relayAddress,
			ImportState:   true,
			ImportStateId: invalidID,
			ExpectError:   relayImportErrorPattern(testCase.message),
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
			ExpectError:   relayImportErrorPattern("must have the form <node>/<vmid>/<kind>/<username>/<password_version>, optionally followed by an absolute file path such as /etc/msmtprc"),
		})
	}
	segments[3] = username + "-mismatch"
	steps = append(steps, resource.TestStep{
		Config:        config,
		ResourceName:  relayAddress,
		ImportState:   true,
		ImportStateId: strings.Join(segments, "/"),
		ExpectError:   relayImportErrorPattern("the user line in the account smtp2go section does not match the import username " + segments[3]),
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

func relayUserRequest(apiKey string, username string, endpoint string) ([]byte, error) {
	return relayUserRequestUntil(apiKey, username, endpoint, time.Now().Add(relayRequestTimeout))
}

func relayUserRequestUntil(apiKey string, username string, endpoint string, deadline time.Time) ([]byte, error) {
	document, err := json.Marshal(relayRemoveRequest{Username: username})
	if err != nil {
		return nil, errors.New("cannot encode the SMTP2GO user request")
	}
	requestDeadline := time.Now().Add(relayRequestTimeout)
	if deadline.Before(requestDeadline) {
		requestDeadline = deadline
	}
	ctx, cancel := context.WithDeadline(context.Background(), requestDeadline)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(document))
	if err != nil {
		return nil, fmt.Errorf("build SMTP2GO user request: %w", err)
	}
	request.Header.Set("Content-Type", relayJSONMediaType)
	request.Header.Set("Accept", relayJSONMediaType)
	request.Header.Set(relayAPIKeyHeader, apiKey)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("SMTP2GO user request failed: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("SMTP2GO user request returned HTTP status %d", response.StatusCode)
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, relayResponseLimit))
	if err != nil {
		return nil, fmt.Errorf("read SMTP2GO user response: %w", err)
	}
	return content, nil
}

// removeRelayUser deletes the test user and requires an empty view result.
// Its errors omit response bodies because SMTP2GO can return credentials.
func removeRelayUser(t *testing.T, apiKey string, username string) {
	t.Helper()
	if _, err := relayUserRequest(apiKey, username, relayRemoveURL); err != nil {
		t.Errorf("remove SMTP2GO user %s: %v", username, err)
		return
	}
	content, err := relayUserRequest(apiKey, username, relayViewURL)
	if err != nil {
		t.Errorf("verify removal of SMTP2GO user %s: %v", username, err)
		return
	}
	var response relayViewResponse
	if err := json.Unmarshal(content, &response); err != nil {
		t.Errorf("cannot decode the removal verification response for SMTP2GO user %s", username)
		return
	}
	if response.Data.Results == nil {
		t.Errorf("the removal verification response for SMTP2GO user %s does not contain data.results", username)
		return
	}
	if len(*response.Data.Results) != 0 {
		t.Errorf("SMTP2GO still lists user %s after removal", username)
	}
}

func TestAccMailRelayImport(t *testing.T) {
	guest := newTestGuest(t)
	apiKey := relayAPIKey(t)
	t.Setenv(relayKeyEnvironment, apiKey)
	guest.useTestDirectory()
	username := relayTestUsername(t)
	t.Logf("SMTP2GO test username: %s", username)
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
				checkGuest(guest.expectRelayReady(path, apiKey, username)),
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
				checkGuest(guest.expectRelayReady(path, apiKey, username)),
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
