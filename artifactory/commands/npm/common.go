package npm

import (
	"bufio"
	"github.com/jfrog/build-info-go/build"
	biutils "github.com/jfrog/build-info-go/utils"
	"github.com/jfrog/gofrog/version"
	commandUtils "github.com/jfrog/jfrog-cli-core/v2/artifactory/commands/utils"
	"github.com/jfrog/jfrog-cli-core/v2/artifactory/utils"
	"github.com/jfrog/jfrog-cli-core/v2/artifactory/utils/npm"
	"github.com/jfrog/jfrog-cli-core/v2/utils/coreutils"
	"github.com/jfrog/jfrog-client-go/auth"
	"github.com/jfrog/jfrog-client-go/utils/errorutils"
	"github.com/jfrog/jfrog-client-go/utils/log"
	"io/ioutil"
	"path/filepath"
	"strconv"
	"strings"
)

type CommonArgs struct {
	cmdName        string
	jsonOutput     bool
	executablePath string
	// Function to be called to restore the user's old npmrc and delete the one we created.
	restoreNpmrcFunc func() error
	workingDirectory string
	// Npm registry as exposed by Artifactory.
	registry string
	// Npm token generated by Artifactory using the user's provided credentials.
	npmAuth          string
	collectBuildInfo bool
	buildInfoModule  *build.NpmModule
	typeRestriction  biutils.TypeRestriction
	authArtDetails   auth.ServiceDetails
	npmVersion       *version.Version
	NpmCommand
}

func (com *CommonArgs) preparePrerequisites(repo string) error {
	log.Debug("Preparing prerequisites...")
	var err error
	com.npmVersion, com.executablePath, err = biutils.GetNpmVersionAndExecPath(log.Logger)
	if err != nil {
		return err
	}

	if com.npmVersion.Compare(minSupportedNpmVersion) > 0 {
		return errorutils.CheckErrorf(
			"JFrog CLI npm %s command requires npm client version "+minSupportedNpmVersion+" or higher. The Current version is: %s", com.cmdName, com.npmVersion.GetVersion())
	}

	if err := com.setJsonOutput(); err != nil {
		return err
	}

	com.workingDirectory, err = coreutils.GetWorkingDirectory()
	if err != nil {
		return err
	}
	log.Debug("Working directory set to:", com.workingDirectory)

	if err = com.setArtifactoryAuth(); err != nil {
		return err
	}

	com.npmAuth, com.registry, err = commandUtils.GetArtifactoryNpmRepoDetails(repo, &com.authArtDetails)
	if err != nil {
		return err
	}

	com.collectBuildInfo, err = com.buildConfiguration.IsCollectBuildInfo()
	if err != nil {
		return err
	}

	if com.collectBuildInfo {
		buildName, err := com.buildConfiguration.GetBuildName()
		if err != nil {
			return err
		}
		buildNumber, err := com.buildConfiguration.GetBuildNumber()
		if err != nil {
			return err
		}
		buildInfoService := utils.CreateBuildInfoService()
		npmBuild, err := buildInfoService.GetOrCreateBuildWithProject(buildName, buildNumber, com.buildConfiguration.GetProject())
		if err != nil {
			return errorutils.CheckError(err)
		}
		com.buildInfoModule, err = npmBuild.AddNpmModule(com.workingDirectory)
		if err != nil {
			return errorutils.CheckError(err)
		}
	}

	com.restoreNpmrcFunc, err = commandUtils.BackupFile(filepath.Join(com.workingDirectory, npmrcFileName), filepath.Join(com.workingDirectory, npmrcBackupFileName))
	return err
}

func (com *CommonArgs) setJsonOutput() error {
	jsonOutput, err := npm.ConfigGet(com.npmArgs, "json", com.executablePath)
	if err != nil {
		return err
	}

	// In case of --json=<not boolean>, the value of json is set to 'true', but the result from the command is not 'true'
	com.jsonOutput = jsonOutput != "false"
	return nil
}

