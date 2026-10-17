package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/agoodkind/terraform-provider-pveguest/transport"
)

const (
	// stagingDirectoryPrefix is the path prefix for temporary guest directories.
	stagingDirectoryPrefix = "/tmp/pveguest-docker-socket-"

	stagingBodyFile      = "body"
	stagingRandomBytes   = 8
	stagingDirectoryMode = "0700"

	// stagingCommandTimeoutSeconds limits each mkdir and dd exec to 30 seconds.
	stagingCommandTimeoutSeconds = 30

	cleanupTimeoutSeconds = 30

	// curlSecondsPerMiB adds two seconds to the staged curl timeout for each
	// complete MiB of body data. bytesPerMiB converts the body size to MiB.
	curlSecondsPerMiB = 2
	bytesPerMiB       = 1 << 20

	// maximumTimeoutSeconds is the largest command timeout the transport accepts.
	maximumTimeoutSeconds = 3600
)

// handleStaged stages the request body in a guest file, runs curl with that
// file, and relays the response. It defers removal before creating the
// directory.
func (handler *proxy) handleStaged(writer http.ResponseWriter, request *http.Request, body []byte) int {
	directory, err := newStagingDirectory()
	if err != nil {
		return reject(writer, request, http.StatusBadGateway, err.Error())
	}
	defer handler.removeStagingDirectory(request.Context(), directory)

	bodyPath := directory + "/" + stagingBodyFile
	err = handler.stageBody(request.Context(), directory, bodyPath, body)
	if err != nil {
		return reject(writer, request, http.StatusBadGateway, "stage request body: "+err.Error())
	}

	result, err := handler.runGuest(
		request.Context(),
		curlArguments(handler.guestSocket, request, "@"+bodyPath),
		nil,
		stagedCurlTimeoutSeconds(len(body)),
	)
	if err != nil {
		return reject(writer, request, http.StatusBadGateway, err.Error())
	}
	return relay(writer, request, result.Stdout)
}

func newStagingDirectory() (string, error) {
	random := make([]byte, stagingRandomBytes)
	_, err := rand.Read(random)
	if err != nil {
		slog.Error("create staging directory name failed", "err", err)
		return "", fmt.Errorf("create staging directory name: %w", err)
	}
	return stagingDirectoryPrefix + hex.EncodeToString(random), nil
}

// stageBody creates a guest directory with mode 0700 and appends body pieces
// to bodyPath with dd. Each dd exec reads one piece from standard input,
// within the guest exec stdin limit. These commands do not use a shell.
func (handler *proxy) stageBody(ctx context.Context, directory string, bodyPath string, body []byte) error {
	_, err := handler.runGuest(
		ctx, []string{"mkdir", "-m", stagingDirectoryMode, "--", directory}, nil, stagingCommandTimeoutSeconds,
	)
	if err != nil {
		return err
	}

	appendArguments := []string{"dd", "of=" + bodyPath, "oflag=append", "conv=notrunc", "status=none"}
	for _, piece := range transport.SplitStdin(handler.guest.Kind, body) {
		_, err = handler.runGuest(ctx, appendArguments, piece, stagingCommandTimeoutSeconds)
		if err != nil {
			return err
		}
	}
	return nil
}

// removeStagingDirectory runs rm to remove the guest directory and its files,
// even after request cancellation. It logs failures at error level without
// changing the response.
func (handler *proxy) removeStagingDirectory(ctx context.Context, directory string) {
	_, err := handler.runGuest(
		context.WithoutCancel(ctx), []string{"rm", "-r", "-f", "--", directory}, nil, cleanupTimeoutSeconds,
	)
	if err != nil {
		slog.Error("remove staging directory failed", "directory", directory, "err", err)
	}
}

func stagedCurlTimeoutSeconds(bodyBytes int) int {
	timeout := commandTimeoutSeconds + (bodyBytes/bytesPerMiB)*curlSecondsPerMiB
	return min(timeout, maximumTimeoutSeconds)
}
