package transport

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
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

const (
	DefaultPort        = 22
	DefaultUser        = "root"
	DefaultMaxSessions = 4

	dialTimeout         = 20 * time.Second
	agentSocketVariable = "SSH_AUTH_SOCK"
)

// NodeConfig is the SSH endpoint of one hypervisor.
type NodeConfig struct {
	Host string
	Port int
	User string
}

// Result is the outcome of one command that ran to completion.
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

// NewPool returns a pool for the given nodes. It opens no connection; the
// first command for a node dials that node.
func NewPool(nodes map[string]NodeConfig, maxSessions int) (*Pool, error) {
	if maxSessions < 1 {
		return nil, fmt.Errorf("max_sessions must be at least 1, got %d", maxSessions)
	}
	homeDirectory, err := os.UserHomeDir()
	if err != nil {
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

// Close closes every open SSH client.
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

// RunOnNode runs one command line on the hypervisor in its own SSH session.
// A command that exits with a nonzero status is a Result, not an error.
func (pool *Pool) RunOnNode(
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
		return Result{}, ctx.Err()
	}
	defer func() { <-connection.slots }()

	session, err := connection.newSession(pool.knownHostsPath)
	if err != nil {
		return Result{}, fmt.Errorf("ssh to node %q: %w", node, err)
	}
	defer func() { _ = session.Close() }()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	// An empty reader gives the remote command an immediate end of input.
	session.Stdin = bytes.NewReader(stdin)
	session.Stdout = &stdout
	session.Stderr = &stderr

	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-ctx.Done():
			_ = session.Close()
		case <-finished:
		}
	}()

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
	if ctx.Err() != nil {
		return Result{}, fmt.Errorf("command on node %q: %w", node, ctx.Err())
	}
	return Result{}, fmt.Errorf("command on node %q: %w", node, runError)
}

func (connection *nodeConnection) newSession(knownHostsPath string) (*ssh.Session, error) {
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

	client, err := dial(connection.config, knownHostsPath)
	if err != nil {
		return nil, err
	}
	connection.client = client
	return client.NewSession()
}

func dial(config NodeConfig, knownHostsPath string) (*ssh.Client, error) {
	socketPath := os.Getenv(agentSocketVariable)
	if socketPath == "" {
		return nil, fmt.Errorf("%s is not set; the provider authenticates through the SSH agent", agentSocketVariable)
	}
	agentConnection, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("connect to SSH agent: %w", err)
	}
	// The agent signs only during the handshake.
	defer func() { _ = agentConnection.Close() }()

	hostKeyCallback, err := knownhosts.New(knownHostsPath)
	if err != nil {
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
		return nil, fmt.Errorf("dial %s: %w", address, err)
	}
	return client, nil
}

// knownHostKeyAlgorithms returns the host key algorithms that known_hosts
// lists for the address. Without this restriction the server may present a
// key type that known_hosts does not list, and the check fails although a
// listed key exists.
func knownHostKeyAlgorithms(callback ssh.HostKeyCallback, address string) ([]string, error) {
	probePublicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate probe key: %w", err)
	}
	probeKey, err := ssh.NewPublicKey(probePublicKey)
	if err != nil {
		return nil, fmt.Errorf("encode probe key: %w", err)
	}

	checkError := callback(address, &net.TCPAddr{}, probeKey)
	var keyError *knownhosts.KeyError
	if !errors.As(checkError, &keyError) {
		return nil, fmt.Errorf("look up %s in known_hosts: %w", address, checkError)
	}
	if len(keyError.Want) == 0 {
		return nil, fmt.Errorf("host %s has no entry in known_hosts", address)
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
