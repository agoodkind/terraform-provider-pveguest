package transport

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// DefaultPort, DefaultUser, and DefaultMaxSessions apply when the provider
// configuration omits port, user, or max_sessions.
const (
	DefaultPort        = 22
	DefaultUser        = "root"
	DefaultMaxSessions = 4
)

const (
	dialTimeout         = 20 * time.Second
	agentSocketVariable = "SSH_AUTH_SOCK"
)

// NodeConfig is the SSH endpoint of one hypervisor. The SSH agent must offer a
// key that the hypervisor accepts for User.
type NodeConfig struct {
	Host string
	Port int
	User string
}

// Result is the outcome of a command that ran to its end. ExitCode is the
// exit status of the command, and Stdout and Stderr are the bytes that it
// wrote.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Pool owns one SSH client per node and limits the concurrent sessions on
// each client.
type Pool struct {
	maxSessions    int
	knownHostsPath string

	mutex sync.Mutex
	nodes map[string]*nodeConnection
}

type nodeConnection struct {
	config NodeConfig
	slots  chan struct{}

	mutex  sync.Mutex
	client *ssh.Client
}

// NewPool returns a pool for the named hypervisors. The first command for a
// node opens the SSH connection to that node. maxSessions limits the
// concurrent sessions per node and must be at least 1.
func NewPool(nodes map[string]NodeConfig, maxSessions int) (*Pool, error) {
	if maxSessions < 1 {
		return nil, fmt.Errorf("max_sessions must be at least 1, got %d", maxSessions)
	}
	homeDirectory, err := os.UserHomeDir()
	if err != nil {
		slog.Error("find home directory for known_hosts failed", "err", err)
		return nil, fmt.Errorf("find home directory for known_hosts: %w", err)
	}
	pool := &Pool{
		maxSessions:    maxSessions,
		knownHostsPath: filepath.Join(homeDirectory, ".ssh", "known_hosts"),
		nodes:          make(map[string]*nodeConnection, len(nodes)),
	}
	for name, config := range nodes {
		pool.nodes[name] = &nodeConnection{
			config: config,
			slots:  make(chan struct{}, maxSessions),
		}
	}
	return pool, nil
}

// Close closes the SSH client of every node. A later command dials again.
func (pool *Pool) Close() {
	pool.mutex.Lock()
	defer pool.mutex.Unlock()
	for _, connection := range pool.nodes {
		connection.mutex.Lock()
		if connection.client != nil {
			_ = connection.client.Close()
			connection.client = nil
		}
		connection.mutex.Unlock()
	}
}

// runOnNode runs the command line on the hypervisor in a new SSH session.
// A nonzero exit status of the command is returned in Result.ExitCode with a
// nil error.
func (pool *Pool) runOnNode(
	ctx context.Context,
	node string,
	commandLine string,
	stdin []byte,
) (Result, error) {
	pool.mutex.Lock()
	connection, found := pool.nodes[node]
	pool.mutex.Unlock()
	if !found {
		return Result{}, fmt.Errorf("node %q is not in the provider nodes map", node)
	}

	select {
	case connection.slots <- struct{}{}:
	case <-ctx.Done():
		slog.ErrorContext(ctx, "wait for a session slot ended", "node", node, "err", ctx.Err())
		return Result{}, fmt.Errorf("wait for a session slot on node %q: %w", node, ctx.Err())
	}
	defer func() { <-connection.slots }()

	session, err := connection.newSession(ctx, pool.knownHostsPath)
	if err != nil {
		slog.ErrorContext(ctx, "open ssh session failed", "node", node, "err", err)
		return Result{}, fmt.Errorf("ssh to node %q: %w", node, err)
	}
	defer func() { _ = session.Close() }()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	// An empty reader gives the remote command an immediate end of input.
	session.Stdin = bytes.NewReader(stdin)
	session.Stdout = &stdout
	session.Stderr = &stderr

	// A canceled context closes the session, which ends session.Run.
	stopWatching := context.AfterFunc(ctx, func() { _ = session.Close() })
	defer stopWatching()

	runError := session.Run(commandLine)
	result := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if runError == nil {
		return result, nil
	}
	var exitError *ssh.ExitError
	if errors.As(runError, &exitError) {
		result.ExitCode = exitError.ExitStatus()
		return result, nil
	}
	cause := runError
	if ctx.Err() != nil {
		cause = ctx.Err()
	}
	slog.ErrorContext(ctx, "command on node failed", "node", node, "err", cause)
	return Result{}, fmt.Errorf("command on node %q: %w", node, cause)
}

