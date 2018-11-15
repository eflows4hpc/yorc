// Copyright 2018 Bull S.A.S. Atos Technologies - Bull, Rue Jean Jaures, B.P.68, 78340, Les Clayes-sous-Bois, France.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package slurm

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/ystia/yorc/deployments"
	"github.com/ystia/yorc/events"
	"github.com/ystia/yorc/helper/stringutil"
	"github.com/ystia/yorc/log"
	"github.com/ystia/yorc/prov"
)

type executionSingularity struct {
	*executionCommon
	singularityInfo *singularityInfo
}

func (e *executionSingularity) executeAsync(ctx context.Context) (*prov.Action, time.Duration, error) {
	// Only runnable operation is currently supported
	log.Debugf("Execute the operation:%+v", e.operation)

	switch strings.ToLower(e.operation.Name) {
	case "tosca.interfaces.node.lifecycle.runnable.run":
		log.Printf("Running the job: %s", e.operation.Name)
		// Build Job Information
		if err := e.buildJobInfo(ctx); err != nil {
			return nil, 0, errors.Wrap(err, "failed to build job information")
		}

		// Build singularity information
		if err := e.buildSingularityInfo(ctx); err != nil {
			return nil, 0, errors.Wrap(err, "failed to build singularity information")
		}

		// Run the command
		err := e.runJobCommand(ctx)
		if err != nil {
			events.WithContextOptionalFields(ctx).NewLogEntry(events.LogLevelERROR, e.deploymentID).RegisterAsString(err.Error())
			return nil, 0, errors.Wrap(err, "failed to run command")
		}
		return e.buildJobMonitoringAction(), e.jobInfo.monitoringTimeInterval, nil
	default:
		return nil, 0, errors.Errorf("Unsupported operation %q", e.operation.Name)
	}
}

func (e *executionSingularity) runJobCommand(ctx context.Context) error {
	opts := e.fillJobCommandOpts()
	e.OperationRemoteExecDir = e.OperationRemoteBaseDir
	if e.jobInfo.batchMode {
		// get outputs for batch mode
		err := e.searchForBatchOutputs(ctx)
		if err != nil {
			return err
		}
		return e.runBatchMode(ctx, opts)
	}

	err := e.runInteractiveMode(ctx, opts)
	if err != nil {
		return err
	}

	// retrieve jobInfo
	return e.retrieveJobID(ctx)
}

func (e *executionSingularity) searchForBatchOutputs(ctx context.Context) error {
	outputs := parseOutputConfigFromOpts(e.jobInfo.opts)
	e.jobInfo.outputs = outputs
	log.Debugf("job outputs:%+v", e.jobInfo.outputs)
	return nil
}

func (e *executionSingularity) runBatchMode(ctx context.Context, opts string) error {
	// Exec args are passed via env var to sbatch script if "key1=value1, key2=value2" format
	var exports string
	for k, v := range e.jobInfo.inputs {
		log.Debugf("Add env var with key:%q and value:%q", k, v)
		export := fmt.Sprintf("export %s=%s;", k, v)
		exports += export
	}
	innerCmd := fmt.Sprintf("%ssrun %s singularity %s %s %s", exports, opts, e.singularityInfo.command, e.singularityInfo.imageURI, e.singularityInfo.exec)
	cmd := fmt.Sprintf("mkdir -p %s;cd %s;sbatch --wrap=\"%s\"", e.OperationRemoteBaseDir, e.OperationRemoteBaseDir, innerCmd)
	events.WithContextOptionalFields(ctx).NewLogEntry(events.LogLevelINFO, e.deploymentID).RegisterAsString(fmt.Sprintf("Run the command: %q", cmd))
	output, err := e.client.RunCommand(cmd)
	if err != nil {
		log.Debugf("stderr:%q", output)
		return errors.Wrap(err, output)
	}
	output = strings.Trim(output, "\n")
	if e.jobInfo.ID, err = parseJobIDFromBatchOutput(output); err != nil {
		return err
	}
	log.Debugf("JobID:%q", e.jobInfo.ID)
	return nil
}

