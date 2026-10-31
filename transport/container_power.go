package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ContainerRunning is the status that Proxmox reports for a running container.
const ContainerRunning = "running"

// TaskStopped is the status that Proxmox reports for a task that ended.
const TaskStopped = "stopped"

const (
	statusCurrentOperation = "status/current"
	startOperation         = "status/start"
	shutdownOperation      = "status/shutdown"
	stopOperation          = "status/stop"

	// ShutdownTimeoutSeconds is how long Proxmox waits for a container to shut
	// down before the task fails.
	ShutdownTimeoutSeconds = 60

	taskExitOK   = "OK"
	taskLogLimit = 1000
	taskLogTail  = 10

	// A start or shutdown task ends within the shutdown timeout plus margin.
	taskWaitTimeout = 3 * time.Minute

	taskPathPrefix = "/api2/json/nodes/"
	taskPathSep    = "/tasks/"
)

type containerShutdownBody map[string]int

// TaskStatus is the response of GET /nodes/{node}/tasks/{upid}/status.
// ExitStatus stores exitstatus, which appears only after the task stops.
type TaskStatus struct {
	Status     string `json:"status"`
	ExitStatus string `json:"exitstatus"`
}

type containerStatus struct {
	Status string `json:"status"`
}

type taskLogLine struct {
	Text string `json:"t"`
}

// GetContainerStatus returns the status string of status/current, such as
// running or stopped.
func (client *Client) GetContainerStatus(
	ctx context.Context,
	node string,
	vmid int64,
) (string, error) {
	data, err := client.containerCall(ctx, node, vmid, http.MethodGet, statusCurrentOperation, nil)
	if err != nil {
		return "", err
	}
	var current containerStatus
	if err := json.Unmarshal(data, &current); err != nil {
		slog.ErrorContext(ctx, "container status response is malformed", "node", node, "vmid", vmid, "err", err)
		return "", fmt.Errorf("node %q container %d: parse status response: %w", node, vmid, err)
	}
	return current.Status, nil
}

// StartContainer starts the container and waits for the start task to end
// with exit status OK.
func (client *Client) StartContainer(ctx context.Context, node string, vmid int64) error {
	return client.runContainerTask(ctx, node, vmid, startOperation, nil)
}

// ShutdownContainer shuts the container down with the guest init system and
// waits for the shutdown task. Proxmox fails the task after timeoutSeconds.
func (client *Client) ShutdownContainer(
	ctx context.Context,
	node string,
	vmid int64,
	timeoutSeconds int,
) error {
	body := containerShutdownBody{"timeout": timeoutSeconds}
	document, err := encodeJSON(&body)
	if err != nil {
		slog.ErrorContext(ctx, "encode container shutdown body failed", "node", node, "vmid", vmid, "err", err)
		return fmt.Errorf("node %q container %d: %w", node, vmid, err)
	}
	return client.runContainerTask(ctx, node, vmid, shutdownOperation, document)
}

// StopContainer stops the container at once and waits for the stop task.
func (client *Client) StopContainer(ctx context.Context, node string, vmid int64) error {
	return client.runContainerTask(ctx, node, vmid, stopOperation, nil)
}

func (client *Client) runContainerTask(
	ctx context.Context,
	node string,
	vmid int64,
	operation string,
	body []byte,
) error {
	data, err := client.containerCall(ctx, node, vmid, http.MethodPost, operation, body)
	if err != nil {
		return err
	}
	var upid string
	if err := json.Unmarshal(data, &upid); err != nil {
		slog.ErrorContext(ctx, "container task response is malformed", "node", node, "vmid", vmid, "operation", operation, "err", err)
		return fmt.Errorf("node %q container %d: %s: parse task identifier: %w", node, vmid, operation, err)
	}
	if err := client.WaitTask(ctx, node, upid); err != nil {
		return fmt.Errorf("container %d %s: %w", vmid, operation, err)
	}
	return nil
}

// WaitTask calls WaitTaskWithin with a three-minute limit.
func (client *Client) WaitTask(ctx context.Context, node string, upid string) error {
	return client.WaitTaskWithin(ctx, node, upid, taskWaitTimeout)
}