func (com *CommonArgs) setArtifactoryAuth() error {
	authArtDetails, err := com.serverDetails.CreateArtAuthConfig()
	if err != nil {
		return err
	}
	if authArtDetails.GetSshAuthHeaders() != nil {
		return errorutils.CheckErrorf("SSH authentication is not supported in this command")
	}
	com.authArtDetails = authArtDetails
	return nil
}

// In order to make sure the npm resolves artifacts from Artifactory we create a .npmrc file in the project dir.
// If such a file exists we back it up as npmrcBackupFileName.
func (com *CommonArgs) createTempNpmrc() error {
	log.Debug("Creating project .npmrc file.")
	data, err := npm.GetConfigList(com.npmArgs, com.executablePath)
	configData, err := com.prepareConfigData(data)
	if err != nil {
		return errorutils.CheckError(err)
	}

	if err = removeNpmrcIfExists(com.workingDirectory); err != nil {
		return err
	}

	return errorutils.CheckError(ioutil.WriteFile(filepath.Join(com.workingDirectory, npmrcFileName), configData, 0600))
}

func (com *CommonArgs) setTypeRestriction(key string, value string) {
	// From npm 7, type restriction is determined by 'omit' and 'include' (both appear in 'npm config ls').
	// Other options (like 'dev', 'production' and 'only') are deprecated, but if they're used anyway - 'omit' and 'include' are automatically calculated.
	// So 'omit' is always preferred, if it exists.
	if key == "omit" {
		if strings.Contains(value, "dev") {
			com.typeRestriction = biutils.ProdOnly
		} else {
			com.typeRestriction = biutils.All
		}
	} else if com.typeRestriction == biutils.DefaultRestriction { // Until npm 6, configurations in 'npm config ls' are sorted by priority in descending order, so typeRestriction should be set only if it was not set before
		if key == "only" {
			if strings.Contains(value, "prod") {
				com.typeRestriction = biutils.ProdOnly
			} else if strings.Contains(value, "dev") {
				com.typeRestriction = biutils.DevOnly
			}
		} else if key == "production" && strings.Contains(value, "true") {
			com.typeRestriction = biutils.ProdOnly
		}
	}
}

func (com *CommonArgs) restoreNpmrcAndError(err error) error {
	if restoreErr := com.restoreNpmrcFunc(); restoreErr != nil {
		return errorutils.CheckErrorf("Two errors occurred:\n %s\n %s", restoreErr.Error(), err.Error())
	}
	return err
}

// This func transforms "npm config list" result to key=val list of values that can be set to .npmrc file.
// it filters out any nil value key, changes registry and scope registries to Artifactory url and adds Artifactory authentication to the list
func (com *CommonArgs) prepareConfigData(data []byte) ([]byte, error) {
	var filteredConf []string
	configString := string(data)
	scanner := bufio.NewScanner(strings.NewReader(configString))

	for scanner.Scan() {
		currOption := scanner.Text()
		if currOption != "" {
			splitOption := strings.SplitN(currOption, "=", 2)
			key := strings.TrimSpace(splitOption[0])
			if len(splitOption) == 2 && isValidKey(key) {
				value := strings.TrimSpace(splitOption[1])
				if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
					filteredConf = addArrayConfigs(filteredConf, key, value)
				} else {
					filteredConf = append(filteredConf, currOption, "\n")
				}
				com.setTypeRestriction(key, value)
			} else if strings.HasPrefix(splitOption[0], "@") {
				// Override scoped registries (@scope = xyz)
				filteredConf = append(filteredConf, splitOption[0], " = ", com.registry, "\n")
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, errorutils.CheckError(err)
	}

	filteredConf = append(filteredConf, "json = ", strconv.FormatBool(com.jsonOutput), "\n")
	filteredConf = append(filteredConf, "registry = ", com.registry, "\n")
	filteredConf = append(filteredConf, com.npmAuth)
	return []byte(strings.Join(filteredConf, "")), nil
}