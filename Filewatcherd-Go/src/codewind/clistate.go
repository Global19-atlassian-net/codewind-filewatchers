/*******************************************************************************
* Copyright (c) 2019, 2020 IBM Corporation and others.
* All rights reserved. This program and the accompanying materials
* are made available under the terms of the Eclipse Public License v2.0
* which accompanies this distribution, and is available at
* http://www.eclipse.org/legal/epl-v20.html
*
* Contributors:
*     IBM Corporation - initial API and implementation
*******************************************************************************/

package main

import (
	"codewind/models"
	"codewind/utils"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// CLIState will call the cwctl project sync command, in order to allow the
// Codewind CLI to detect and communicate file changes to the server.
//
// This class will ensure that only one instance of the cwctl project sync command is running
// at a time, per project.
//
// For automated testing, if the `MOCK_CWCTL_INSTALLER_PATH` environment variable is specified, a mock cwctl command
// written in Java (as a runnable JAR) can be used to test this class.
type CLIState struct {
	projectID string

	installerPath string

	projectPath string

	/** For automated testing only */
	mockInstallerPath string

	channel chan CLIStateChannelEntry
}

// NewCLIState contains the state of the CLI project sync commmand for a single project (id+path)
func NewCLIState(projectIDParam string, installerPathParam string, projectPathParam string) (*CLIState, error) {

	if installerPathParam == "" {
		// This object should not be instantiated if the installerPath is empty.
		return nil, errors.New("Installer path is empty: " + installerPathParam)
	}

	result := &CLIState{
		projectID:         projectIDParam,
		installerPath:     installerPathParam,
		projectPath:       projectPathParam,
		mockInstallerPath: strings.TrimSpace(os.Getenv("MOCK_CWCTL_INSTALLER_PATH")),
		channel:           make(chan CLIStateChannelEntry),
	}

	go result.readChannel()

	return result, nil

}

// OnFileChangeEvent is called by eventbatchutil and projectlist.
// This method is defacto non-blocking: it will pass the file notification to the go channel (which should be read immediately)
// then immediately return.
func (state *CLIState) OnFileChangeEvent(projectCreationTimeInAbsoluteMsecsParam int64, debugPtw *models.ProjectToWatch) error {

	if strings.TrimSpace(state.projectPath) == "" {
		msg := "Project path passed to CLIState is empty, so ignoring file change event."
		utils.LogSevere(msg)
		return errors.New(msg)
	}

	// Inform channel that a new file change list was received (but don't actually send it)
	state.channel <- CLIStateChannelEntry{projectCreationTimeInAbsoluteMsecsParam, nil, debugPtw}

	return nil
}

func (state *CLIState) readChannel() {
	processWaiting := false // Once the current command completes, should we start another one
	processActive := false  // Is there currently a cwctl command active.

	var lastTimestamp int64 = 0

	debugMostRecentPtw := (*models.ProjectToWatch)(nil) // Only used during automated testing

	for {

		channelResult := <-state.channel

		if channelResult.runProjectReturn != nil {
			// Event: Previous run of cwctl command has completed
			processActive = false

			rpr := channelResult.runProjectReturn

			if rpr.errorCode == 0 {
				// Success, so update the timestamp to the process start time.
				lastTimestamp = rpr.spawnTime
				utils.LogInfo("Updating timestamp to latest: " + strconv.FormatInt(lastTimestamp, 10))

			} else {
				utils.LogSevere("Non-zero error code from installer: " + rpr.output)
			}

		} else {
			// Event: Another thread has informed us of new file changes
			if channelResult.projectCreationTimeInAbsoluteMsecsParam != 0 && lastTimestamp == 0 {
				utils.LogInfo("Timestamp updated from " + timestampToString(lastTimestamp) + " to " + timestampToString(channelResult.projectCreationTimeInAbsoluteMsecsParam) + " from project creation time.")
				lastTimestamp = channelResult.projectCreationTimeInAbsoluteMsecsParam
			}

			if channelResult.debugPtw != nil {
				debugMostRecentPtw = channelResult.debugPtw
			}

			processWaiting = true
		}

		if !processActive && processWaiting {
			// Start a new process if there isn't one running, and we received an update event.
			processWaiting = false
			processActive = true
			go state.runProjectCommand(lastTimestamp, debugMostRecentPtw)
		}
	}

}

// CLIStateChannelEntry runprojectReturn will be non-null if it is a runProjectCommand response, otherwise null if it is a new file change. */
type CLIStateChannelEntry struct {
	projectCreationTimeInAbsoluteMsecsParam int64
	runProjectReturn                        *RunProjectReturn
	debugPtw                                *models.ProjectToWatch // Only used during automated testing
}

func (state *CLIState) runProjectCommand(timestamp int64, debugPtw *models.ProjectToWatch) {

	firstArg := ""

	currInstallPath := state.installerPath

	var args []string

	lastTimestamp := timestamp

	if state.mockInstallerPath == "" {

		// Normal call to `cwctl project sync`

		firstArg = state.installerPath
		// Example:
		// cwctl project sync -p
		// /Users/tobes/workspaces/git/eclipse/codewind/codewind-workspace/lib5 \
		// -i b1a78500-eaa5-11e9-b0c1-97c28a7e77c7 -t 1571944337

		// Do not wrap paths in quotes; it's not needed and Go doesn't like that :P

		args = append(args, "project", "sync", "-p", state.projectPath, "-i", state.projectID, "-t",
			strconv.FormatInt(lastTimestamp, 10))

	} else {

		// The filewatcher is being run in an automated test scenario: we will now run a
		// mock version of cwctl that simulates the project sync command. This mock
		// version takes slightly different parameters.

		firstArg = "java"

		// Convert filesToWatch to absolute paths
		convertedFilesToWatch := []string{}
		for _, fileToWatch := range (*debugPtw).RefPaths {

			val, err := utils.ConvertAbsoluteUnixStyleNormalizedPathToLocalFile(fileToWatch.From)
			if err != nil {
				utils.LogErrorErr("Unable to convert file path: "+fileToWatch.From, err)
				continue
			}
			convertedFilesToWatch = append(convertedFilesToWatch, val)

		}
		simplifiedPtwObj := DebugSimplifiedPtw{
			FilesToWatch:     convertedFilesToWatch,
			IgnoredFilenames: (*debugPtw).IgnoredFilenames,
			IgnoredPaths:     (*debugPtw).IgnoredPaths,
		}

		simplifiedPtw, err := json.Marshal(simplifiedPtwObj)
		if err != nil {
			utils.LogSevereErr("Unable to marshal JSON", err)
			simplifiedPtw = []byte("{}")
		}

		base64Conversion := base64.StdEncoding.EncodeToString(simplifiedPtw)

		args = append(args, "-jar", state.mockInstallerPath, "-p", state.projectPath, "-i",
			state.projectID, "-t", strconv.FormatInt(lastTimestamp, 10), "-projectJson", base64Conversion)

		currInstallPath = state.mockInstallerPath
	}

	debugStr := ""

	for _, key := range args {

		debugStr += "[ " + key + "] "
	}

	utils.LogInfo("Calling cwctl project sync with: [" + state.projectID + "] { " + debugStr + "}")

	// Start process and wait for complete on this thread.

	installerPwd := filepath.Dir(currInstallPath)

	spawnTimeInMsecs := (time.Now().UnixNano() / int64(time.Millisecond))

	cmd := exec.Command(firstArg, args...)
	cmd.Dir = installerPwd

	stdoutStderr, err := cmd.CombinedOutput()

	utils.LogInfo("Cwctl call completed, elapsed time of cwctl call: " + strconv.FormatInt((time.Now().UnixNano()/int64(time.Millisecond))-spawnTimeInMsecs, 10))

	if err != nil {

		errorCode := -1

		one, castable := err.(*exec.ExitError)

		if castable {
			errorCode = one.ExitCode()
		}

		utils.LogError("Error running 'project sync' installer command: " + debugStr)
		utils.LogError("Out: " + string(stdoutStderr))

		result := RunProjectReturn{
			errorCode,
			string(stdoutStderr),
			spawnTimeInMsecs,
		}

		state.channel <- CLIStateChannelEntry{0, &result, nil}

	} else {

		utils.LogInfo("Successfully ran installer command: " + debugStr)
		utils.LogInfo("Output:" + string(stdoutStderr)) // TODO: Convert to DEBUG once everything matures.

		result := RunProjectReturn{
			0,
			string(stdoutStderr),
			spawnTimeInMsecs,
		}

		state.channel <- CLIStateChannelEntry{0, &result, nil}

	}
}

// RunProjectReturn contains the return value of runProjectCommand()
type RunProjectReturn struct {
	errorCode int
	output    string
	spawnTime int64
}

// DebugSimplifiedPtw is only used during automated testing.
type DebugSimplifiedPtw struct {
	FilesToWatch     []string `json:"filesToWatch"`
	IgnoredFilenames []string `json:"ignoredFilenames"`
	IgnoredPaths     []string `json:"ignoredPaths"`
}