// WaitTaskWithin polls the task status until the task stops or timeout expires.
// If the exit status is not OK, it returns an error with the log tail. The
// delay between polls starts at 200 ms and doubles up to 2 s.
func (client *Client) WaitTaskWithin(ctx context.Context, node string, upid string, timeout time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	delay := pollInitialDelay
	for {
		current, err := client.GetTaskStatus(waitCtx, node, upid)
		if err != nil {
			return err
		}
		if current.Status == TaskStopped {
			if current.ExitStatus == taskExitOK {
				return nil
			}
			return client.taskFailure(ctx, node, upid, current.ExitStatus)
		}
		select {
		case <-time.After(delay):
		case <-waitCtx.Done():
			slog.ErrorContext(ctx, "wait for task ended", "node", node, "upid", upid, "err", waitCtx.Err())
			return fmt.Errorf("node %q task %s: wait for the task to stop: %w", node, upid, waitCtx.Err())
		}
		delay = min(delay*2, pollMaximumDelay)
	}
}

// GetTaskStatus returns the task status and, after the task stops, its exit
// status.
func (client *Client) GetTaskStatus(ctx context.Context, node string, upid string) (TaskStatus, error) {
	data, err := client.taskCall(ctx, node, upid, "status", nil)
	if err != nil {
		return TaskStatus{}, err
	}
	var current TaskStatus
	if err := json.Unmarshal(data, &current); err != nil {
		slog.ErrorContext(ctx, "task status response is malformed", "node", node, "upid", upid, "err", err)
		return TaskStatus{}, fmt.Errorf("node %q task %s: parse status response: %w", node, upid, err)
	}
	return current, nil
}

// StopTask calls DELETE /nodes/{node}/tasks/{upid}.
// The node sends SIGTERM to the task process group and waits up to 5 seconds.
// If the task still runs, the node sends SIGKILL.
func (client *Client) StopTask(ctx context.Context, node string, upid string) error {
	connection, found := client.nodes[node]
	if !found {
		return fmt.Errorf("node %q is not in the provider nodes map", node)
	}
	guest := Guest{Node: node, Kind: KindLXC}
	_, err := connection.call(ctx, guest, apiRequest{
		method: http.MethodDelete,
		path:   taskPath(connection, upid),
	})
	return err
}

func taskPath(connection *nodeConnection, upid string) string {
	return taskPathPrefix + url.PathEscape(connection.nodeName) + taskPathSep + url.PathEscape(upid)
}

func (client *Client) taskFailure(ctx context.Context, node string, upid string, exitStatus string) error {
	query := url.Values{}
	query.Set("limit", strconv.Itoa(taskLogLimit))
	data, err := client.taskCall(ctx, node, upid, "log", query)
	if err != nil {
		slog.ErrorContext(ctx, "read task log failed", "node", node, "upid", upid, "err", err)
		return fmt.Errorf("node %q task %s ended with exit status %q; reading the task log failed: %w", node, upid, exitStatus, err)
	}
	var lines []taskLogLine
	if err := json.Unmarshal(data, &lines); err != nil {
		slog.ErrorContext(ctx, "task log response is malformed", "node", node, "upid", upid, "err", err)
		return fmt.Errorf("node %q task %s ended with exit status %q; parse task log: %w", node, upid, exitStatus, err)
	}
	if len(lines) > taskLogTail {
		lines = lines[len(lines)-taskLogTail:]
	}
	texts := make([]string, 0, len(lines))
	for _, line := range lines {
		texts = append(texts, line.Text)
	}
	return fmt.Errorf("node %q task %s ended with exit status %q; log tail:\n%s", node, upid, exitStatus, strings.Join(texts, "\n"))
}

func (client *Client) taskCall(
	ctx context.Context,
	node string,
	upid string,
	operation string,
	query url.Values,
) (json.RawMessage, error) {
	connection, found := client.nodes[node]
	if !found {
		return nil, fmt.Errorf("node %q is not in the provider nodes map", node)
	}
	guest := Guest{Node: node, Kind: KindLXC}
	return connection.call(ctx, guest, apiRequest{
		method: http.MethodGet,
		path:   taskPath(connection, upid) + "/" + operation,
		query:  query,
	})
}