func (e *executionSingularity) runInteractiveMode(ctx context.Context, opts string) error {
	// Add inputs as env variables
	var exports string
	for k, v := range e.jobInfo.inputs {
		log.Debugf("Add env var with key:%q and value:%q", k, v)
		export := fmt.Sprintf("export %s=%s;", k, v)
		exports += export
	}
	redirectFile := stringutil.UniqueTimestampedName("yorc_", "")
	e.jobInfo.outputs = []string{redirectFile}

	cmd := fmt.Sprintf("%ssrun %s singularity %s %s %s %s > %s &", exports, opts, e.singularityInfo.command, strings.Join(e.jobInfo.execArgs, " "), e.singularityInfo.imageURI, e.singularityInfo.exec, redirectFile)
	cmd = strings.Trim(cmd, "")
	events.WithContextOptionalFields(ctx).NewLogEntry(events.LogLevelINFO, e.deploymentID).RegisterAsString(fmt.Sprintf("Run the command: %q", cmd))
	output, err := e.client.RunCommand(cmd)
	if err != nil {
		log.Debugf("stderr:%q", output)
		return errors.Wrap(err, output)
	}
	return nil
}

func (e *executionSingularity) buildSingularityInfo(ctx context.Context) error {
	singularityInfo := singularityInfo{}
	for _, input := range e.EnvInputs {
		if input.Name == "exec_command" && input.Value != "" {
			singularityInfo.exec = input.Value
			singularityInfo.command = "exec"
		}
	}

	singularityInfo.imageName = e.Primary
	if singularityInfo.imageName == "" {
		return errors.New("The image name is mandatory and must be filled in the operation artifact implementation")
	}

	// Default singularity command is "run"
	if singularityInfo.command == "" {
		singularityInfo.command = "run"
	}
	log.Debugf("singularity Info:%+v", singularityInfo)
	e.singularityInfo = &singularityInfo
	return e.resolveContainerImage()
}

func (e *executionSingularity) resolveContainerImage() error {
	switch {
	// Docker image
	case strings.HasPrefix(e.singularityInfo.imageName, "docker://"):
		if err := e.buildImageURI("docker://"); err != nil {
			return err
		}
		// Singularity image
	case strings.HasPrefix(e.singularityInfo.imageName, "shub://"):
		if err := e.buildImageURI("shub://"); err != nil {
			return err
		}
		// File image
	case strings.HasSuffix(e.singularityInfo.imageName, ".simg") || strings.HasSuffix(e.singularityInfo.imageName, ".img"):
		e.singularityInfo.imageURI = e.singularityInfo.imageName
	default:
		return errors.Errorf("Unable to resolve container image URI from image name:%q", e.singularityInfo.imageName)
	}
	return nil
}

func (e *executionSingularity) buildImageURI(prefix string) error {
	repoName, err := deployments.GetOperationImplementationRepository(e.kv, e.deploymentID, e.operation.ImplementedInNodeTemplate, e.NodeType, e.operation.Name)
	if err != nil {
		return err
	}
	if repoName == "" {
		e.singularityInfo.imageURI = e.singularityInfo.imageName
	} else {
		repoURL, err := deployments.GetRepositoryURLFromName(e.kv, e.deploymentID, repoName)
		if err != nil {
			return err
		}
		// Just ignore default public Docker and Singularity registries
		if repoURL == deployments.DockerHubURL || repoURL == deployments.SingularityHubURL {
			e.singularityInfo.imageURI = e.singularityInfo.imageName
		} else if repoURL != "" {
			urlStruct, err := url.Parse(repoURL)
			if err != nil {
				return err
			}
			tabs := strings.Split(e.singularityInfo.imageName, prefix)
			imageURI := prefix + path.Join(urlStruct.Host, tabs[1])
			log.Debugf("imageURI:%q", imageURI)
			e.singularityInfo.imageURI = imageURI
		} else {
			e.singularityInfo.imageURI = e.singularityInfo.imageName
		}
	}
	return nil
}