func (connection *nodeConnection) newSession(ctx context.Context, knownHostsPath string) (*ssh.Session, error) {
	connection.mutex.Lock()
	defer connection.mutex.Unlock()

	if connection.client != nil {
		session, err := connection.client.NewSession()
		if err == nil {
			return session, nil
		}
		// The pooled client is unusable, for example after a network drop.
		// One new dial replaces it.
		_ = connection.client.Close()
		connection.client = nil
	}

	client, err := dial(ctx, connection.config, knownHostsPath)
	if err != nil {
		return nil, err
	}
	connection.client = client
	session, err := client.NewSession()
	if err != nil {
		slog.ErrorContext(
			ctx,
			"open ssh session on a new connection failed",
			"host", connection.config.Host, "port", connection.config.Port, "err", err,
		)
		return nil, fmt.Errorf("open session on %s: %w", connection.config.Host, err)
	}
	return session, nil
}

func dial(ctx context.Context, config NodeConfig, knownHostsPath string) (*ssh.Client, error) {
	socketPath := os.Getenv(agentSocketVariable)
	if socketPath == "" {
		return nil, fmt.Errorf("%s is not set; the provider authenticates through the SSH agent", agentSocketVariable)
	}
	dialContext, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	var agentDialer net.Dialer
	agentConnection, err := agentDialer.DialContext(dialContext, "unix", socketPath)
	if err != nil {
		slog.ErrorContext(ctx, "connect to ssh agent failed", "host", config.Host, "err", err)
		return nil, fmt.Errorf("connect to SSH agent: %w", err)
	}
	// The agent signs only during the handshake.
	defer func() { _ = agentConnection.Close() }()

	hostKeyCallback, err := knownhosts.New(knownHostsPath)
	if err != nil {
		slog.Error("read known_hosts failed", "path", knownHostsPath, "err", err)
		return nil, fmt.Errorf("read %s: %w", knownHostsPath, err)
	}
	address := net.JoinHostPort(config.Host, strconv.Itoa(config.Port))
	algorithms, err := knownHostKeyAlgorithms(hostKeyCallback, address)
	if err != nil {
		return nil, err
	}

	clientConfig := &ssh.ClientConfig{
		User:              config.User,
		Auth:              []ssh.AuthMethod{ssh.PublicKeysCallback(agent.NewClient(agentConnection).Signers)},
		HostKeyCallback:   hostKeyCallback,
		HostKeyAlgorithms: algorithms,
		Timeout:           dialTimeout,
	}
	client, err := ssh.Dial("tcp", address, clientConfig)
	if err != nil {
		slog.Error("ssh dial failed", "address", address, "user", config.User, "err", err)
		return nil, fmt.Errorf("dial %s: %w", address, err)
	}
	return client, nil
}

// Limit host-key negotiation to the algorithms recorded in known_hosts.
func knownHostKeyAlgorithms(callback ssh.HostKeyCallback, address string) ([]string, error) {
	probePublicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		slog.Error("generate probe key failed", "address", address, "err", err)
		return nil, fmt.Errorf("generate probe key: %w", err)
	}
	probeKey, err := ssh.NewPublicKey(probePublicKey)
	if err != nil {
		slog.Error("encode probe key failed", "address", address, "err", err)
		return nil, fmt.Errorf("encode probe key: %w", err)
	}

	checkError := callback(address, &net.TCPAddr{}, probeKey)
	var keyError *knownhosts.KeyError
	if !errors.As(checkError, &keyError) {
		slog.Error("look up host in known_hosts failed", "address", address, "err", checkError)
		return nil, fmt.Errorf("look up %s in known_hosts: %w", address, checkError)
	}
	if len(keyError.Want) == 0 {
		return nil, fmt.Errorf("known_hosts does not list host %s", address)
	}

	algorithms := make([]string, 0, len(keyError.Want))
	for _, knownKey := range keyError.Want {
		keyType := knownKey.Key.Type()
		if keyType == ssh.KeyAlgoRSA {
			algorithms = append(algorithms, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256)
		}
		algorithms = append(algorithms, keyType)
	}
	return algorithms, nil
}
